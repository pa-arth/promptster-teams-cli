package capture

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Plan tier + usage-limit hits — the `planTier` and `usageLimitHit` events.
//
// SPEC: pa-arth/openspec changes/plan-tier-and-limit-hits/ (design.md §1–§4).
// That spec is the contract for kinds, field names and patterns; the backend
// mirrors them in packages/shared/src/captureAllowlist.ts.
//
// PRIVACY IS THE TRUST BOUNDARY. ~/.claude.json, ~/.codex/auth.json and Cursor's
// state.vscdb hold tokens and emails. Every reader below decodes into a struct
// that NAMES only the non-secret fields it needs, so nothing else is ever held
// in a Go value, logged or emitted. Account ids are hashed on-device with the
// install-local key before they reach an event. ~/.claude/.credentials.json is
// never opened.
//
// The device relays the vendor's OWN plan strings; the backend maps them to our
// tiers, so a new vendor tier needs no CLI release.

const providerCursor = "cursor"

const (
	tierSourceClaudeProfile = "claude_profile"
	tierSourceCodexRollout  = "codex_rollout"
	tierSourceCodexIDToken  = "codex_id_token"
	tierSourceCursorStateDB = "cursor_state_db"

	planSignalSourceAbsent = "source_absent"
	planSignalUnreadable   = "unreadable"
)

var (
	vendorTokenPattern     = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	vendorRateLimitPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
)

// vendorToken lower-cases a vendor string and returns it only if it matches the
// pattern; "" (omitted on the wire) otherwise. A name or an email cannot pass.
func vendorToken(s string, p *regexp.Regexp) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !p.MatchString(s) {
		return ""
	}
	return s
}

// planTierReading is the on-device projection of one provider's tier source.
// SignalState "" means reported; an absence carries no tier fields.
type planTierReading struct {
	Provider            string
	VendorPlan          string
	VendorRateLimitTier string
	SeatTier            string
	BillingType         string
	OverageEnabled      *bool
	TierSource          string
	SourceObservedAt    int64
	AccountRef          string
	SignalState         string
}

func planAbsence(provider, source, signal string) planTierReading {
	return planTierReading{Provider: provider, TierSource: source, SignalState: signal}
}

