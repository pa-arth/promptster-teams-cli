package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
)

// Regression coverage for the bounded-history backfill. Every test here pins a
// way that replaying weeks of local transcripts through the LIVE event funnel
// used to report old work as if it had just happened, or used to read the whole
// window in one unbounded pass.

// aiFileDiffAt builds a likely_ai file_diff stamped at ts — the shape the
// watchers hand the funnel when they replay history.
func aiFileDiffAt(sessionID, path, diff string, ts time.Time) event.Event {
	e := event.NewEvent("file_diff", sessionID)
	e.Ts = ts.UTC().Format(time.RFC3339Nano)
	e.Data = map[string]interface{}{"path": path, "diff": diff}
	e.Provenance = &event.Provenance{Attribution: "likely_ai", Confidence: 1}
	return e
}

// TestReplayedFileDiffDoesNotFabricateFreshAiActivity is the fabrication proof.
// A file an agent last touched 20 days ago, hand-edited by the engineer ever
// since, must not re-enter the AI-paths ledger as touched TODAY when the
// backfill replays that session: the git watcher would then tag the next
// commit's purely-human lines likely_ai and seed them as AI durability spans.
func TestReplayedFileDiffDoesNotFabricateFreshAiActivity(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "foo.go"), []byte("package foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	old := aiFileDiffAt("sess-replay", "foo.go", "@@\n+package foo", time.Now().Add(-20*24*time.Hour))
	dedupeFileDiff(ws, &old, true)

	if got := readAiTouchedPaths(""); len(got) != 0 {
		t.Fatalf("a 20-day-old replayed edit must not read as AI-touched today; got %v", got)
	}

	// The same funnel, live, still records — the fix must not silence real work.
	live := aiFileDiffAt("sess-live", "bar.go", "@@\n+package bar", time.Now())
	dedupeFileDiff(ws, &live, false)
	if readAiTouchedPaths("")["bar.go"] != "sess-live" {
		t.Fatalf("a live AI edit must still be recorded; got %v", readAiTouchedPaths(""))
	}
}

// TestReplayedFileDiffCarriesHistoricalWriteStamp pins the per-path evidence
// clock. durabilitySeedAuthorized re-authorizes a buried path only on a
// STRICTLY NEWER AI write stamp, so a replayed edit that stamped the wall clock
// would lift a tombstone that no agent had earned.
func TestReplayedFileDiffCarriesHistoricalWriteStamp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "app.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	threeDaysAgo := time.Now().Add(-3 * 24 * time.Hour)
	replayed := aiFileDiffAt("sess-hist", "app.go", "@@\n+one", threeDaysAgo)
	dedupeFileDiff(ws, &replayed, true)

	mark := readAiPathMarks("")["app.go"]
	if mark.SessionID != "sess-hist" {
		t.Fatalf("in-TTL history must still be recorded; got %+v", mark)
	}
	drift := time.Now().UnixMilli() - mark.WriteMs
	if drift < (2 * 24 * time.Hour).Milliseconds() {
		t.Fatalf("replayed write stamped %dms ago; want the edit's own ~3-day-old time", drift)
	}

	// A replay of an even OLDER edit to the same path must not advance the
	// stamp: the collision bump exists for two live writes in one millisecond,
	// not to let history manufacture "the agent wrote this again" evidence.
	before := mark.WriteMs
	older := aiFileDiffAt("sess-hist", "app.go", "@@\n+two", threeDaysAgo.Add(-time.Hour))
	dedupeFileDiff(ws, &older, true)
	if after := readAiPathMarks("")["app.go"].WriteMs; after > before {
		t.Fatalf("an older replayed write advanced the stamp %d -> %d", before, after)
	}

	// A genuine live write to the same path still advances it.
	live := aiFileDiffAt("sess-hist", "app.go", "@@\n+three", time.Now())
	dedupeFileDiff(ws, &live, false)
	if after := readAiPathMarks("")["app.go"].WriteMs; after <= before {
		t.Fatalf("a live write must advance the stamp; %d -> %d", before, after)
	}
}

