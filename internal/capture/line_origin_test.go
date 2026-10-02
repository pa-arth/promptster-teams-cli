package capture

import (
	"encoding/json"
	"fmt"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func originTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	b, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %s", args, b)
	}
	return strings.TrimSpace(string(b))
}
func originRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	originTestGit(t, dir, "init")
	originTestGit(t, dir, "config", "user.name", "Fixture")
	originTestGit(t, dir, "config", "user.email", "fixture@example.invalid")
	return dir
}
func originWrite(t *testing.T, dir, path, text string) {
	t.Helper()
	if e := os.WriteFile(filepath.Join(dir, path), []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
}
func originCommit(t *testing.T, dir string) string {
	t.Helper()
	originTestGit(t, dir, "add", "-A")
	originTestGit(t, dir, "commit", "-m", "fixture")
	return originTestGit(t, dir, "rev-parse", "HEAD")
}
func TestOriginChangedDeletedAndRenamedLines(t *testing.T) {
	dir := originRepo(t)
	originWrite(t, dir, "space file.txt", "SOURCE_CANARY\nb\nc\nd\ne\nf\ng\nh\ni\nj\n")
	intro := originCommit(t, dir)
	root, err := AnalyzeLineOrigin(dir, intro)
	if err != nil || root.State != "measured" || root.ReplacedLines != 0 {
		t.Fatalf("root: %+v %v", root, err)
	}
	originTestGit(t, dir, "mv", "space file.txt", "renamed\tfile.txt")
	originWrite(t, dir, "renamed\tfile.txt", "changed\nb\nc\nd\ne\nf\ng\nh\ni\nj\n")
	fix := originCommit(t, dir)
	r, err := AnalyzeLineOrigin(dir, fix)
	if err != nil || r.State != "measured" || r.ReplacedLines != 1 || r.TracedLines != 1 || len(r.ReplacedFrom) != 1 || r.ReplacedFrom[0].Sha != intro {
		t.Fatalf("rename: %+v %v", r, err)
	}
	os.Remove(filepath.Join(dir, "renamed\tfile.txt"))
	deleted := originCommit(t, dir)
	r, err = AnalyzeLineOrigin(dir, deleted)
	if err != nil || r.TracedLines != 10 || r.ReplacedLines != 10 {
		t.Fatalf("delete: %+v %v", r, err)
	}
	e := lineOriginEvent(Session{DeviceID: "fixture"}, r)
	redact.ProjectEvent(&e, false)
	b, _ := json.Marshal(e)
	for _, canary := range []string{"SOURCE_CANARY", "space file", "renamed", "fixture@example"} {
		if strings.Contains(string(b), canary) {
			t.Fatalf("leak: %s", b)
		}
	}
	if !strings.Contains(string(b), intro) {
		t.Fatalf("projector dropped origins: %s", b)
	}
}
func TestOriginShallowBoundaryIsUnavailable(t *testing.T) {
	dir := originRepo(t)
	originWrite(t, dir, "a", "old\n")
	originCommit(t, dir)
	originWrite(t, dir, "a", "new\n")
	sha := originCommit(t, dir)
	clone := filepath.Join(t.TempDir(), "clone")
	originTestGit(t, dir, "clone", "--depth=1", "file://"+dir, clone)
	r, err := AnalyzeLineOrigin(clone, sha)
	if err != nil || r.State != "unavailable" || r.TracedLines != 0 {
		t.Fatalf("shallow: %+v %v", r, err)
	}
}
func TestOriginBackfillDoesNotCreateHistoricalAI(t *testing.T) {
	dir := originRepo(t)
	originWrite(t, dir, "a", "old\n")
	intro := originCommit(t, dir)
	originWrite(t, dir, "a", "new\n")
	originCommit(t, dir)
	receipts, err := BackfillLineOrigins(Session{}, dir, 2, true)
	if err != nil || len(receipts) != 2 || receipts[0].ReplacedFrom[0].Sha != intro {
		t.Fatalf("backfill: %+v %v", receipts, err)
	}
	if _, err := BackfillLineOrigins(Session{}, dir, 501, true); err == nil {
		t.Fatal("unbounded replay")
	}
}

func TestOriginMergeUsesNetFirstParentFix(t *testing.T) {
	dir := originRepo(t)
	originWrite(t, dir, "a", "bad\n")
	intro := originCommit(t, dir)
	branch := originTestGit(t, dir, "branch", "--show-current")
	originTestGit(t, dir, "checkout", "-b", "fix")
	originWrite(t, dir, "a", "good\n")
	originCommit(t, dir)
	originTestGit(t, dir, "checkout", branch)
	originTestGit(t, dir, "merge", "--no-ff", "fix", "-m", "merge fix")
	sha := originTestGit(t, dir, "rev-parse", "HEAD")
	r, err := AnalyzeLineOrigin(dir, sha)
	if err != nil || r.TracedLines != 1 || r.ReplacedFrom[0].Sha != intro {
		t.Fatalf("merge: %+v %v", r, err)
	}
}
func TestOriginBinaryAndFileCapStayPartial(t *testing.T) {
	dir := originRepo(t)
	for i := 0; i < 21; i++ {
		originWrite(t, dir, fmt.Sprintf("f%02d", i), "old\n")
	}
	originWrite(t, dir, "binary", string([]byte{0, 1, 2}))
	originCommit(t, dir)
	for i := 0; i < 21; i++ {
		originWrite(t, dir, fmt.Sprintf("f%02d", i), "new\n")
	}
	originWrite(t, dir, "binary", string([]byte{0, 2, 3}))
	sha := originCommit(t, dir)
	r, err := AnalyzeLineOrigin(dir, sha)
	if err != nil || r.State != "partial" || r.CappedFiles != 1 || r.SkippedFiles != 1 || r.TracedLines != 20 {
		t.Fatalf("bounded: %+v %v", r, err)
	}
}
func TestOriginReceiptProjectionRejectsNestedSource(t *testing.T) {
	e := lineOriginEvent(Session{DeviceID: "fixture"}, LineOrigin{})
	e.Data = map[string]interface{}{"commitSha": strings.Repeat("a", 40), "replacedFrom": []interface{}{map[string]interface{}{"sha": strings.Repeat("b", 40), "lines": 1, "text": "SOURCE_CANARY"}}, "text": "SOURCE_CANARY"}
	redact.ProjectEvent(&e, false)
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "SOURCE_CANARY") {
		t.Fatalf("leak: %s", b)
	}
}

