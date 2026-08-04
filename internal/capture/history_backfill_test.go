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

// TestTailClaudeTranscriptDoesNotCrossBudgetMidRecord proves the byte cap is a
// hard read boundary, not a check performed after ReadBytes has already pulled
// an arbitrarily large record into memory. A record that does not fit in the
// remaining budget stays intact for the next poll.
func TestTailClaudeTranscriptDoesNotCrossBudgetMidRecord(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	line1 := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"first"}}`+"\n", workspace, ts)
	line2 := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"second record must remain whole"}}`+"\n", workspace, ts)
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	if err := os.WriteFile(path, []byte(line1+line2), 0o600); err != nil {
		t.Fatal(err)
	}

	key := claudeProgressKey(path)
	progress := claudeWatchProgress{Offsets: map[string]int64{}, Match: map[string]string{key: "yes"}, V: claudeProgressSchemaV}
	proc := normalize.NewClaudeTranscriptProcessor("budget-claude")
	session := Session{DeviceID: "device", SessionToken: "PSE-TEST", TaskRoot: workspace}
	budget := int64(len(line1) + len(line2)/2)

	_, res := tailClaudeTranscript(path, progress, proc, session, true, false, budget, true)
	if !res.truncated || res.consumed != int64(len(line1)) {
		t.Fatalf("first poll consumed %d bytes (more=%v), want exactly first record %d", res.consumed, res.truncated, len(line1))
	}
	if got := progress.Offsets[key]; got != int64(len(line1)) {
		t.Fatalf("partial second record advanced offset to %d, want %d", got, len(line1))
	}

	_, res = tailClaudeTranscript(path, progress, proc, session, true, false, int64(len(line2)+1), true)
	if res.consumed != int64(len(line2)) || progress.Offsets[key] != int64(len(line1)+len(line2)) {
		t.Fatalf("deferred record did not drain intact: consumed=%d offset=%d", res.consumed, progress.Offsets[key])
	}
}

