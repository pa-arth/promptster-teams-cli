package capture

import (
	"database/sql"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// claudeRejectRow is the observed shape (2026-09-28, Claude Code 2.1.283).
func claudeRejectRow(ts string, resetsAt int64, status string, overage bool) string {
	ov := "false"
	if overage {
		ov = "true"
	}
	return `{"type":"assistant","timestamp":"` + ts + `","isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,` +
		`"message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your session limit"}]},` +
		`"quotaLimits":{"status":"` + status + `","resetsAt":` + strconv.FormatInt(resetsAt, 10) + `,"rateLimitType":"five_hour","overageStatus":"rejected",` +
		`"overageDisabledReason":"out_of_credits","upgradePaths":["upgrade_plan"],"isUsingOverage":` + ov + `}}`
}

// The committed synthetic twin of the real-corpus replay: 3 windows across 4
// files, repeated rejections, plus every row that must NOT count.
func TestScanClaudeLimitHitsSynthetic(t *testing.T) {
	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "proj-a", "s1.jsonl"),
		`{"type":"user","timestamp":"2026-09-28T07:00:00Z","message":{"content":"rate_limit quotaLimits"}}`,
		claudeRejectRow("2026-09-28T07:39:31.320Z", 1790587800, "rejected", false),
		claudeRejectRow("2026-09-28T07:39:40.154Z", 1790587800, "rejected", false),
	)
	writeLines(t, filepath.Join(dir, "proj-a", "s2.jsonl"),
		claudeRejectRow("2026-09-28T07:52:37.913Z", 1790587800, "rejected", false),
		claudeRejectRow("2026-09-28T19:22:19.904Z", 1790623800, "rejected", false),
		// Overage is not a hit.
		claudeRejectRow("2026-09-28T23:00:00Z", 1790640000, "rejected", true),
		// Allowed is not a hit.
		claudeRejectRow("2026-09-28T23:10:00Z", 1790650000, "allowed", false),
	)
	writeLines(t, filepath.Join(dir, "proj-b", "s3", "subagents", "agent-1.jsonl"),
		claudeRejectRow("2026-09-28T19:28:57.322Z", 1790623800, "rejected", false),
		// Not a rate_limit error row, though it carries quotaLimits and the
		// "rate_limit" string (so only the error check can exclude it).
		strings.Replace(claudeRejectRow("2026-09-29T10:00:00Z", 1790700000, "rejected", false), `"error":"rate_limit"`, `"error":"overloaded","note":"rate_limit"`, 1),
	)
	writeLines(t, filepath.Join(dir, "proj-b", "s4.jsonl"),
		claudeRejectRow("2026-09-29T19:53:58.365Z", 1790714400, "rejected", false),
	)

	hits := scanClaudeLimitHits(dir, time.Time{})
	want := []int64{1790587800, 1790623800, 1790714400}
	if len(hits) != len(want) {
		t.Fatalf("got %d hits, want %d: %+v", len(hits), len(want), hits)
	}
	for i, h := range hits {
		if h.ResetsAt != want[i] || h.Window != "five_hour" || h.Evidence != evidenceRejectedRequest || h.OverageStatus != "rejected" {
			t.Errorf("hit %d = %+v", i, h)
		}
	}
	if first := time.Unix(hits[0].FirstSeenAt, 0).UTC().Format(time.RFC3339); first != "2026-09-28T07:39:31Z" {
		t.Errorf("firstSeenAt = %s, want the earliest rejected row", first)
	}
}

