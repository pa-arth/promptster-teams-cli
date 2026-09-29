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
	if err != nil || r.State != "partial" || r.CappedFiles != 2 || r.SkippedFiles != 1 || r.TracedLines != 19 {
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