// TestReplayedFileDiffsAreNotCollapsedByCurrentContent pins the dedup bypass.
// The dedup ledger is keyed by the file's CURRENT content hash, so replaying
// twenty historical edits to one path used to key all twenty to today's bytes:
// the first won and the rest vanished.
func TestReplayedFileDiffsAreNotCollapsedByCurrentContent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "svc.go"), []byte("current bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	base := time.Now().Add(-10 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		e := aiFileDiffAt("sess-replay", "svc.go", fmt.Sprintf("@@\n+edit %d", i), base.Add(time.Duration(i)*time.Hour))
		if !dedupeFileDiff(ws, &e, true) {
			t.Fatalf("replayed edit %d was collapsed against current on-disk content", i)
		}
	}

	// Replay must also leave no claim behind that would swallow a genuine live
	// edit landing on this same content inside the next dedup TTL.
	live := aiFileDiffAt("sess-live", "svc.go", "@@\n+live", time.Now())
	if !dedupeFileDiff(ws, &live, false) {
		t.Fatal("replay poisoned the dedup ledger against a live edit")
	}
	// Live cross-channel dedup itself is untouched.
	again := aiFileDiffAt("sess-git", "svc.go", "diff --git", time.Now())
	if dedupeFileDiff(ws, &again, false) {
		t.Fatal("live cross-channel dedup must still collapse identical resulting content")
	}
}

// TestReplayedAiBashWindowIsNotRecorded keeps a replayed command out of the
// bash-window ledger. Its window is already outside the reader's TTL, so it can
// recover nothing — it can only spend a slot in the 64-session cap that a first
// boot would otherwise fill with history, evicting the live session.
func TestReplayedAiBashWindowIsNotRecorded(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	old := event.NewEvent("command", "sess-old")
	old.Ts = time.Now().Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	old.Provenance = &event.Provenance{Attribution: "likely_ai", Confidence: 1}
	recordAiBashWindow(&old, "", true)
	if got := readBashWindows(""); len(got) != 0 {
		t.Fatalf("a 20-day-old replayed command must not enter the window ledger; got %v", got)
	}

	live := event.NewEvent("command", "sess-live")
	live.Provenance = &event.Provenance{Attribution: "likely_ai", Confidence: 1}
	recordAiBashWindow(&live, "", false)
	if got := readBashWindows(""); len(got) != 1 {
		t.Fatalf("a live AI command must still be recorded; got %v", got)
	}
}