// TestPollClaudeRevalidatesMatchAfterRootsNarrow proves a durable positive
// decision cannot outlive the capture scope that authorized it. The offset is
// retained, but content appended after the root is removed is not parsed.
func TestPollClaudeRevalidatesMatchAfterRootsNarrow(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	oldWorkspace := resolvePath(t.TempDir())
	newWorkspace := resolvePath(t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	line1 := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"captured while watched"}}`+"\n", oldWorkspace, ts)
	line2 := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"must stay private after removal"}}`+"\n", oldWorkspace, ts)
	dir := filepath.Join(root, "-Users-me-removed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "removed-root.jsonl")
	if err := os.WriteFile(path, []byte(line1+line2), 0o600); err != nil {
		t.Fatal(err)
	}

	key := claudeProgressKey(path)
	saveClaudeWatchProgress(claudeWatchProgress{
		Offsets: map[string]int64{key: int64(len(line1))},
		Match:   map[string]string{key: "yes"},
		RootsFP: captureRootsFingerprint(workspaceMatchRoots(oldWorkspace)),
		V:       claudeProgressSchemaV,
	})
	session := Session{DeviceID: "device", SessionToken: "PSE-TEST", TaskRoot: newWorkspace, StartedAt: time.Now()}
	parsed, consumed := pollClaudeTranscripts(session, newWorkspace, transcriptHistoryCutoff(time.Now()), map[string]*normalize.ClaudeTranscriptProcessor{}, true, false)
	if parsed != 0 || consumed != 0 {
		t.Fatalf("removed workspace content was captured: parsed=%d consumed=%d", parsed, consumed)
	}
	saved := loadClaudeWatchProgress()
	if saved.Match[key] != "no" || saved.Offsets[key] != int64(len(line1)) {
		t.Fatalf("removed match was not revoked without moving its offset: match=%q offset=%d", saved.Match[key], saved.Offsets[key])
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

// TestTailCodexRolloutDoesNotCrossBudgetMidRecord is the Codex half of the
// hard budget boundary. The second complete JSONL record is physically bounded
// by the limited reader and logically retried from its original offset.
func TestTailCodexRolloutDoesNotCrossBudgetMidRecord(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	line1 := codexSessionMetaLine(workspace, ts)
	line2 := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":"second record must remain whole","images":[]}}`+"\n", ts)
	path := filepath.Join(t.TempDir(), "rollout-budget.jsonl")
	if err := os.WriteFile(path, []byte(line1+line2), 0o600); err != nil {
		t.Fatal(err)
	}

	progress := codexWatchProgress{Offsets: map[string]int64{}, Match: map[string]string{path: "yes"}, V: codexProgressSchemaV}
	proc := normalize.NewCodexRolloutProcessor("budget-codex")
	session := Session{DeviceID: "device", SessionToken: "PSE-TEST", TaskRoot: workspace}
	budget := int64(len(line1) + len(line2)/2)

	_, res := tailCodexRollout(path, progress, proc, session, false, budget, true)
	if !res.truncated || res.consumed != int64(len(line1)) {
		t.Fatalf("first poll consumed %d bytes (more=%v), want exactly first record %d", res.consumed, res.truncated, len(line1))
	}
	if got := progress.Offsets[path]; got != int64(len(line1)) {
		t.Fatalf("partial second record advanced offset to %d, want %d", got, len(line1))
	}

	_, res = tailCodexRollout(path, progress, proc, session, false, int64(len(line2)+1), true)
	if res.consumed != int64(len(line2)) || progress.Offsets[path] != int64(len(line1)+len(line2)) {
		t.Fatalf("deferred record did not drain intact: consumed=%d offset=%d", res.consumed, progress.Offsets[path])
	}
}

// TestPollCodexRevalidatesMatchAfterRootsNarrow mirrors the Claude security
// regression for Codex's globally persisted rollout cache.
func TestPollCodexRevalidatesMatchAfterRootsNarrow(t *testing.T) {
	root := codexSessionsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	oldWorkspace := resolvePath(t.TempDir())
	newWorkspace := resolvePath(t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	line1 := codexSessionMetaLine(oldWorkspace, ts)
	line2 := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":"must stay private after removal","images":[]}}`+"\n", ts)
	dir := filepath.Join(root, "2026", "08", "03")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-removed-root.jsonl")
	if err := os.WriteFile(path, []byte(line1+line2), 0o600); err != nil {
		t.Fatal(err)
	}

	saveCodexWatchProgress(codexWatchProgress{
		Offsets: map[string]int64{path: int64(len(line1))},
		Match:   map[string]string{path: "yes"},
		RootsFP: captureRootsFingerprint(workspaceMatchRoots(oldWorkspace)),
		V:       codexProgressSchemaV,
	})
	session := Session{DeviceID: "device", SessionToken: "PSE-TEST", TaskRoot: newWorkspace, StartedAt: time.Now()}
	if queued := pollCodexRollouts(session, newWorkspace, transcriptHistoryCutoff(time.Now()), map[string]*normalize.CodexRolloutProcessor{}, false); queued != 0 {
		t.Fatalf("removed workspace content queued %d events", queued)
	}
	saved := loadCodexWatchProgress()
	if saved.Match[path] != "no" || saved.Offsets[path] != int64(len(line1)) {
		t.Fatalf("removed match was not revoked without moving its offset: match=%q offset=%d", saved.Match[path], saved.Offsets[path])
	}
}

// TestCleanShutdownPreservesClaudeBackfillProgress pins the restart half of the
// bounded backfill. The watcher's deferred cleanup drops its LIVENESS state on
// every clean exit; it must not drop the DURABLE offsets with it. Wiping them
// used to cost nothing (a restart's re-read was bounded to the last two
// minutes), but with the 28-day window it means every login, every self-update
// re-exec and every sleep/wake re-reads and re-ships the whole window.
func TestCleanShutdownPreservesClaudeBackfillProgress(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	path, size := writeClaudeHistory(t, root, ws, "restart.jsonl", 6)
	key := claudeProgressKey(path)

	session := Session{DeviceID: "sess-restart", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	cutoff := func() time.Time { return transcriptHistoryCutoff(time.Now().UTC()) }

	processors := map[string]*normalize.ClaudeTranscriptProcessor{}
	if _, consumed := pollClaudeTranscripts(session, ws, cutoff(), processors, true, false); consumed != size {
		t.Fatalf("first boot backfilled %d of %d bytes", consumed, size)
	}

	// A clean shutdown — exactly what RunClaudeWatcher's deferred cleanup runs.
	clearClaudeWatcherState()

	if _, err := os.Stat(claudeWatchProgressPath()); err != nil {
		t.Fatalf("a clean shutdown must preserve durable transcript progress: %v", err)
	}
	if got := loadClaudeWatchProgress().Offsets[key]; got != size {
		t.Fatalf("offset after a clean shutdown = %d, want %d", got, size)
	}

	// Restart: fresh processors, same on-disk progress. Nothing already
	// processed may be read again.
	processors = map[string]*normalize.ClaudeTranscriptProcessor{}
	if parsed, consumed := pollClaudeTranscripts(session, ws, cutoff(), processors, true, false); consumed != 0 || parsed != 0 {
		t.Fatalf("restart replayed %d bytes / %d events of already-processed history", consumed, parsed)
	}

	// Live capture survives the restart: an appended turn is still picked up.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"live turn"}}`+"\n",
		ws, time.Now().UTC().Format(time.RFC3339))
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	parsed, consumed := pollClaudeTranscripts(session, ws, cutoff(), processors, true, false)
	if consumed != int64(len(line)) || parsed == 0 {
		t.Fatalf("live append after a restart parsed %d events over %d bytes; want the %d appended bytes", parsed, consumed, len(line))
	}
}

// TestCleanShutdownPreservesCodexBackfillProgress is the Codex half of the
// restart guarantee.
func TestCleanShutdownPreservesCodexBackfillProgress(t *testing.T) {
	root := codexSessionsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	dir := filepath.Join(root, "2026", "07", "22")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body := codexSessionMetaLine(ws, ts) +
		`{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"user_message","message":"history","images":[]}}` + "\n"
	path := filepath.Join(dir, "rollout-restart.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	size := int64(len(body))

	session := Session{DeviceID: "sess-codex-restart", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	cutoff := func() time.Time { return transcriptHistoryCutoff(time.Now().UTC()) }

	processors := map[string]*normalize.CodexRolloutProcessor{}
	if sent := pollCodexRollouts(session, ws, cutoff(), processors, false); sent == 0 {
		t.Fatal("first boot must backfill the in-window rollout")
	}
	if got := loadCodexWatchProgress().Offsets[path]; got != size {
		t.Fatalf("offset after the first boot = %d, want %d", got, size)
	}

	clearCodexWatcherState()

	if _, err := os.Stat(codexWatchProgressPath()); err != nil {
		t.Fatalf("a clean shutdown must preserve durable rollout progress: %v", err)
	}
	if got := loadCodexWatchProgress().Offsets[path]; got != size {
		t.Fatalf("offset after a clean shutdown = %d, want %d", got, size)
	}

	processors = map[string]*normalize.CodexRolloutProcessor{}
	if sent := pollCodexRollouts(session, ws, cutoff(), processors, false); sent != 0 {
		t.Fatalf("restart re-emitted %d already-processed event(s)", sent)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	live := `{"timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `","type":"event_msg","payload":{"type":"user_message","message":"live turn","images":[]}}` + "\n"
	if _, err := f.WriteString(live); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if sent := pollCodexRollouts(session, ws, cutoff(), processors, false); sent == 0 {
		t.Fatal("live append after a restart must still be captured")
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

// oversizedRecordLine builds a single JSONL record with no newline inside it,
// larger than n bytes — the shape no bounded poll can ever complete.
func oversizedRecordLine(n int) string {
	return `{"type":"user","blob":"` + strings.Repeat("x", n) + `"}` + "\n"
}

// TestPollClaudeDiscardsOversizedRecordAfterAnotherFileConsumed pins the escape
// hatch for an unsupported record against WALK ORDER, which is what made it
// unreachable in practice. It used to fire only while the stalled transcript was
// the first budget-consuming file of the poll (`budget == transcriptMaxRecordBytes`),
// so one live session a few kilobytes ahead of it in the walk kept the stalled
// file's offset pinned at the same byte for as long as that session stayed
// active — the exact "must not stall capture forever" case.
func TestPollClaudeDiscardsOversizedRecordAfterAnotherFileConsumed(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	prevMax, prevPoll := transcriptMaxRecordBytes, claudeWatchMaxBytesPerPoll
	transcriptMaxRecordBytes, claudeWatchMaxBytesPerPoll = 4096, 4096
	t.Cleanup(func() { transcriptMaxRecordBytes, claudeWatchMaxBytesPerPoll = prevMax, prevPoll })

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	ts := time.Now().UTC().Format(time.RFC3339)
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := func(text string) string {
		return fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n", ws, ts, text)
	}
	// The live session the walk reaches FIRST — it consumes budget every poll.
	live := filepath.Join(dir, "a-live.jsonl")
	if err := os.WriteFile(live, []byte(record("live turn")), 0o600); err != nil {
		t.Fatal(err)
	}
	// The stalled transcript: an unsupported record, then ordinary content that
	// must become reachable again once the record is discarded.
	huge := oversizedRecordLine(int(transcriptMaxRecordBytes) + 2048)
	stalled := filepath.Join(dir, "b-oversized.jsonl")
	if err := os.WriteFile(stalled, []byte(huge+record("reachable after the discard")), 0o600); err != nil {
		t.Fatal(err)
	}
	stalledKey := claudeProgressKey(stalled)
	stalledSize := int64(len(huge) + len(record("reachable after the discard")))
	saveClaudeWatchProgress(claudeWatchProgress{
		Offsets: map[string]int64{}, Discarding: map[string]bool{},
		ClassifyOffsets: map[string]int64{}, ClassifyDiscarding: map[string]bool{}, ClassifyScanned: map[string]int{},
		Match: map[string]string{claudeProgressKey(live): "yes", stalledKey: "yes"},
		V:     claudeProgressSchemaV, RootsFP: captureRootsFingerprint(workspaceMatchRoots(ws)),
	})

	session := Session{DeviceID: "sess-oversized", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}

	pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, true, false)
	if got := loadClaudeWatchProgress().Offsets[stalledKey]; got != transcriptMaxRecordBytes {
		t.Fatalf("oversized record left the offset at %d; want it advanced by %d even though an earlier file spent budget first",
			got, transcriptMaxRecordBytes)
	}

	// And the discard must actually unblock the file: the ordinary record after
	// it parses on a later poll.
	parsedAfter := 0
	for i := 0; i < 10 && loadClaudeWatchProgress().Offsets[stalledKey] < stalledSize; i++ {
		before := loadClaudeWatchProgress().Offsets[stalledKey]
		n, _ := pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, true, false)
		parsedAfter += n
		if loadClaudeWatchProgress().Offsets[stalledKey] == before {
			break
		}
	}
	if got := loadClaudeWatchProgress().Offsets[stalledKey]; got != stalledSize {
		t.Fatalf("stalled transcript drained to %d of %d bytes", got, stalledSize)
	}
	if parsedAfter == 0 {
		t.Fatal("content after the discarded record never parsed")
	}
}

// TestPollClaudeOversizedDiscardDoesNotDegradeParser pins the OTHER half: bytes
// no parser was ever offered must not count as evidence the parser is broken.
// The discard used to be reported as ordinary consumed bytes, and one discard
// exceeds claudeDegradedByteThreshold on its own — flipping the watcher to
// degraded, where the next poll parses and DROPS everything it reads while the
// hook rail that is supposed to cover that window is not driven from here.
func TestPollClaudeOversizedDiscardDoesNotDegradeParser(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	// Above claudeDegradedByteThreshold, so a discard reported as consumed
	// degrades the watcher in a single poll.
	prevMax, prevPoll := transcriptMaxRecordBytes, claudeWatchMaxBytesPerPoll
	transcriptMaxRecordBytes = claudeDegradedByteThreshold + 64*1024
	claudeWatchMaxBytesPerPoll = transcriptMaxRecordBytes
	t.Cleanup(func() { transcriptMaxRecordBytes, claudeWatchMaxBytesPerPoll = prevMax, prevPoll })

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	ts := time.Now().UTC().Format(time.RFC3339)
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Several chunks long: the regression only appeared on a later poll, when
	// the malformed suffix was mistaken for a fresh record. Restart between
	// chunks to prove the continuation bit is durable, not process-local.
	huge := oversizedRecordLine(int(3*transcriptMaxRecordBytes) + 2048)
	tail := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"after"}}`+"\n", ws, ts)
	path := filepath.Join(dir, "oversized.jsonl")
	if err := os.WriteFile(path, []byte(huge+tail), 0o600); err != nil {
		t.Fatal(err)
	}
	key := claudeProgressKey(path)
	saveClaudeWatchProgress(claudeWatchProgress{
		Offsets: map[string]int64{}, Discarding: map[string]bool{},
		ClassifyOffsets: map[string]int64{}, ClassifyDiscarding: map[string]bool{}, ClassifyScanned: map[string]int{},
		Match: map[string]string{key: "yes"}, V: claudeProgressSchemaV,
		RootsFP: captureRootsFingerprint(workspaceMatchRoots(ws)),
	})

	session := Session{DeviceID: "sess-degrade", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	parsed, consumed := pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()),
		map[string]*normalize.ClaudeTranscriptProcessor{}, true, false)
	if got := loadClaudeWatchProgress().Offsets[key]; got != transcriptMaxRecordBytes {
		t.Fatalf("discard did not durably advance the offset: %d, want %d", got, transcriptMaxRecordBytes)
	}
	if consumed != 0 {
		t.Fatalf("poll reported %d parser-visible bytes for a record no parser saw; want 0", consumed)
	}
	if degraded, _ := claudeDegradationStep(false, parsed, consumed, 0); degraded {
		t.Fatal("a single unsupported record degraded the watcher and handed the window to hooks")
	}

	parsed, consumed = pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()),
		map[string]*normalize.ClaudeTranscriptProcessor{}, true, false)
	if parsed != 0 || consumed != 0 {
		t.Fatalf("oversized suffix after restart reached the parser: parsed=%d consumed=%d", parsed, consumed)
	}
	if degraded, _ := claudeDegradationStep(false, parsed, consumed, 0); degraded {
		t.Fatal("an oversized suffix after restart degraded the parser")
	}
	if !loadClaudeWatchProgress().Discarding[key] {
		t.Fatal("oversized continuation was not preserved for the next bounded poll")
	}
}

// TestPollCodexDiscardsOversizedRecordAfterAnotherFileConsumed is the Codex half
// of the walk-order regression — the same escape hatch, the same gate, and the
// same stall.
func TestPollCodexDiscardsOversizedRecordAfterAnotherFileConsumed(t *testing.T) {
	root := codexSessionsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	prevMax, prevPoll := transcriptMaxRecordBytes, codexWatchMaxBytesPerPoll
	transcriptMaxRecordBytes, codexWatchMaxBytesPerPoll = 4096, 4096
	t.Cleanup(func() { transcriptMaxRecordBytes, codexWatchMaxBytesPerPoll = prevMax, prevPoll })

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	ts := time.Now().UTC().Format(time.RFC3339)
	dir := filepath.Join(root, "2026", "07", "20")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	userMsg := func(text string) string {
		return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":%q,"images":[]}}`+"\n", ts, text)
	}
	live := filepath.Join(dir, "rollout-a-live.jsonl")
	if err := os.WriteFile(live, []byte(codexSessionMetaLine(ws, ts)+userMsg("live turn")), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := codexSessionMetaLine(ws, ts)
	huge := oversizedRecordLine(int(transcriptMaxRecordBytes) + 2048)
	stalled := filepath.Join(dir, "rollout-b-oversized.jsonl")
	if err := os.WriteFile(stalled, []byte(meta+huge+userMsg("reachable after the discard")), 0o600); err != nil {
		t.Fatal(err)
	}

	session := Session{DeviceID: "sess-codex-oversized", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	processors := map[string]*normalize.CodexRolloutProcessor{}

	// Poll 1 consumes the header and stops at the unsupported record.
	pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false)
	if got := loadCodexWatchProgress().Offsets[stalled]; got != int64(len(meta)) {
		t.Fatalf("first poll advanced to %d, want the header only (%d)", got, len(meta))
	}

	// The live rollout grows, so poll 2 reaches the stalled one with a PARTIAL
	// budget — which is what the old equality gate could not survive.
	f, err := os.OpenFile(live, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(userMsg("another live turn")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false)
	want := int64(len(meta)) + transcriptMaxRecordBytes
	if got := loadCodexWatchProgress().Offsets[stalled]; got != want {
		t.Fatalf("oversized record left the offset at %d; want %d after the bounded discard", got, want)
	}

	total := int64(len(meta) + len(huge) + len(userMsg("reachable after the discard")))
	for i := 0; i < 10 && loadCodexWatchProgress().Offsets[stalled] < total; i++ {
		before := loadCodexWatchProgress().Offsets[stalled]
		pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false)
		if loadCodexWatchProgress().Offsets[stalled] == before {
			break
		}
	}
	if got := loadCodexWatchProgress().Offsets[stalled]; got != total {
		t.Fatalf("stalled rollout drained to %d of %d bytes", got, total)
	}
}

// TestClassifyClaudeTranscriptSkipsOversizedRecordAheadOfCwd pins the
// CLASSIFICATION half of the oversized-record escape.
//
// The escape lives in the tail path, and a transcript only reaches the tail once
// it is classified. So a record over the supported maximum sitting AHEAD of the
// first cwd-bearing line hit the classifier's own reader instead: it returned
// undecided, cached nothing, and the file was re-read from byte zero on every
// 3s poll forever — never tailed, so the escape never ran. Reproduced on the
// real binary with a 9,437,209-byte leading record: match uncached and offset 0
// across 30s of polling, 0 events captured.
//
// The skip must not widen what is captured: a cwd outside the watched roots
// reached PAST an unsupported record is still a definitive mismatch.
//
// Run at PRODUCTION transcriptMaxRecordBytes on purpose. The classifier used a
// scanner capped by its own 8 MiB literal rather than by the supported maximum,
// so a lowered maximum leaves that literal — and the bug — untouched: this test
// only reproduces against a record over the real 8 MiB.
func TestClassifyClaudeTranscriptSkipsOversizedRecordAheadOfCwd(t *testing.T) {
	ws := resolvePath(t.TempDir())
	outside := resolvePath(t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	huge := oversizedRecordLine(int(transcriptMaxRecordBytes) + 2048)

	write := func(name, cwd string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		body := huge + fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q}`+"\n", cwd, ts)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cutoff := transcriptHistoryCutoff(time.Now().UTC())

	if got := classifyClaudeTranscript(write("inside.jsonl", ws), []string{ws}, cutoff); got != claudeMatchYes {
		t.Fatalf("classify past an unsupported record = %v, want claudeMatchYes (%v) — the cwd line after it must not be lost",
			got, claudeMatchYes)
	}
	if got := classifyClaudeTranscript(write("outside.jsonl", outside), []string{ws}, cutoff); got != claudeMatchNo {
		t.Fatalf("classify past an unsupported record with an unwatched cwd = %v, want claudeMatchNo (%v)", got, claudeMatchNo)
	}
	// transcriptCwd feeds the repo identity of every prompt the newly-unblocked
	// transcript emits, so it has to clear the same record.
	if got := transcriptCwd(write("cwd.jsonl", ws)); got != ws {
		t.Fatalf("transcriptCwd past an unsupported record = %q, want %q", got, ws)
	}
}

// TestPollClaudeDrainsTranscriptLedByAnOversizedRecord is the real-path half:
// one poll must CACHE the decision (no re-read next poll) and the content behind
// the unsupported record must actually reach the parser. Production constants,
// for the reason given on the classification half.
func TestPollClaudeDrainsTranscriptLedByAnOversizedRecord(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	ts := time.Now().UTC().Format(time.RFC3339)
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tail := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"reachable after the discard"}}`+"\n", ws, ts)
	huge := oversizedRecordLine(int(transcriptMaxRecordBytes) + 2048)
	stalled := filepath.Join(dir, "head-oversized.jsonl")
	if err := os.WriteFile(stalled, []byte(huge+tail), 0o600); err != nil {
		t.Fatal(err)
	}
	key := claudeProgressKey(stalled)
	total := int64(len(huge) + len(tail))

	session := Session{DeviceID: "sess-head-oversized", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}

	pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, true, false)
	first := loadClaudeWatchProgress()
	if got := first.Match[key]; got != "" {
		t.Fatalf("first bounded classification unexpectedly decided %q", got)
	}
	if got := first.ClassifyOffsets[key]; got != transcriptMaxRecordBytes || !first.ClassifyDiscarding[key] {
		t.Fatalf("first bounded classification progress = (%d, %v), want (%d, true)",
			got, first.ClassifyDiscarding[key], transcriptMaxRecordBytes)
	}
	for i := 0; i < 4 && loadClaudeWatchProgress().Match[key] == ""; i++ {
		before := loadClaudeWatchProgress().ClassifyOffsets[key]
		pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, true, false)
		after := loadClaudeWatchProgress().ClassifyOffsets[key]
		if after-before > transcriptMaxRecordBytes {
			t.Fatalf("classification read %d bytes in one poll; max %d", after-before, transcriptMaxRecordBytes)
		}
	}
	if got := loadClaudeWatchProgress().Match[key]; got != "yes" {
		t.Fatalf("bounded classification never cached match: %q", got)
	}

	parsed := 0
	for i := 0; i < 10 && loadClaudeWatchProgress().Offsets[key] < total; i++ {
		before := loadClaudeWatchProgress().Offsets[key]
		n, _ := pollClaudeTranscripts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, true, false)
		parsed += n
		if loadClaudeWatchProgress().Offsets[key] == before {
			break
		}
	}
	if got := loadClaudeWatchProgress().Offsets[key]; got != total {
		t.Fatalf("transcript drained to %d of %d bytes", got, total)
	}
	if parsed == 0 {
		t.Fatal("content after the unsupported leading record never parsed")
	}
}