func TestOriginRemoteIdentityMatchesAcrossClonesAndEvents(t *testing.T) {
	dir := originRepo(t)
	originTestGit(t, dir, "remote", "add", "origin", "https://example.invalid/team/repo+private.git")
	originWrite(t, dir, "a", "old\n")
	sha := originCommit(t, dir)
	r, err := AnalyzeLineOrigin(dir, sha)
	if err != nil || r.WorkspaceKey != "team/repo+private" || r.WorkspaceKey != workspaceKey(dir) {
		t.Fatalf("workspace: %+v %v", r, err)
	}
	e := lineOriginEvent(Session{}, r)
	redact.ProjectEvent(&e, false)
	if e.Data.(map[string]interface{})["commitSha"] != sha {
		t.Fatal("valid receipt dropped")
	}
	clone := filepath.Join(t.TempDir(), "clone")
	originTestGit(t, dir, "clone", dir, clone)
	for _, remote := range []string{"https://example.invalid/team/repo+private.git", "https://example.invalid/team/repo:private.git"} {
		originTestGit(t, dir, "remote", "set-url", "origin", remote)
		originTestGit(t, clone, "remote", "set-url", "origin", remote)
		expected := workspaceKey(dir)
		if workspaceKey(clone) != expected || sessionRepoRoot(dir) != expected || sessionRepoRoot(clone) != expected {
			t.Fatal("identity fragmented across events or checkouts")
		}
		r, err := AnalyzeLineOrigin(clone, sha)
		if err != nil || r.WorkspaceKey != expected {
			t.Fatal("receipt identity fragmented", err)
		}
		e := lineOriginEvent(Session{}, r)
		redact.ProjectEvent(&e, false)
		if e.Data.(map[string]interface{})["workspaceKey"] != expected {
			t.Fatal("fallback projection dropped identity")
		}
	}
}