// planAccountRef is hex(sha256(localKey ‖ accountId))[:16], or "" when either is
// missing. The raw id never leaves this function.
func planAccountRef(accountID string) string {
	accountID = strings.TrimSpace(accountID)
	key := state.CursorAttributionKey()
	if accountID == "" || len(key) == 0 {
		return ""
	}
	h := sha256.New()
	_, _ = h.Write(key)
	_, _ = h.Write([]byte(accountID))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// --- Claude: ~/.claude.json .oauthAccount ------------------------------------

// claudeProfilePath is where Claude Code keeps its global config: inside
// CLAUDE_CONFIG_DIR when that is set, else ~/.claude.json.
func claudeProfilePath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

// claudeProfile names ONLY the oauthAccount fields we relay. emailAddress,
// displayName, fullName, organizationName etc. are skipped by the decoder and
// never land in memory as values.
type claudeProfile struct {
	OauthAccount *struct {
		AccountUUID               string `json:"accountUuid"`
		OrganizationType          string `json:"organizationType"`
		OrganizationRateLimitTier string `json:"organizationRateLimitTier"`
		UserRateLimitTier         string `json:"userRateLimitTier"`
		SeatTier                  string `json:"seatTier"`
		BillingType               string `json:"billingType"`
		HasExtraUsageEnabled      *bool  `json:"hasExtraUsageEnabled"`
		ProfileFetchedAt          int64  `json:"profileFetchedAt"`
	} `json:"oauthAccount"`
}

func readClaudePlanTier(path string) planTierReading {
	f, err := os.Open(path) // #nosec G304 -- fixed Claude config path; only named oauthAccount fields are decoded.
	if err != nil {
		return planAbsence(providerClaudeCode, tierSourceClaudeProfile, planSignalSourceAbsent)
	}
	defer f.Close()
	var p claudeProfile
	if err := json.NewDecoder(f).Decode(&p); err != nil {
		return planAbsence(providerClaudeCode, tierSourceClaudeProfile, planSignalUnreadable)
	}
	a := p.OauthAccount
	if a == nil {
		// An API-key user has no oauthAccount. That is an observed absence, not "free".
		return planAbsence(providerClaudeCode, tierSourceClaudeProfile, planSignalSourceAbsent)
	}
	r := planTierReading{
		Provider:         providerClaudeCode,
		TierSource:       tierSourceClaudeProfile,
		VendorPlan:       vendorToken(a.OrganizationType, vendorTokenPattern),
		SeatTier:         vendorToken(a.SeatTier, vendorTokenPattern),
		BillingType:      vendorToken(a.BillingType, vendorTokenPattern),
		OverageEnabled:   a.HasExtraUsageEnabled,
		AccountRef:       planAccountRef(a.AccountUUID),
		SourceObservedAt: a.ProfileFetchedAt / 1000,
	}
	r.VendorRateLimitTier = vendorToken(a.OrganizationRateLimitTier, vendorRateLimitPattern)
	if u := vendorToken(a.UserRateLimitTier, vendorRateLimitPattern); u != "" {
		r.VendorRateLimitTier = u
	}
	if r.SourceObservedAt <= 0 {
		if info, err := f.Stat(); err == nil {
			r.SourceObservedAt = info.ModTime().Unix()
		}
	}
	return r
}

// --- Codex: newest rollout plan_type, id_token claim as fallback --------------

const codexPlanLookback = 7 * 24 * time.Hour

// forEachCodexTokenCount calls fn for every token_count line carrying
// rate_limits in one rollout. Only the minimal codexTokenCountLine projection is
// parsed; raw line bytes are never retained.
func forEachCodexTokenCount(path string, fn func(ts time.Time, rl map[string]interface{})) {
	forEachSmallLine(path, func(line []byte) {
		if !containsToken(line, "token_count") || !containsToken(line, "rate_limits") {
			return
		}
		var rec codexTokenCountLine
		if json.Unmarshal(line, &rec) != nil || rec.Payload.Type != "token_count" || rec.Payload.RateLimits == nil {
			return
		}
		ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
		if err != nil {
			return
		}
		fn(ts, rec.Payload.RateLimits)
	})
}

// forEachSmallLine calls fn for every line of a JSONL file up to 1 MiB, and
// SKIPS longer lines instead of stopping (a bufio.Scanner stops the whole file
// at its cap, which would hide every hit after one huge tool-output line). The
// rows we look for are ~1 KB. The slice is only valid during fn.
func forEachSmallLine(path string, fn func(line []byte)) {
	f, err := os.Open(path) // #nosec G304 -- a transcript/rollout under the tool's own dir, opened read-only.
	if err != nil {
		return
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadSlice('\n')
		for err == bufio.ErrBufferFull { // oversized: drain to the next newline
			line = nil
			_, err = br.ReadSlice('\n')
		}
		if len(line) > 0 {
			fn(line)
		}
		if err != nil {
			return
		}
	}
}

// walkCodexRollouts visits rollout files modified at/after modifiedAfter.
func walkCodexRollouts(sessionsDir string, modifiedAfter time.Time, fn func(path string)) {
	_ = filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, ".jsonl") || info.ModTime().Before(modifiedAfter) {
			return nil
		}
		fn(path)
		return nil
	})
}

// codexAccountRef hashes auth.json tokens.account_id. Only that one field is
// named; the decoder skips every token.
func codexAccountRef(authPath string) string {
	var a struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	data, err := os.ReadFile(authPath) // #nosec G304 -- fixed Codex auth path; only tokens.account_id is decoded.
	if err != nil || json.Unmarshal(data, &a) != nil {
		return ""
	}
	return planAccountRef(a.Tokens.AccountID)
}

func readCodexPlanTier(sessionsDir, authPath string, now time.Time) planTierReading {
	var best planTierReading
	var bestTs time.Time
	walkCodexRollouts(sessionsDir, now.Add(-codexPlanLookback), func(path string) {
		forEachCodexTokenCount(path, func(ts time.Time, rl map[string]interface{}) {
			plan, _ := rl["plan_type"].(string)
			plan = vendorToken(plan, vendorTokenPattern)
			if plan == "" || !ts.After(bestTs) {
				return
			}
			bestTs = ts
			best = planTierReading{Provider: providerCodex, TierSource: tierSourceCodexRollout, VendorPlan: plan, SourceObservedAt: ts.Unix()}
			if c, ok := rl["credits"].(map[string]interface{}); ok {
				has, ok1 := c["has_credits"].(bool)
				unl, ok2 := c["unlimited"].(bool)
				if ok1 || ok2 {
					v := has || unl
					best.OverageEnabled = &v
				}
			}
		})
	})
	if bestTs.IsZero() {
		best = readCodexIDTokenPlan(authPath)
	}
	if best.SignalState == "" {
		best.AccountRef = codexAccountRef(authPath)
	}
	return best
}

