package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// statMtimeMs returns a file's on-disk mtime in Unix ms — read back after any
// os.Chtimes so windows are placed against the value the reconciler will actually
// stat, immune to filesystem mtime-granularity truncation.
func statMtimeMs(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().UnixMilli()
}

// writeFile writes content to <dir>/<rel>, creating parents.
func writeCommitFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// filesByPath indexes an event's projected files array by path for assertions.
func filesByPath(t *testing.T, ev event.Event) map[string]map[string]interface{} {
	t.Helper()
	data, ok := ev.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("event Data is %T, want map", ev.Data)
	}
	arr, ok := data["files"].([]interface{})
	if !ok {
		t.Fatalf("files is %T, want []interface{}: %+v", data["files"], data["files"])
	}
	out := map[string]map[string]interface{}{}
	for _, f := range arr {
		fm := f.(map[string]interface{})
		out[fm["path"].(string)] = fm
	}
	return out
}

// TestCommitAttributionHappyPath: an AI-touched file committed → its committed
// @@ span is emitted as likely_ai, the commitSha matches, and the event kind is
// commit_attribution.
func TestCommitAttributionHappyPath(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	writeCommitFile(t, ws, "foo.go", "package main\n\nfunc main() {}\n")
	git("add", "-A")
	git("commit", "-m", "add foo")
	sha := gitOut("rev-parse", "HEAD")

	// The AI channel recorded foo.go for this session (repo-relative POSIX path),
	// scoped to this workspace's root key.
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(ws), "foo.go")

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("expected an emittable event for a commit with changed files")
	}
	if ev.Kind != "commit_attribution" {
		t.Fatalf("kind = %q, want commit_attribution", ev.Kind)
	}
	if ev.SessionID != "ai-sess-1" {
		t.Errorf("sessionId = %q, want the AI session that touched the commit", ev.SessionID)
	}
	data := ev.Data.(map[string]interface{})
	if data["commitSha"] != sha {
		t.Errorf("commitSha = %v, want %s", data["commitSha"], sha)
	}
	files := filesByPath(t, ev)
	foo, present := files["foo.go"]
	if !present {
		t.Fatalf("foo.go missing from files: %+v", files)
	}
	ranges := foo["lineRanges"].([]interface{})
	if len(ranges) == 0 {
		t.Fatal("foo.go has no lineRanges")
	}
	for _, r := range ranges {
		rm := r.(map[string]interface{})
		if rm["attribution"] != attributionLikelyAI {
			t.Errorf("range attribution = %v, want likely_ai: %+v", rm["attribution"], rm)
		}
	}
	// The new file is 3 lines, so the committed new-side span is 1..3.
	first := ranges[0].(map[string]interface{})
	if first["start"] != float64(1) || first["end"] != float64(3) {
		t.Errorf("committed span = %v..%v, want 1..3", first["start"], first["end"])
	}
}

// TestCommitAttributionUnknownNeverHuman: a file with NO AI evidence is marked
// unknown, and the string "human" appears NOWHERE in the emitted event.
func TestCommitAttributionUnknownNeverHuman(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	writeCommitFile(t, ws, "hand.go", "package main\n\nvar x = 1\n")
	git("add", "-A")
	git("commit", "-m", "human edit, no AI evidence")
	sha := gitOut("rev-parse", "HEAD")

	// Deliberately record NOTHING in the AI-paths ledger.

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	// No AI session touched it → sessionId falls back to the device id.
	if ev.SessionID != "dev-x" {
		t.Errorf("sessionId = %q, want device fallback dev-x", ev.SessionID)
	}
	files := filesByPath(t, ev)
	hand := files["hand.go"]
	for _, r := range hand["lineRanges"].([]interface{}) {
		if r.(map[string]interface{})["attribution"] != attributionUnknown {
			t.Errorf("not-AI residue must be unknown, got %+v", r)
		}
	}
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), "human") {
		t.Errorf(`"human" must never appear in a commit_attribution event: %s`, b)
	}
}

