package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// TestSessionRepoRootCollapsesToOneSlug is the core Phase-2 guarantee: a repo's
// main checkout, a subdirectory of it, and a linked worktree of it all resolve to
// the SAME repoRoot — the origin remote slug — so the backend de-fragments the
// repo across every checkout depth and joins it exactly to outcome_events.repo.
func TestSessionRepoRootCollapsesToOneSlug(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init")
	gitCmd(t, repo, "remote", "add", "origin", "git@github.com:acme/foo.git")
	// A commit is required before `git worktree add` can create a linked worktree.
	gitCmd(t, repo, "config", "user.email", "t@example.com")
	gitCmd(t, repo, "config", "user.name", "T")
	gitCmd(t, repo, "commit", "--allow-empty", "-m", "init")

	sub := filepath.Join(repo, "packages", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	worktree := t.TempDir()
	gitCmd(t, repo, "worktree", "add", worktree, "-b", "feature")

	const want = "acme/foo"
	for name, cwd := range map[string]string{
		"main-checkout": repo,
		"subdirectory":  sub,
		"worktree":      worktree,
	} {
		if got := sessionRepoRoot(cwd); got != want {
			t.Errorf("%s: sessionRepoRoot(%q) = %q, want %q", name, cwd, got, want)
		}
	}
}

// TestSessionRepoRootNonGitHashesFallback: a directory not inside any git repo
// resolves to a stable opaque 16-hex hash — never a slug, never a raw path.
func TestSessionRepoRootNonGitHashesFallback(t *testing.T) {
	dir := t.TempDir() // t.TempDir() is not a git repo
	got := sessionRepoRoot(dir)
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(got) {
		t.Errorf("non-git repoRoot = %q, want a 16-hex-char opaque hash", got)
	}
	// Stable across calls, and it must not leak a filesystem path.
	if again := sessionRepoRoot(dir); again != got {
		t.Errorf("non-git repoRoot not stable: %q != %q", got, again)
	}
}

// TestSessionRepoRootOmittedWhenCwdGone: a resumed/backfilled transcript whose
// cwd no longer exists on disk emits NO repoRoot (empty string), mirroring
// state.HomeRelativeStrict rather than hashing a stale path.
func TestSessionRepoRootOmittedWhenCwdGone(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "was-here-now-gone")
	if got := sessionRepoRoot(gone); got != "" {
		t.Errorf("sessionRepoRoot for a gone cwd = %q, want \"\"", got)
	}
	if got := sessionRepoRoot(""); got != "" {
		t.Errorf("sessionRepoRoot(\"\") = %q, want \"\"", got)
	}
}