// TestAnchoredLiveEventIsNotTreatedAsReplay is the Cursor guard. Cursor stamps
// every action in a turn with that TURN'S START anchor, so a long turn hands
// the shared funnel live edits that look arbitrarily old. Inferring replay from
// that age silently disables live cross-channel dedupe and freezes the per-path
// write stamp durabilitySeedAuthorized and reworkSeedEvidence read as "the
// agent wrote this again". Replay is the producer's call, and Cursor's answer
// is always no.
func TestAnchoredLiveEventIsNotTreatedAsReplay(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	ws := t.TempDir()
	target := filepath.Join(ws, "turn.go")
	if err := os.WriteFile(target, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A 46-minute turn: every action carries the turn's start anchor.
	anchor := time.Now().Add(-46 * time.Minute)

	first := aiFileDiffAt("cursor-sess", "turn.go", "@@\n+first", anchor)
	if !dedupeFileDiff(ws, &first, false) {
		t.Fatal("first live edit must emit")
	}
	// Live cross-channel dedupe must still fire for an anchored event: the git
	// watcher seeing this same resulting content has to be collapsed.
	gitEcho := aiFileDiffAt("cursor-sess", "turn.go", "diff --git", anchor)
	if dedupeFileDiff(ws, &gitEcho, false) {
		t.Fatal("an anchored live edit must still claim the dedup ledger")
	}

	before := readAiPathMarks("")["turn.go"].WriteMs
	if before == 0 {
		t.Fatalf("anchored live edit was not recorded: %+v", readAiPathMarks(""))
	}
	// Two genuine edits to one path inside a single turn share the anchor. The
	// collision bump must still distinguish them, or a tombstoned path is never
	// re-authorized by the second write.
	if err := os.WriteFile(target, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := aiFileDiffAt("cursor-sess", "turn.go", "@@\n+second", anchor)
	if !dedupeFileDiff(ws, &second, false) {
		t.Fatal("a second live edit with new content must emit")
	}
	if after := readAiPathMarks("")["turn.go"].WriteMs; after <= before {
		t.Fatalf("a second live edit in the same turn must advance the stamp; %d -> %d", before, after)
	}

	// The session's activity stamp must be NOW, not the turn anchor — it feeds
	// the 7-day TTL and the 64-entry eviction order.
	drift := time.Now().UnixMilli() - readAiPathMarks("")["turn.go"].WriteMs
	if drift > time.Minute.Milliseconds() {
		t.Fatalf("live stamp lags the wall clock by %dms; the turn anchor leaked in", drift)
	}
}

// TestEmitCursorEventNeverReplays pins the producer signal at the funnel Cursor
// actually calls, so the constant cannot drift back to an inferred one.
func TestEmitCursorEventNeverReplays(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "c.go"), []byte("body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := Session{DeviceID: "dev-cursor", SessionToken: "PSE-TEST", TaskRoot: ws}

	ev := aiFileDiffAt("cursor-sess", filepath.Join(ws, "c.go"), "@@\n+body", time.Now().Add(-46*time.Minute))
	if n := emitCursorEvent(ev, session, false); n != 1 {
		t.Fatalf("anchored cursor edit queued %d events, want 1", n)
	}
	mark := readAiPathMarks(gitWatchRootKey(ws))["c.go"]
	if mark.SessionID != "cursor-sess" {
		t.Fatalf("cursor edit missing from the ai-paths ledger: %+v", readAiPathMarks(gitWatchRootKey(ws)))
	}
	if drift := time.Now().UnixMilli() - mark.WriteMs; drift > time.Minute.Milliseconds() {
		t.Fatalf("cursor write stamped %dms ago; the turn anchor leaked in as a replay stamp", drift)
	}
}

// TestReplayedBashWindowDoesNotRefreshSessionActivity pins the sibling ledger's
// half of the same rule. pruneBashWindows evicts oldest-by-TsMs against the same
// 64-session cap the ai-paths ledger uses, so a backfill stamping the wall clock
// spends that cap on history and evicts the live session's windows.
func TestReplayedBashWindowDoesNotRefreshSessionActivity(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	// In-TTL history: recorded, but as three-day-old activity.
	replayed := event.NewEvent("command", "sess-hist")
	replayed.Ts = time.Now().Add(-3 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	replayed.Provenance = &event.Provenance{Attribution: "likely_ai", Confidence: 1}
	recordAiBashWindow(&replayed, "", true)

	data, err := os.ReadFile(bashWindowsLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	var ledger bashWindowsLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	entry, ok := ledger.Sessions["sess-hist"]
	if !ok {
		t.Fatalf("an in-TTL replayed window must still be recorded; got %v", ledger.Sessions)
	}
	if drift := time.Now().UnixMilli() - entry.TsMs; drift < (2 * 24 * time.Hour).Milliseconds() {
		t.Fatalf("replayed session stamped active %dms ago; want its own ~3-day-old time", drift)
	}

	// Live activity on the same session still moves it forward.
	live := event.NewEvent("command", "sess-hist")
	live.Provenance = &event.Provenance{Attribution: "likely_ai", Confidence: 1}
	recordAiBashWindow(&live, "", false)

	data, err = os.ReadFile(bashWindowsLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	ledger = bashWindowsLedger{}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	if drift := time.Now().UnixMilli() - ledger.Sessions["sess-hist"].TsMs; drift > time.Minute.Milliseconds() {
		t.Fatalf("live activity must refresh the session stamp; %dms stale", drift)
	}
}

// TestOutOfTtlReplayWritesNoLedger pins the early return. A replayed write
// already past the TTL would otherwise take the ledger lock, rewrite the file
// and let pruneAiPaths delete the entry in the same call — pure contention
// against live capture, once per historical edit across a 28-day backfill.
func TestOutOfTtlReplayWritesNoLedger(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	recordAiTouchedPathAt("sess-old", "", "old.go", time.Now().Add(-20*24*time.Hour).UnixMilli(), true)
	if _, err := os.Stat(aiPathsLedgerPath()); !os.IsNotExist(err) {
		t.Fatalf("an out-of-TTL replayed write must not touch the ledger file at all (stat err = %v)", err)
	}

	// A replay with no usable stamp lands in the same fail-closed branch.
	recordAiTouchedPathAt("sess-nots", "", "nots.go", 0, true)
	if _, err := os.Stat(aiPathsLedgerPath()); !os.IsNotExist(err) {
		t.Fatalf("a stampless replayed write must not touch the ledger file (stat err = %v)", err)
	}

	// In-TTL history and live writes still land.
	recordAiTouchedPathAt("sess-recent", "", "recent.go", time.Now().Add(-time.Hour).UnixMilli(), true)
	if readAiTouchedPaths("")["recent.go"] != "sess-recent" {
		t.Fatalf("in-TTL replayed write must be recorded; got %v", readAiTouchedPaths(""))
	}
}

// TestClassifyClaudeTranscriptFailsClosedOnUnknownAge pins the age gate's
// direction. mtime is the only other bound, and a session started six months
// ago but resumed today has today's mtime — so an absent or unparseable first
// timestamp must seed to EOF (undercount), never replay from byte zero.
func TestClassifyClaudeTranscriptFailsClosedOnUnknownAge(t *testing.T) {
	dir := t.TempDir()
	ws := resolvePath(t.TempDir())
	roots := []string{ws}
	cutoff := transcriptHistoryCutoff(time.Now().UTC())

	cases := []struct {
		name string
		line string
		want claudeMatchResult
	}{
		{"missing timestamp", fmt.Sprintf(`{"type":"user","cwd":%q,"message":{"role":"user","content":"hi"}}`, ws), claudeMatchYesPreexisting},
		{"unparseable timestamp", fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":"not-a-time","message":{"role":"user","content":"hi"}}`, ws), claudeMatchYesPreexisting},
		{"in-window timestamp", fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"hi"}}`, ws, time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339)), claudeMatchYes},
	}
	for i, tc := range cases {
		path := filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i))
		if err := os.WriteFile(path, []byte(tc.line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := classifyClaudeTranscript(path, roots, cutoff); got != tc.want {
			t.Errorf("%s: classify = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestClassifyCodexRolloutFailsClosedOnUnknownAge is the Codex half of the
// same gate.
func TestClassifyCodexRolloutFailsClosedOnUnknownAge(t *testing.T) {
	dir := t.TempDir()
	ws := resolvePath(t.TempDir())
	roots := []string{ws}
	cutoff := transcriptHistoryCutoff(time.Now().UTC())

	bad := filepath.Join(dir, "rollout-bad.jsonl")
	if err := os.WriteFile(bad, []byte(codexSessionMetaLine(ws, "not-a-time")), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := classifyCodexRollout(bad, roots, cutoff); got != codexMatchYesPreexisting {
		t.Errorf("unparseable session_meta timestamp = %v, want codexMatchYesPreexisting", got)
	}

	good := filepath.Join(dir, "rollout-good.jsonl")
	ts := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(good, []byte(codexSessionMetaLine(ws, ts)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := classifyCodexRollout(good, roots, cutoff); got != codexMatchYes {
		t.Errorf("in-window session_meta = %v, want codexMatchYes", got)
	}
}

// TestTranscriptHistoryCutoffRolls pins the window as RELATIVE: the same
// transcript that classifies in-window at one clock reading must classify
// out-of-window at a later one. That is the property the watch loops' per-poll
// recompute exists to preserve — frozen at boot the cutoff is an absolute date,
// so a daemon up for 60 days would still admit an 88-day-old transcript from
// byte zero. The recompute itself lives inside the loops and is not reachable
// from here; this pins the semantics it depends on.
func TestTranscriptHistoryCutoffRolls(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := base.Add(30 * 24 * time.Hour)
	if !transcriptHistoryCutoff(later).After(transcriptHistoryCutoff(base)) {
		t.Fatal("the history cutoff must advance with the clock")
	}

	// A session in-window at boot must fall OUT of the window 30 days later.
	dir := t.TempDir()
	ws := resolvePath(t.TempDir())
	path := filepath.Join(dir, "aging.jsonl")
	line := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"hi"}}`+"\n",
		ws, base.Add(-24*time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := []string{ws}
	if got := classifyClaudeTranscript(path, roots, transcriptHistoryCutoff(base)); got != claudeMatchYes {
		t.Fatalf("at boot the session is in-window; got %v", got)
	}
	if got := classifyClaudeTranscript(path, roots, transcriptHistoryCutoff(later)); got != claudeMatchYesPreexisting {
		t.Fatalf("30 days later the same session must be out of window; got %v", got)
	}
}

// writeClaudeHistory lays down one transcript with n in-window prompt lines and
// returns its path plus its total size.
func writeClaudeHistory(t *testing.T, root, ws, name string, n int) (string, int64) {
	t.Helper()
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"prompt %d"}}`+"\n", ws, ts, i)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, int64(len(b.String()))
}

// TestPollClaudeTranscriptsChunksBackfill pins the per-poll read budget: the
// first poll after an upgrade used to read every in-window transcript to EOF in
// one unbounded pass, which blocks shutdown for its whole duration (the signal
// select sits after the poll) and puts every byte of offset progress at risk of
// a single kill. A bounded poll caps both. The per-file save that narrows the
// loss further is deliberately NOT asserted here — the poll's end-of-pass save
// makes the two indistinguishable from outside, and a seam to tell them apart
// would be test scaffolding in the watcher.
func TestPollClaudeTranscriptsChunksBackfill(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	path, size := writeClaudeHistory(t, root, ws, "chunked.jsonl", 40)

	prev := claudeWatchMaxBytesPerPoll
	claudeWatchMaxBytesPerPoll = size / 4
	t.Cleanup(func() { claudeWatchMaxBytesPerPoll = prev })

	session := Session{DeviceID: "sess-chunk", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}
	key := claudeProgressKey(path)

	_, consumed := pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, true, false)
	if consumed == 0 || consumed >= size {
		t.Fatalf("first poll consumed %d of %d bytes; want a bounded chunk", consumed, size)
	}
	// The bounded chunk must be persisted, so the next poll resumes rather than
	// re-reading it.
	saved := loadClaudeWatchProgress().Offsets[key]
	if saved != consumed {
		t.Fatalf("offset saved after a bounded poll = %d, want %d", saved, consumed)
	}

	// Subsequent polls drain the remainder — deferred, never dropped.
	total := consumed
	for i := 0; i < 20 && total < size; i++ {
		_, c := pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, true, false)
		if c == 0 {
			break
		}
		total += c
	}
	if total != size {
		t.Fatalf("backfill drained %d of %d bytes across polls", total, size)
	}
}

// TestPollClaudeTranscriptsDefersUnderOutboxPressure pins the backpressure. The
// transcript is already durable on disk with its offset unmoved, so declining
// to read defers the work perfectly; reading on would race the outbox to the
// cap where Append DROPS, taking live capture down with the backfill.
func TestPollClaudeTranscriptsDefersUnderOutboxPressure(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	outboxPath := filepath.Join(stateDir, "outbox.jsonl")
	t.Setenv("PROMPTSTER_OUTBOX_PATH", outboxPath)

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	path, size := writeClaudeHistory(t, root, ws, "pressured.jsonl", 10)

	if err := os.WriteFile(outboxPath, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := outbox.PressureHighWater
	outbox.PressureHighWater = 1024
	t.Cleanup(func() { outbox.PressureHighWater = prev })

	session := Session{DeviceID: "sess-pressure", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}

	if _, consumed := pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, true, false); consumed != 0 {
		t.Fatalf("a pressured outbox must defer the read; consumed %d bytes", consumed)
	}
	if got := loadClaudeWatchProgress().Offsets[claudeProgressKey(path)]; got != 0 {
		t.Fatalf("a deferred read must leave the offset at 0; got %d", got)
	}

	// Pressure clears — the deferred bytes are still there to read.
	outbox.PressureHighWater = prev
	if err := os.Remove(outboxPath); err != nil {
		t.Fatal(err)
	}
	if _, consumed := pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, true, false); consumed != size {
		t.Fatalf("deferred history must drain once pressure clears; consumed %d of %d", consumed, size)
	}
}

// TestPollCodexRolloutsChunksBackfill is the Codex half of the read budget.
func TestPollCodexRolloutsChunksBackfill(t *testing.T) {
	root := codexSessionsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	dir := filepath.Join(root, "2026", "07", "20")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	var b strings.Builder
	b.WriteString(codexSessionMetaLine(ws, ts))
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, `{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":"prompt %d","images":[]}}`+"\n", ts, i)
	}
	path := filepath.Join(dir, "rollout-chunked.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	size := int64(len(b.String()))

	prev := codexWatchMaxBytesPerPoll
	codexWatchMaxBytesPerPoll = size / 4
	t.Cleanup(func() { codexWatchMaxBytesPerPoll = prev })

	session := Session{DeviceID: "sess-codex-chunk", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	processors := map[string]*normalize.CodexRolloutProcessor{}

	pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false)
	first := loadCodexWatchProgress().Offsets[path]
	if first == 0 || first >= size {
		t.Fatalf("first poll advanced to %d of %d bytes; want a bounded chunk", first, size)
	}

	for i := 0; i < 20 && loadCodexWatchProgress().Offsets[path] < size; i++ {
		before := loadCodexWatchProgress().Offsets[path]
		pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false)
		if loadCodexWatchProgress().Offsets[path] == before {
			break
		}
	}
	if got := loadCodexWatchProgress().Offsets[path]; got != size {
		t.Fatalf("backfill drained %d of %d bytes across polls", got, size)
	}
}

// TestPollCodexRolloutsDefersUnderOutboxPressure is the Codex half of the
// backpressure.
func TestPollCodexRolloutsDefersUnderOutboxPressure(t *testing.T) {
	root := codexSessionsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	outboxPath := filepath.Join(stateDir, "outbox.jsonl")
	t.Setenv("PROMPTSTER_OUTBOX_PATH", outboxPath)

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	dir := filepath.Join(root, "2026", "07", "21")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body := codexSessionMetaLine(ws, ts) +
		`{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"user_message","message":"history","images":[]}}` + "\n"
	path := filepath.Join(dir, "rollout-pressured.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(outboxPath, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := outbox.PressureHighWater
	outbox.PressureHighWater = 1024
	t.Cleanup(func() { outbox.PressureHighWater = prev })

	session := Session{DeviceID: "sess-codex-pressure", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	processors := map[string]*normalize.CodexRolloutProcessor{}

	if sent := pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false); sent != 0 {
		t.Fatalf("a pressured outbox must defer the read; queued %d events", sent)
	}
	if got := loadCodexWatchProgress().Offsets[path]; got != 0 {
		t.Fatalf("a deferred read must leave the offset at 0; got %d", got)
	}

	outbox.PressureHighWater = prev
	if err := os.Remove(outboxPath); err != nil {
		t.Fatal(err)
	}
	if sent := pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false); sent == 0 {
		t.Fatal("deferred history must drain once pressure clears")
	}
}