// TestCommitAttributionFormatterRobust: attribution lands on the SPANS THAT
// ACTUALLY LANDED in the commit (parsed from git diff), not on any transcript
// range. We simulate a formatter reflow by committing content whose changed
// lines sit at positions a transcript-range approach would not predict, and
// assert the emitted ranges equal the committed @@ new-side spans.
func TestCommitAttributionFormatterRobust(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	// Base commit (pre-existing, watcher would have baselined past it).
	writeCommitFile(t, ws, "app.go", "line1\nline2\nline3\n")
	git("add", "-A")
	git("commit", "-m", "base")

	// AI edits app.go; the ledger records the path. The "reflow": the change
	// lands as two NEW lines appended at the end (new-side lines 4-5), which no
	// pre-formatter transcript range for the middle of the file would predict.
	recordAiTouchedPath("ai-sess-2", gitWatchRootKey(ws), "app.go")
	writeCommitFile(t, ws, "app.go", "line1\nline2\nline3\nnew4\nnew5\n")
	git("add", "-A")
	git("commit", "-m", "ai appends, reflowed")
	sha := gitOut("rev-parse", "HEAD")

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	files := filesByPath(t, ev)
	app := files["app.go"]
	ranges := app["lineRanges"].([]interface{})
	if len(ranges) != 1 {
		t.Fatalf("want one committed hunk, got %d: %+v", len(ranges), ranges)
	}
	got := ranges[0].(map[string]interface{})
	// The committed diff added new-side lines 4..5 — reconciled from git, not
	// from any transcript range.
	if got["start"] != float64(4) || got["end"] != float64(5) {
		t.Errorf("span = %v..%v, want committed 4..5", got["start"], got["end"])
	}
	if got["attribution"] != attributionLikelyAI {
		t.Errorf("attribution = %v, want likely_ai", got["attribution"])
	}
}

// TestCommitAttributionFirstCommitAgainstEmptyTree: a parentless first commit is
// diffed against the empty tree (via `git show --root`), so its files are still
// attributed rather than skipped.
func TestCommitAttributionFirstCommitAgainstEmptyTree(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	writeCommitFile(t, ws, "root.go", "a\nb\n")
	git("add", "-A")
	git("commit", "-m", "the very first commit")
	sha := gitOut("rev-parse", "HEAD")

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("first commit must still be attributable (diffed vs empty tree)")
	}
	files := filesByPath(t, ev)
	if _, present := files["root.go"]; !present {
		t.Fatalf("root.go missing from first-commit attribution: %+v", files)
	}
}