// TestClassifyCodexRolloutSkipsOversizedRecordAheadOfSessionMeta is the Codex
// half. session_meta is the ONLY rollout record carrying cwd, so an unsupported
// record ahead of it stalls classification exactly as it does on the Claude
// rail — and codexRolloutCwd, which supplies the session's repo identity, has
// to clear the same record or the rollout is admitted with no repo at all.
// Production constants, for the reason given on the Claude half.
func TestClassifyCodexRolloutSkipsOversizedRecordAheadOfSessionMeta(t *testing.T) {
	ws := resolvePath(t.TempDir())
	outside := resolvePath(t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	huge := oversizedRecordLine(int(transcriptMaxRecordBytes) + 2048)

	write := func(name, cwd string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(huge+codexSessionMetaLine(cwd, ts)), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cutoff := transcriptHistoryCutoff(time.Now().UTC())

	if got := classifyCodexRollout(write("rollout-inside.jsonl", ws), []string{ws}, cutoff); got != codexMatchYes {
		t.Fatalf("classify past an unsupported record = %v, want codexMatchYes (%v) — the session_meta after it must not be lost",
			got, codexMatchYes)
	}
	if got := classifyCodexRollout(write("rollout-outside.jsonl", outside), []string{ws}, cutoff); got != codexMatchNo {
		t.Fatalf("classify past an unsupported record with an unwatched cwd = %v, want codexMatchNo (%v)", got, codexMatchNo)
	}
	if got := codexRolloutCwd(write("rollout-cwd.jsonl", ws)); got != ws {
		t.Fatalf("codexRolloutCwd past an unsupported record = %q, want %q", got, ws)
	}
}

// TestPollCodexDrainsRolloutLedByAnOversizedRecord is the Codex real-path half.
func TestPollCodexDrainsRolloutLedByAnOversizedRecord(t *testing.T) {
	root := codexSessionsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	ws := resolvePath(workspace)
	ts := time.Now().UTC().Format(time.RFC3339)
	dir := filepath.Join(root, "2026", "07", "20")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	huge := oversizedRecordLine(int(transcriptMaxRecordBytes) + 2048)
	meta := codexSessionMetaLine(ws, ts)
	userMsg := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":"reachable after the discard","images":[]}}`+"\n", ts)
	stalled := filepath.Join(dir, "rollout-head-oversized.jsonl")
	if err := os.WriteFile(stalled, []byte(huge+meta+userMsg), 0o600); err != nil {
		t.Fatal(err)
	}
	total := int64(len(huge) + len(meta) + len(userMsg))

	session := Session{DeviceID: "sess-codex-head-oversized", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	processors := map[string]*normalize.CodexRolloutProcessor{}

	sent := pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false)
	first := loadCodexWatchProgress()
	if got := first.Match[stalled]; got != "" {
		t.Fatalf("first bounded classification unexpectedly decided %q", got)
	}
	if got := first.ClassifyOffsets[stalled]; got != transcriptMaxRecordBytes || !first.ClassifyDiscarding[stalled] {
		t.Fatalf("first bounded classification progress = (%d, %v), want (%d, true)",
			got, first.ClassifyDiscarding[stalled], transcriptMaxRecordBytes)
	}
	for i := 0; i < 4 && loadCodexWatchProgress().Match[stalled] == ""; i++ {
		before := loadCodexWatchProgress().ClassifyOffsets[stalled]
		sent += pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false)
		after := loadCodexWatchProgress().ClassifyOffsets[stalled]
		if after-before > transcriptMaxRecordBytes {
			t.Fatalf("classification read %d bytes in one poll; max %d", after-before, transcriptMaxRecordBytes)
		}
	}
	if got := loadCodexWatchProgress().Match[stalled]; got != "yes" {
		t.Fatalf("bounded classification never cached match: %q", got)
	}

	for i := 0; i < 10 && loadCodexWatchProgress().Offsets[stalled] < total; i++ {
		before := loadCodexWatchProgress().Offsets[stalled]
		sent += pollCodexRollouts(session, ws, transcriptHistoryCutoff(time.Now().UTC()), processors, false)
		if loadCodexWatchProgress().Offsets[stalled] == before {
			break
		}
	}
	if got := loadCodexWatchProgress().Offsets[stalled]; got != total {
		t.Fatalf("rollout drained to %d of %d bytes", got, total)
	}
	if sent == 0 {
		t.Fatal("content after the unsupported leading record never reached the parser")
	}
}