func codexTokenCount(ts string, pct float64, resetsAt int64, limitID string) string {
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"` + limitID + `",` +
		`"primary":{"used_percent":` + strconv.FormatFloat(pct, 'f', 1, 64) + `,"window_minutes":10080,"resets_at":` + strconv.FormatInt(resetsAt, 10) + `},"secondary":null,` +
		`"plan_type":"prolite","credits":{"has_credits":false,"unlimited":false,"balance":"0"},"rate_limit_reached_type":null}}}`
}

func TestScanCodexLimitHitsSynthetic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026", "09", "29", "rollout-2026-09-29T16-00-00-019eb780-3081-7ce0-9ba0-8a0bad13b532.jsonl")
	writeLines(t, path,
		codexTokenCount("2026-09-29T16:30:00.000Z", 98, 1791053686, "codex"),
		codexTokenCount("2026-09-29T16:33:23.406Z", 100, 1791053686, "codex"),
		codexTokenCount("2026-09-29T16:34:35.809Z", 100, 1791053686, "codex"),
		`{"timestamp":"2026-09-29T16:33:25.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"premium","primary":null,"secondary":null,"plan_type":null}}}`,
		// A later, lower reading in the SAME scan: the window reset early.
		codexTokenCount("2026-09-29T18:41:00.000Z", 0, 1791300000, "codex"),
	)
	hits := scanCodexLimitHits(dir, time.Time{})
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(hits), hits)
	}
	h := hits[0]
	if h.Window != "weekly" || h.ResetsAt != 1791053686 || h.Evidence != evidenceWindowFull || h.SessionID != "019eb780-3081-7ce0-9ba0-8a0bad13b532" {
		t.Errorf("hit = %+v", h)
	}
	if got := time.Unix(h.FirstSeenAt, 0).UTC().Format(time.RFC3339); got != "2026-09-29T16:33:23Z" {
		t.Errorf("firstSeenAt = %s", got)
	}
}

func TestCodexWindowName(t *testing.T) {
	for m, want := range map[float64]string{300: "five_hour", 10080: "weekly", 43800: "other_43800"} {
		if got := codexWindowName(m); got != want {
			t.Errorf("codexWindowName(%v) = %q, want %q", m, got, want)
		}
	}
}

// Real-corpus replay (design §8.1). Opt-in: it reads THIS machine's transcripts
// and rollouts, which CI does not have. Prints counts and reset times only.
//
//	PROMPTSTER_TEST_REAL_LIMIT_HITS=1 go test ./internal/capture -run TestRealCorpusLimitHitReplay -v
func TestRealCorpusLimitHitReplay(t *testing.T) {
	if os.Getenv("PROMPTSTER_TEST_REAL_LIMIT_HITS") == "" {
		t.Skip("set PROMPTSTER_TEST_REAL_LIMIT_HITS=1 to replay this machine's Claude + Codex corpus")
	}
	claude := scanClaudeLimitHits(ClaudeProjectsDir(), time.Time{})
	var claudeResets []int64
	for _, h := range claude {
		claudeResets = append(claudeResets, h.ResetsAt)
		if h.Window != "five_hour" || h.Evidence != evidenceRejectedRequest {
			t.Errorf("claude hit %+v", h)
		}
	}
	// 2026-09-28T09:30Z, 2026-09-28T19:30Z, 2026-09-29T20:40Z.
	wantClaude := []int64{1790587800, 1790623800, 1790714400}
	if len(claude) != 3 || claudeResets[0] != wantClaude[0] || claudeResets[1] != wantClaude[1] || claudeResets[2] != wantClaude[2] {
		t.Errorf("claude: got %d hits resetsAt=%v, want 3 at %v", len(claude), claudeResets, wantClaude)
	}

	codex := scanCodexLimitHits(codexSessionsDir(), time.Time{})
	if len(codex) != 1 {
		t.Fatalf("codex: got %d hits, want 1: %+v", len(codex), codex)
	}
	if h := codex[0]; h.Window != "weekly" || h.ResetsAt != 1791053686 ||
		time.Unix(h.FirstSeenAt, 0).UTC().Format("2006-01-02T15:04") != "2026-09-29T16:33" {
		t.Errorf("codex hit %+v", h)
	}
	t.Logf("claude=%d resetsAt=%v codex=%d resetsAt=%d", len(claude), claudeResets, len(codex), codex[0].ResetsAt)

	// Tier readers against the real files. Only the projected, pattern-checked
	// vendor strings are logged.
	logTier := func(r planTierReading) {
		t.Logf("tier %s: plan=%q rateLimitTier=%q source=%s observed=%s state=%q",
			r.Provider, r.VendorPlan, r.VendorRateLimitTier, r.TierSource,
			time.Unix(r.SourceObservedAt, 0).UTC().Format(time.RFC3339), r.SignalState)
	}
	logTier(readClaudePlanTier(claudeProfilePath()))
	logTier(readCodexPlanTier(codexSessionsDir(), filepath.Join(codexHome(), "auth.json"), time.Now()))
	if r, ok := readCursorPlanTier(cursorPlanStateDBCandidates()); ok {
		logTier(r)
	} else {
		t.Log("tier cursor: no state.vscdb found — nothing emitted")
	}
}

