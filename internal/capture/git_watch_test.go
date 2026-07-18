package capture

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gitRepo spins up an isolated git repo under a temp dir and returns a closure
// that runs git in it (failing the test on error) plus one that returns trimmed
// stdout. Mirrors the harness in census_test.go.
func gitRepo(t *testing.T) (dir string, run func(args ...string), out func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir = t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	run = func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, o)
		}
	}
	out = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		o, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(o))
	}
	run("init")
	return dir, run, out
}

// TestGitWatchDetectsNewCommits: cold start baselines silently, a later commit
// is reported exactly once (not the baseline commit), and a steady-state poll
// reports nothing.
func TestGitWatchDetectsNewCommits(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	git("commit", "--allow-empty", "-m", "A")

	roots := []string{ws}
	key := gitWatchRootKey(ws)

	// Cold start: record HEAD as baseline, report no new commits.
	if d := pollGitWatch(roots); len(d) != 0 {
		t.Fatalf("cold start must report no commits, got %v", d)
	}

	// New commit B → reported exactly, and only B (not the baseline A).
	git("commit", "--allow-empty", "-m", "B")
	headB := gitOut("rev-parse", "HEAD")
	d := pollGitWatch(roots)
	if len(d[key]) != 1 || d[key][0] != headB {
		t.Fatalf("expected only B (%s), got %v", headB, d[key])
	}

	// Idle poll → cursor already at HEAD, nothing new.
	if d := pollGitWatch(roots); len(d) != 0 {
		t.Fatalf("idle poll must report nothing, got %v", d)
	}
}

// TestGitWatchHandlesDegenerateRoots asserts no panic and no false positives on
// a repo with zero commits, a non-git directory, and a detached HEAD.
func TestGitWatchHandlesDegenerateRoots(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	// Repo with no commits: rev-parse HEAD fails → skipped cleanly.
	empty, _, _ := gitRepo(t)
	if d := pollGitWatch([]string{empty}); len(d) != 0 {
		t.Errorf("repo with no commits must report nothing, got %v", d)
	}

	// Not a git repo at all → skipped cleanly.
	if d := pollGitWatch([]string{t.TempDir()}); len(d) != 0 {
		t.Errorf("non-repo must report nothing, got %v", d)
	}

	// Detached HEAD: rev-parse still resolves, so it baselines then idles.
	ws, git, _ := gitRepo(t)
	git("commit", "--allow-empty", "-m", "A")
	git("commit", "--allow-empty", "-m", "B")
	git("checkout", "--detach", "HEAD~1")
	pollGitWatch([]string{ws}) // cold-start baseline at detached HEAD
	if d := pollGitWatch([]string{ws}); len(d) != 0 {
		t.Errorf("detached HEAD steady state must report nothing, got %v", d)
	}
}