// TestCommitAttributionSurvivesEmitPath is the guard against the []interface{}
// type trap: the FULL emit path (eventDataMap round-trip → sign/redact funnel →
// outbox) must yield a commit_attribution whose files/lineRanges are NON-EMPTY
// with real integer spans. A struct-to-Data bug or a projector that dropped the
// nested arrays would ship {} and this fails.
func TestCommitAttributionSurvivesEmitPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))

	// Sign so the queued bytes are the real projected/signed wire bytes.
	if _, err := sign.GenerateSessionKeypair(); err != nil {
		t.Fatal(err)
	}

	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "svc/handler.go", "package svc\n\nfunc H() int { return 1 }\n")
	git("add", "-A")
	git("commit", "-m", "ai handler")
	sha := gitOut("rev-parse", "HEAD")
	recordAiTouchedPath("ai-sess-3", gitWatchRootKey(ws), "svc/handler.go")

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-emit", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	emitCommitAttribution(ev)

	// Read the exact bytes that were queued for the wire.
	queued, err := os.ReadFile(state.OutboxPath())
	if err != nil {
		t.Fatal(err)
	}
	line := queued
	if i := strings.IndexByte(string(queued), '\n'); i >= 0 {
		line = queued[:i]
	}
	var got event.Event
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("unmarshal queued event: %v", err)
	}
	if got.Kind != "commit_attribution" {
		t.Fatalf("queued kind = %q, want commit_attribution", got.Kind)
	}
	if got.Sig == "" {
		t.Error("queued event is unsigned — it did not go through the sign funnel")
	}

	data, ok := got.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("queued Data is %T, want map — the payload was stripped on the emit path", got.Data)
	}
	if data["commitSha"] != sha {
		t.Errorf("commitSha = %v, want %s", data["commitSha"], sha)
	}
	if data["workspaceKey"] == nil || data["workspaceKey"] == "" {
		t.Error("workspaceKey missing from queued payload")
	}
	files, ok := data["files"].([]interface{})
	if !ok || len(files) == 0 {
		t.Fatalf("files empty on the wire (the []interface{} trap): %#v", data["files"])
	}
	file := files[0].(map[string]interface{})
	if file["path"] != "svc/handler.go" {
		t.Errorf("file path = %v, want svc/handler.go", file["path"])
	}
	ranges, ok := file["lineRanges"].([]interface{})
	if !ok || len(ranges) == 0 {
		t.Fatalf("lineRanges empty on the wire: %#v", file["lineRanges"])
	}
	r := ranges[0].(map[string]interface{})
	// After the JSON round-trip through the wire, ints arrive as float64.
	if _, isNum := r["start"].(float64); !isNum {
		t.Errorf("start is %T, want a number: %+v", r["start"], r)
	}
	if r["attribution"] != attributionLikelyAI {
		t.Errorf("attribution = %v, want likely_ai", r["attribution"])
	}
	// Only the three scalar keys survive the nested element allowlist.
	if len(r) != 3 {
		t.Errorf("lineRange has %d keys, want exactly {start,end,attribution}: %+v", len(r), r)
	}
}

// TestCommitAttributionCrossWorkspaceNoBleed: an AI path recorded under
// workspace A's root key must NOT attribute a same-named file committed in a
// DIFFERENT repo B. The workspace scoping (readAiTouchedPaths(rootKey)) is what
// prevents the cross-repo over-attribution.
func TestCommitAttributionCrossWorkspaceNoBleed(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	// Workspace A: AI touched shared.go here.
	wsA, _, _ := gitRepo(t)
	recordAiTouchedPath("ai-sess-A", gitWatchRootKey(wsA), "shared.go")

	// Workspace B: a DIFFERENT repo commits a same-named shared.go.
	wsB, gitB, gitBOut := gitRepo(t)
	writeCommitFile(t, wsB, "shared.go", "package main\n\nvar y = 2\n")
	gitB("add", "-A")
	gitB("commit", "-m", "human edit in repo B")
	shaB := gitBOut("rev-parse", "HEAD")

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-b", TaskRoot: wsB}, wsB, shaB)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	files := filesByPath(t, ev)
	shared := files["shared.go"]
	for _, r := range shared["lineRanges"].([]interface{}) {
		if r.(map[string]interface{})["attribution"] != attributionUnknown {
			t.Errorf("cross-workspace shared.go must be unknown, not bled likely_ai: %+v", r)
		}
	}
}

// TestCommitAttributionNonASCIIPath: a committed file with a non-ASCII name that
// IS in the ai-paths ledger must reconcile as likely_ai. Without
// core.quotePath=false git would C-quote the path in the diff header, and it
// would never match the ledger's UTF-8 key (falling to unknown).
func TestCommitAttributionNonASCIIPath(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	writeCommitFile(t, ws, "café.go", "package main\n\nvar z = 3\n")
	git("add", "-A")
	git("commit", "-m", "ai edit, non-ascii path")
	sha := gitOut("rev-parse", "HEAD")

	recordAiTouchedPath("ai-sess-utf8", gitWatchRootKey(ws), "café.go")

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	files := filesByPath(t, ev)
	cafe, present := files["café.go"]
	if !present {
		t.Fatalf("café.go missing from files (path likely C-quoted): %+v", files)
	}
	ranges := cafe["lineRanges"].([]interface{})
	if len(ranges) == 0 {
		t.Fatal("café.go has no lineRanges")
	}
	for _, r := range ranges {
		if r.(map[string]interface{})["attribution"] != attributionLikelyAI {
			t.Errorf("non-ASCII AI-touched path must be likely_ai: %+v", r)
		}
	}
}