// readCodexIDTokenPlan is the fallback: decode ONLY the JWT payload's plan claim.
// The token is never verified, stored or logged; it goes out of scope here.
func readCodexIDTokenPlan(authPath string) planTierReading {
	data, err := os.ReadFile(authPath) // #nosec G304 -- fixed Codex auth path; only tokens.id_token's plan claim is decoded.
	if err != nil {
		return planAbsence(providerCodex, tierSourceCodexIDToken, planSignalSourceAbsent)
	}
	var a struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &a) != nil {
		return planAbsence(providerCodex, tierSourceCodexIDToken, planSignalUnreadable)
	}
	if a.Tokens.IDToken == "" {
		return planAbsence(providerCodex, tierSourceCodexIDToken, planSignalSourceAbsent)
	}
	parts := strings.Split(a.Tokens.IDToken, ".")
	if len(parts) != 3 {
		return planAbsence(providerCodex, tierSourceCodexIDToken, planSignalUnreadable)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return planAbsence(providerCodex, tierSourceCodexIDToken, planSignalUnreadable)
	}
	var claims struct {
		Auth *struct {
			PlanType    string      `json:"chatgpt_plan_type"`
			LastChecked interface{} `json:"chatgpt_subscription_last_checked"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return planAbsence(providerCodex, tierSourceCodexIDToken, planSignalUnreadable)
	}
	if claims.Auth == nil || claims.Auth.PlanType == "" {
		return planAbsence(providerCodex, tierSourceCodexIDToken, planSignalSourceAbsent)
	}
	r := planTierReading{Provider: providerCodex, TierSource: tierSourceCodexIDToken, VendorPlan: vendorToken(claims.Auth.PlanType, vendorTokenPattern)}
	switch v := claims.Auth.LastChecked.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			r.SourceObservedAt = t.Unix()
		}
	case float64:
		r.SourceObservedAt = int64(v)
	}
	return r
}

// --- Cursor: one ItemTable key, read-only ------------------------------------

const cursorMembershipKey = "cursorAuth/stripeMembershipType"

// cursorPlanStateDBCandidates lists where Cursor's global state store can be:
// the platform user dir, and on WSL the Windows-side user dirs.
func cursorPlanStateDBCandidates() []string {
	if override := os.Getenv(cursorStateDBEnv); override != "" {
		return []string{override}
	}
	rel := filepath.Join("Cursor", "User", "globalStorage", "state.vscdb")
	home, _ := os.UserHomeDir()
	var out []string
	switch runtime.GOOS {
	case "darwin":
		out = append(out, filepath.Join(home, "Library", "Application Support", rel))
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			out = append(out, filepath.Join(appData, rel))
		}
	default:
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		out = append(out, filepath.Join(cfg, rel))
		// WSL: Cursor runs on the Windows side.
		wsl, _ := filepath.Glob("/mnt/c/Users/*/AppData/Roaming/" + filepath.ToSlash(rel))
		out = append(out, wsl...)
	}
	return out
}

// readCursorPlanTier reads the membership key from the most recently modified
// store. ok=false when no store exists anywhere: we emit NOTHING then, rather
// than a fake tier.
func readCursorPlanTier(candidates []string) (planTierReading, bool) {
	var path string
	var mtime time.Time
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err != nil || info.IsDir() {
			continue
		}
		m := info.ModTime()
		// WAL mode: the newest write may be in -wal, not the main file. An
		// empty -wal holds no write, so its mtime says nothing.
		if w, err := os.Stat(c + "-wal"); err == nil && w.Size() > 0 && w.ModTime().After(m) {
			m = w.ModTime()
		}
		if path == "" || m.After(mtime) {
			path, mtime = c, m
		}
	}
	if path == "" {
		return planTierReading{}, false
	}
	value, found, err := queryCursorMembership(path)
	switch {
	case err != nil:
		return planAbsence(providerCursor, tierSourceCursorStateDB, planSignalUnreadable), true
	case !found:
		return planAbsence(providerCursor, tierSourceCursorStateDB, planSignalSourceAbsent), true
	}
	return planTierReading{
		Provider:         providerCursor,
		TierSource:       tierSourceCursorStateDB,
		VendorPlan:       vendorToken(value, vendorTokenPattern),
		SourceObservedAt: mtime.Unix(),
	}, true
}

// queryCursorMembership asks for exactly one key; tokens and cachedEmail are
// never selected.
//
// With no -wal beside the store, Cursor is not writing it, so it is opened
// mode=ro&immutable=1. That matters: a plain mode=ro open of a WAL-mode store
// CREATES an empty -wal and a -shm in Cursor's directory (observed on
// /mnt/c, 2026-10-07), a write side effect a read must not have. With a -wal
// present Cursor may be live, so it is opened mode=ro (design §1) and falls
// back to immutable=1 only if a lock refuses that; a torn read of a one-word
// value at worst fails the vendor pattern and is dropped.
func queryCursorMembership(path string) (string, bool, error) {
	base := "file:" + escapeSQLiteURI(filepath.ToSlash(path)) + "?mode=ro"
	if _, err := os.Stat(path + "-wal"); err != nil {
		return queryOneCursorKey(base + "&immutable=1")
	}
	value, found, err := queryOneCursorKey(base)
	if err != nil {
		value, found, err = queryOneCursorKey(base + "&immutable=1")
	}
	return value, found, err
}

func queryOneCursorKey(dsn string) (string, bool, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var raw []byte
	err = db.QueryRowContext(ctx, `SELECT value FROM ItemTable WHERE key = ?`, cursorMembershipKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// The value may be stored as a bare word or a JSON string.
	s := strings.TrimSpace(string(raw))
	var unquoted string
	if json.Unmarshal([]byte(s), &unquoted) == nil {
		s = unquoted
	}
	return s, true, nil
}

// --- planTier event -----------------------------------------------------------

func (r planTierReading) tupleKey() string {
	ov := "-"
	if r.OverageEnabled != nil {
		ov = fmt.Sprint(*r.OverageEnabled)
	}
	return strings.Join([]string{r.SignalState, r.TierSource, r.VendorPlan, r.VendorRateLimitTier, r.SeatTier, r.BillingType, ov}, "|")
}

func buildPlanTierEvent(r planTierReading, capturedAt int64, deviceID string) event.Event {
	e := event.NewEvent("planTier", deviceID)
	e.Source = "cli"
	e.Actor = event.SystemActor()
	e.DeviceID = deviceID
	data := map[string]interface{}{
		"provider":   r.Provider,
		"tierSource": r.TierSource,
		"capturedAt": capturedAt,
	}
	if r.AccountRef != "" {
		data["accountRef"] = r.AccountRef
	}
	if r.SignalState != "" {
		// An absence carries no tier fields, by construction.
		data["signalState"] = r.SignalState
	} else {
		for k, v := range map[string]string{
			"vendorPlan": r.VendorPlan, "vendorRateLimitTier": r.VendorRateLimitTier,
			"seatTier": r.SeatTier, "billingType": r.BillingType,
		} {
			if v != "" {
				data[k] = v
			}
		}
		if r.OverageEnabled != nil {
			data["overageEnabled"] = *r.OverageEnabled
		}
		if r.SourceObservedAt > 0 {
			data["sourceObservedAt"] = r.SourceObservedAt
		}
	}
	e.Data = data
	utcDate := time.Unix(capturedAt, 0).UTC().Format("2006-01-02")
	e.ID = event.DeterministicUUID(fmt.Sprintf("planTier:%s:%s:%s:%s", r.Provider, r.AccountRef, r.tupleKey(), utcDate))
	return e
}

// queuePlanEvent signs, buffers and queues one event; an outbox failure is
// returned so the caller does not mark it emitted.
func queuePlanEvent(e event.Event, captureProse bool) error {
	ev := e
	if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
		fmt.Fprintf(os.Stderr, "plan-tier: buffer error: %v\n", err)
	}
	return outbox.Append(ev)
}