func TestOriginModeOnlyChangesDoNotConsumeTraceCap(t *testing.T) {
	dir := originRepo(t)
	for i := 0; i < 20; i++ {
		originWrite(t, dir, fmt.Sprintf("a%02d", i), "same\n")
	}
	originWrite(t, dir, "z", "old\n")
	intro := originCommit(t, dir)
	for i := 0; i < 20; i++ {
		if err := os.Chmod(filepath.Join(dir, fmt.Sprintf("a%02d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	originWrite(t, dir, "z", "new\n")
	sha := originCommit(t, dir)
	r, err := AnalyzeLineOrigin(dir, sha)
	if err != nil || r.CappedFiles != 0 || r.TracedLines != 1 || r.ReplacedFrom[0].Sha != intro {
		t.Fatalf("mode cap: %+v %v", r, err)
	}
}

func TestOriginPendingSurvivesFailureAndRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PROMPTSTER_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(home, "buffer.jsonl"))
	dir := originRepo(t)
	originWrite(t, dir, "a", "old\n")
	sha := originCommit(t, dir)
	// Outbox failure must not consume the persisted job or mark it handled.
	blocked := filepath.Join(home, "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(blocked, "outbox.jsonl"))
	if !requestLineOrigin(Session{DeviceID: "fixture"}, dir, sha, 1) {
		t.Fatal("enqueue failed")
	}
	if _, err := os.Stat(filepath.Join(home, "buffer.jsonl")); !os.IsNotExist(err) {
		t.Fatal("enqueue ran trace/signing on polling goroutine")
	}
	drainLineOriginPending()
	jobs, err := os.ReadDir(lineOriginPendingDir())
	if err != nil || len(jobs) != 1 {
		t.Fatal("failed queue lost pending job", err)
	}
	// A new worker invocation uses only the persisted job after delivery recovers.
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(home, "outbox.jsonl"))
	drainLineOriginPending()
	jobs, err = os.ReadDir(lineOriginPendingDir())
	if err != nil || len(jobs) != 0 {
		t.Fatal("successful retry did not drain", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "buffer.jsonl"))
	if err != nil || !strings.Contains(string(data), sha) {
		t.Fatal("retry did not sign receipt", err)
	}
}

func TestOriginPublishFailurePreservesWorkingCursor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PROMPTSTER_STATE_DIR", filepath.Join(home, "state"))
	dir := originRepo(t)
	originWrite(t, dir, "a", "old\n")
	intro := originCommit(t, dir)
	pollGitWatch([]string{dir}, Session{DeviceID: "fixture"})
	originWrite(t, dir, "a", "new\n")
	fix := originCommit(t, dir)
	pending := lineOriginPendingDir()
	if err := os.WriteFile(pending, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	detected, _, _, _ := pollGitWatch([]string{dir}, Session{DeviceID: "fixture"})
	if len(detected) != 0 || loadGitWatchCursors()[gitWatchRootKey(dir)] != intro {
		t.Fatal("failed publish advanced cursor")
	}
	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	detected, _, _, _ = pollGitWatch([]string{dir}, Session{DeviceID: "fixture"})
	if len(detected[gitWatchRootKey(dir)]) != 1 || loadGitWatchCursors()[gitWatchRootKey(dir)] != fix {
		t.Fatal("recovered publication did not advance")
	}
	jobs, err := os.ReadDir(pending)
	if err != nil || len(jobs) != 1 {
		t.Fatal("advance has no durable pending work", err)
	}
}