// TestBashRecoveryHappyPath: a committed file with NO ai-paths evidence
// (simulating an AI `sed -i`/codegen edit that produced no file_diff) whose
// working-tree mtime falls inside a recorded AI bash window is recovered from
// unknown to likely_ai, tagged with that bash session.
func TestBashRecoveryHappyPath(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	writeCommitFile(t, ws, "gen.go", "package main\n\nvar Gen = 1\n")
	git("add", "-A")
	git("commit", "-m", "bash-generated file")
	sha := gitOut("rev-parse", "HEAD")

	// Deliberately record NOTHING in the ai-paths ledger for gen.go — a bash edit
	// produces no file_diff, so its path never enters that ledger.
	base := time.Now()
	if err := os.Chtimes(filepath.Join(ws, "gen.go"), base, base); err != nil {
		t.Fatal(err)
	}
	m := statMtimeMs(t, filepath.Join(ws, "gen.go"))
	recordBashWindow("bash-sess", gitWatchRootKey(ws), m-500, m+500) // window spans the file mtime

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	files := filesByPath(t, ev)
	gen := files["gen.go"]
	for _, r := range gen["lineRanges"].([]interface{}) {
		if r.(map[string]interface{})["attribution"] != attributionLikelyAI {
			t.Errorf("bash-recovered file must be likely_ai, got %+v", r)
		}
	}
	// The recovered bash session is the commit's representative AI session.
	if ev.SessionID != "bash-sess" {
		t.Errorf("sessionId = %q, want recovered bash session bash-sess", ev.SessionID)
	}
}

// TestBashRecoveryOutsideWindowStaysUnknown: same setup, but the recorded bash
// window is far from the file mtime (beyond δ/ε) → the file stays unknown and
// "human" appears nowhere.
func TestBashRecoveryOutsideWindowStaysUnknown(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	writeCommitFile(t, ws, "gen.go", "package main\n\nvar Gen = 1\n")
	git("add", "-A")
	git("commit", "-m", "not-AI file")
	sha := gitOut("rev-parse", "HEAD")

	base := time.Now()
	if err := os.Chtimes(filepath.Join(ws, "gen.go"), base, base); err != nil {
		t.Fatal(err)
	}
	m := statMtimeMs(t, filepath.Join(ws, "gen.go"))
	// A window a full minute before the file mtime — well outside the 3s tolerance.
	recordBashWindow("bash-sess", gitWatchRootKey(ws), m-60000, m-59000)

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	files := filesByPath(t, ev)
	for _, r := range files["gen.go"]["lineRanges"].([]interface{}) {
		if r.(map[string]interface{})["attribution"] != attributionUnknown {
			t.Errorf("file outside any bash window must stay unknown, got %+v", r)
		}
	}
	if ev.SessionID != "dev-x" {
		t.Errorf("sessionId = %q, want device fallback dev-x (no AI evidence)", ev.SessionID)
	}
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), "human") {
		t.Errorf(`"human" must never appear in a commit_attribution event: %s`, b)
	}
}