// --- tier readers ------------------------------------------------------------

func setupCaptureState(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	if _, err := sign.GenerateSessionKeypair(); err != nil {
		t.Fatal(err)
	}
	return tmp
}

const plantedEmail = "planted.user@example.com"
const plantedUUID = "5f0c1e2a-9b8d-4c3e-a1f2-0123456789ab"

func TestReadClaudePlanTier(t *testing.T) {
	setupCaptureState(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	writeLines(t, path, `{"numStartups":3,"oauthAccount":{"accountUuid":"`+plantedUUID+`","emailAddress":"`+plantedEmail+`",`+
		`"displayName":"Planted Name","organizationName":"Planted Org","organizationUuid":"`+plantedUUID+`",`+
		`"billingType":"stripe_subscription","organizationType":"claude_max","organizationRateLimitTier":"default_claude_max_5x",`+
		`"userRateLimitTier":null,"seatTier":null,"hasExtraUsageEnabled":true,"profileFetchedAt":1791347467270}}`)
	r := readClaudePlanTier(path)
	if r.VendorPlan != "claude_max" || r.VendorRateLimitTier != "default_claude_max_5x" || r.BillingType != "stripe_subscription" ||
		r.SeatTier != "" || r.OverageEnabled == nil || !*r.OverageEnabled || r.SourceObservedAt != 1791347467 ||
		r.TierSource != tierSourceClaudeProfile || r.SignalState != "" || len(r.AccountRef) != 16 {
		t.Errorf("reading = %+v", r)
	}

	// No oauthAccount (API-key user) and no file: an observed absence, never "free".
	writeLines(t, path, `{"numStartups":3,"primaryApiKey":"sk-ant-planted"}`)
	if r := readClaudePlanTier(path); r.SignalState != planSignalSourceAbsent || r.VendorPlan != "" {
		t.Errorf("no oauthAccount: %+v", r)
	}
	if r := readClaudePlanTier(filepath.Join(dir, "missing.json")); r.SignalState != planSignalSourceAbsent {
		t.Errorf("missing file: %+v", r)
	}
	writeLines(t, path, `{"oauthAccount":`)
	if r := readClaudePlanTier(path); r.SignalState != planSignalUnreadable {
		t.Errorf("corrupt file: %+v", r)
	}
}

// Design §8.3 / tasks 4.3: an email planted in the tier source cannot reach the
// buffer, and neither can any other identifying field of the file. The event
// still ships without it.
func TestPlanTierPlantedEmailNeverReachesBuffer(t *testing.T) {
	tmp := setupCaptureState(t)
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeLines(t, path, `{"oauthAccount":{"accountUuid":"`+plantedUUID+`","emailAddress":"`+plantedEmail+`",`+
		`"organizationType":"`+plantedEmail+`","organizationRateLimitTier":"a@b.com","billingType":"eyJhbGciOiJIUzI1NiJ9.e30.sig",`+
		`"seatTier":"Planted Name","profileFetchedAt":1791347467270}}`)
	r := readClaudePlanTier(path)
	// Bypass the reader's own pattern check too, so the projection alone is tested.
	r.VendorPlan, r.VendorRateLimitTier = "a@b.com", plantedEmail
	if err := queuePlanEvent(buildPlanTierEvent(r, time.Now().Unix(), "dev-1"), false); err != nil {
		t.Fatal(err)
	}
	h := limitHit{Provider: providerClaudeCode, Window: "five_hour", ResetsAt: 1790587800, FirstSeenAt: 1790580000,
		Evidence: evidenceRejectedRequest, OverageStatus: plantedEmail, SessionID: "s1"}
	if err := queuePlanEvent(buildUsageLimitHitEvent(h, r.AccountRef, time.Now().Unix(), "dev-1"), false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{state.HookBufferPath(), filepath.Join(tmp, "outbox.jsonl")} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		// Dropped, not masked: a downstream email scrubber turning the value into
		// [REDACTED_EMAIL] would still ship the key, so assert the KEYS are gone.
		for _, bad := range []string{"@", "eyj", "eyJ", "REDACTED", plantedUUID, "Planted", "accountUuid", "account_id",
			`"vendorPlan"`, `"vendorRateLimitTier"`, `"overageStatus"`, `"billingType"`, `"seatTier"`} {
			if strings.Contains(s, bad) {
				t.Errorf("%s leaked %q", filepath.Base(p), bad)
			}
		}
		if !strings.Contains(s, `"kind":"planTier"`) || !strings.Contains(s, `"kind":"usageLimitHit"`) {
			t.Errorf("%s: events did not ship without the planted fields: %s", filepath.Base(p), s)
		}
	}
}

func TestReadCodexPlanTier(t *testing.T) {
	setupCaptureState(t)
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	writeLines(t, auth, `{"auth_mode":"chatgpt","tokens":{"access_token":"eyJplanted","refresh_token":"rt-planted","account_id":"acct-planted",`+
		`"id_token":"x.`+base64.RawURLEncoding.EncodeToString([]byte(`{"email":"`+plantedEmail+`","https://api.openai.com/auth":{"chatgpt_plan_type":"plus","chatgpt_subscription_last_checked":"2026-09-18T00:00:00Z"}}`))+`.y"}}`)
	now := time.Now()
	sessions := filepath.Join(dir, "sessions")
	writeLines(t, filepath.Join(sessions, "rollout-a.jsonl"),
		strings.Replace(codexTokenCount(now.Add(-time.Hour).UTC().Format(time.RFC3339Nano), 10, 1791053686, "codex"), "prolite", "pro", 1),
		codexTokenCount(now.Add(-time.Minute).UTC().Format(time.RFC3339Nano), 12, 1791053686, "codex"),
	)
	r := readCodexPlanTier(sessions, auth, now)
	if r.VendorPlan != "prolite" || r.TierSource != tierSourceCodexRollout || r.OverageEnabled == nil || *r.OverageEnabled ||
		len(r.AccountRef) != 16 || r.SourceObservedAt != now.Add(-time.Minute).Unix() {
		t.Errorf("rollout reading = %+v", r)
	}
	// A resumed old rollout: fresh mtime, but its only plan row is 8 days old.
	// That stale plan must not beat auth.json's current one.
	stale := filepath.Join(dir, "stale-sessions")
	writeLines(t, filepath.Join(stale, "rollout-old.jsonl"),
		codexTokenCount(now.Add(-8*24*time.Hour).UTC().Format(time.RFC3339Nano), 12, 1791053686, "codex"))
	if r := readCodexPlanTier(stale, auth, now); r.VendorPlan != "plus" || r.TierSource != tierSourceCodexIDToken {
		t.Errorf("stale rollout row won: %+v", r)
	}
	// No rollout in 7 days: fall back to the id_token claim.
	r = readCodexPlanTier(filepath.Join(dir, "none"), auth, now)
	if r.VendorPlan != "plus" || r.TierSource != tierSourceCodexIDToken || r.SourceObservedAt != 1789689600 {
		t.Errorf("id_token reading = %+v", r)
	}
	if r := readCodexPlanTier(filepath.Join(dir, "none"), filepath.Join(dir, "no-auth.json"), now); r.SignalState != planSignalSourceAbsent {
		t.Errorf("no source: %+v", r)
	}
}

func TestReadCursorPlanTier(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
		`INSERT INTO ItemTable VALUES ('cursorAuth/stripeMembershipType', 'pro')`,
		`INSERT INTO ItemTable VALUES ('cursorAuth/cachedEmail', '` + plantedEmail + `')`,
		`INSERT INTO ItemTable VALUES ('cursorAuth/accessToken', 'eyJplanted')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	mtime := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	_ = os.Chtimes(path, mtime, mtime)
	// Read-only: the file must not be modified by the read.
	r, ok := readCursorPlanTier([]string{filepath.Join(dir, "missing.vscdb"), path})
	if !ok || r.VendorPlan != "pro" || r.SourceObservedAt != mtime.Unix() || r.TierSource != tierSourceCursorStateDB || r.AccountRef != "" {
		t.Errorf("reading = %+v ok=%v", r, ok)
	}
	if info, _ := os.Stat(path); !info.ModTime().Equal(mtime) {
		t.Errorf("read modified the store: mtime %v", info.ModTime())
	}
	// A plain mode=ro open of a WAL store creates -wal and -shm beside it.
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + side); err == nil {
			t.Errorf("read created %s in Cursor's directory", side)
		}
	}
	// Missing everywhere: emit nothing.
	if _, ok := readCursorPlanTier([]string{filepath.Join(dir, "missing.vscdb")}); ok {
		t.Error("no store anywhere must emit nothing")
	}
	// Store present, key absent: an observed absence.
	db, _ = sql.Open("sqlite", path)
	_, _ = db.Exec(`DELETE FROM ItemTable WHERE key = 'cursorAuth/stripeMembershipType'`)
	_ = db.Close()
	if r, ok := readCursorPlanTier([]string{path}); !ok || r.SignalState != planSignalSourceAbsent || r.VendorPlan != "" {
		t.Errorf("key absent: %+v", r)
	}
}

// One event per window across scans and restarts; one tier heartbeat per day.
func TestPlanLimitEmitterDedup(t *testing.T) {
	tmp := setupCaptureState(t)
	hits := []limitHit{{Provider: providerClaudeCode, Window: "five_hour", ResetsAt: 1790587800, FirstSeenAt: 1790580000, Evidence: evidenceRejectedRequest, SessionID: "s1"}}
	newEmitter := func() *planLimitEmitter {
		return &planLimitEmitter{
			provider: providerClaudeCode,
			readTier: func(time.Time) (planTierReading, bool) {
				return planTierReading{Provider: providerClaudeCode, TierSource: tierSourceClaudeProfile, VendorPlan: "claude_max", SourceObservedAt: 1}, true
			},
			scanHits: func(time.Time) []limitHit { return hits },
		}
	}
	session := Session{DeviceID: "dev-1"}
	day1 := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	e := newEmitter()
	e.maybe(session, day1, false)
	e.maybe(session, day1.Add(10*time.Minute), false)
	newEmitter().maybe(session, day1.Add(time.Hour), false) // a restarted watcher
	newEmitter().maybe(session, day1.Add(24*time.Hour), false)

	b, _ := os.ReadFile(filepath.Join(tmp, "outbox.jsonl"))
	if n := strings.Count(string(b), `"kind":"usageLimitHit"`); n != 1 {
		t.Errorf("usageLimitHit emitted %d times, want 1", n)
	}
	if n := strings.Count(string(b), `"kind":"planTier"`); n != 2 {
		t.Errorf("planTier emitted %d times, want 2 (day 1 + day 2 heartbeat)", n)
	}
}

// A hit that fails to queue must stay reachable: the scan position is held, so
// the next scan still covers it and it is emitted once the queue recovers.
func TestPlanLimitEmitterHoldsScanOnQueueFailure(t *testing.T) {
	tmp := setupCaptureState(t)
	good := filepath.Join(tmp, "outbox.jsonl")
	blocker := filepath.Join(tmp, "not-a-dir")
	writeLines(t, blocker, "x")
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(blocker, "outbox.jsonl"))

	hits := []limitHit{{Provider: providerClaudeCode, Window: "five_hour", ResetsAt: 1790587800, FirstSeenAt: 1790580000, Evidence: evidenceRejectedRequest, SessionID: "s1"}}
	e := &planLimitEmitter{
		provider: providerClaudeCode,
		readTier: func(time.Time) (planTierReading, bool) { return planTierReading{}, false },
		scanHits: func(time.Time) []limitHit { return hits },
	}
	session := Session{DeviceID: "dev-1"}
	day1 := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	e.maybe(session, day1, false)
	if st := loadPlanLimitState(providerClaudeCode); st.HitScan != 0 || len(st.Hits) != 0 {
		t.Fatalf("failed queue advanced state: %+v", st)
	}

	t.Setenv("PROMPTSTER_OUTBOX_PATH", good)
	e.maybe(session, day1.Add(limitHitScanInterval), false)
	if st := loadPlanLimitState(providerClaudeCode); st.HitScan == 0 || len(st.Hits) != 1 {
		t.Errorf("recovered queue did not record the hit: %+v", st)
	}
	b, _ := os.ReadFile(good)
	if n := strings.Count(string(b), `"kind":"usageLimitHit"`); n != 1 {
		t.Errorf("usageLimitHit emitted %d times after recovery, want 1", n)
	}
}