// TestBashRecoveryNeverOverridesPrimaryAI: a file already likely_ai from the
// ai-paths ledger is NOT re-tagged by the recovery pass even when a bash window
// also matches — recovery fills holes only, and the primary session wins.
func TestBashRecoveryNeverOverridesPrimaryAI(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	writeCommitFile(t, ws, "gen.go", "package main\n\nvar Gen = 1\n")
	git("add", "-A")
	git("commit", "-m", "ai file with a matching bash window too")
	sha := gitOut("rev-parse", "HEAD")

	// Primary AI evidence for gen.go.
	recordAiTouchedPath("ai-sess", gitWatchRootKey(ws), "gen.go")

	base := time.Now()
	if err := os.Chtimes(filepath.Join(ws, "gen.go"), base, base); err != nil {
		t.Fatal(err)
	}
	m := statMtimeMs(t, filepath.Join(ws, "gen.go"))
	recordBashWindow("bash-sess", gitWatchRootKey(ws), m-500, m+500) // also matches, must be ignored

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	files := filesByPath(t, ev)
	for _, r := range files["gen.go"]["lineRanges"].([]interface{}) {
		if r.(map[string]interface{})["attribution"] != attributionLikelyAI {
			t.Errorf("primary AI file must stay likely_ai, got %+v", r)
		}
	}
	// The primary ai-paths session, not the bash session, represents the commit.
	if ev.SessionID != "ai-sess" {
		t.Errorf("sessionId = %q, want primary ai-sess (recovery must not seize it)", ev.SessionID)
	}
}

// TestBashRecoveryNearestWindowChosen: two windows both within tolerance of the
// file mtime; the nearer one's session must win the tie-break.
func TestBashRecoveryNearestWindowChosen(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	writeCommitFile(t, ws, "gen.go", "package main\n\nvar Gen = 1\n")
	git("add", "-A")
	git("commit", "-m", "bash file, two candidate windows")
	sha := gitOut("rev-parse", "HEAD")

	base := time.Now()
	if err := os.Chtimes(filepath.Join(ws, "gen.go"), base, base); err != nil {
		t.Fatal(err)
	}
	m := statMtimeMs(t, filepath.Join(ws, "gen.go"))
	// near-sess ends 100ms before the mtime (distance 100); far-sess starts
	// 2000ms after it (distance 2000). Both are within the 3s tolerance.
	recordBashWindow("near-sess", gitWatchRootKey(ws), m-200, m-100)
	recordBashWindow("far-sess", gitWatchRootKey(ws), m+2000, m+2500)

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	files := filesByPath(t, ev)
	for _, r := range files["gen.go"]["lineRanges"].([]interface{}) {
		if r.(map[string]interface{})["attribution"] != attributionLikelyAI {
			t.Errorf("recovered file must be likely_ai, got %+v", r)
		}
	}
	if ev.SessionID != "near-sess" {
		t.Errorf("sessionId = %q, want nearest window session near-sess", ev.SessionID)
	}
}

// TestParseUnifiedDiffNewRanges pins the diff parser against real git output
// forms: multi-hunk, adds, and a deletion (which contributes no new-side range).
func TestParseUnifiedDiffNewRanges(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/foo.go b/foo.go",
		"index de98044..a7bc997 100644",
		"--- a/foo.go",
		"+++ b/foo.go",
		"@@ -2 +2 @@ ctx",
		"-b",
		"+B",
		"@@ -3,0 +4 @@ ctx",
		"+d",
		"diff --git a/gone.txt b/gone.txt",
		"deleted file mode 100644",
		"index 587be6b..0000000",
		"--- a/gone.txt",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-x",
	}, "\n")

	got := parseUnifiedDiffNewRanges(diff)
	foo := got["foo.go"]
	if len(foo) != 2 {
		t.Fatalf("foo.go ranges = %+v, want 2", foo)
	}
	if foo[0].Start != 2 || foo[0].End != 2 {
		t.Errorf("hunk 1 = %+v, want 2..2", foo[0])
	}
	if foo[1].Start != 4 || foo[1].End != 4 {
		t.Errorf("hunk 2 = %+v, want 4..4", foo[1])
	}
	if _, present := got["gone.txt"]; present {
		t.Errorf("a deletion must contribute no new-side range, got %+v", got["gone.txt"])
	}
}
