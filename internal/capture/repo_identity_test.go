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

// TestSessionRepoIdentityReportsHostWithSlug: the host rides alongside the slug
// from every checkout depth, exactly as repoRoot does. Without it the backend
// cannot tell this repo from a gitlab.com repo whose owner happens to collide.
func TestSessionRepoIdentityReportsHostWithSlug(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init")
	gitCmd(t, repo, "remote", "add", "origin", "https://GitLab.com:443/acme/foo.git")
	gitCmd(t, repo, "config", "user.email", "t@example.com")
	gitCmd(t, repo, "config", "user.name", "T")
	gitCmd(t, repo, "commit", "--allow-empty", "-m", "init")

	sub := filepath.Join(repo, "packages", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	gitCmd(t, repo, "worktree", "add", worktree, "-b", "feature")

	for name, cwd := range map[string]string{
		"main-checkout": repo,
		"subdirectory":  sub,
		"worktree":      worktree,
	} {
		root, host := sessionRepoIdentity(cwd)
		if root != "acme/foo" {
			t.Errorf("%s: repoRoot = %q, want acme/foo", name, root)
		}
		// Lowercased and port-stripped, or it will never compare equal to the
		// backend's provider host.
		if host != "gitlab.com" {
			t.Errorf("%s: repoHost = %q, want gitlab.com", name, host)
		}
	}
}

// TestSessionRepoIdentityNoHostWithoutRemote: the two opaque-hash cases have no
// remote by definition, so the host must be empty rather than inherited,
// guessed, or left over from another resolution.
func TestSessionRepoIdentityNoHostWithoutRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// git repo, no origin remote → hash key, no host.
	repo := t.TempDir()
	gitCmd(t, repo, "init")
	root, host := sessionRepoIdentity(repo)
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(root) {
		t.Errorf("no-remote repoRoot = %q, want a 16-hex opaque hash", root)
	}
	if host != "" {
		t.Errorf("no-remote repoHost = %q, want \"\"", host)
	}

	// Not a git repo at all → hash key, no host.
	if root, host = sessionRepoIdentity(t.TempDir()); host != "" {
		t.Errorf("non-git repoHost = %q (root %q), want \"\"", host, root)
	}

	// Gone / empty cwd → both empty.
	gone := filepath.Join(t.TempDir(), "was-here-now-gone")
	if root, host = sessionRepoIdentity(gone); root != "" || host != "" {
		t.Errorf("gone cwd = (%q, %q), want both empty", root, host)
	}
	if root, host = sessionRepoIdentity(""); root != "" || host != "" {
		t.Errorf("empty cwd = (%q, %q), want both empty", root, host)
	}
}

// TestSessionRepoRootUnchangedByHostSplit: sessionRepoRoot is now a wrapper over
// sessionRepoIdentity, so pin that the value every existing caller sees did not
// move. This is the whole safety argument for the refactor.
func TestSessionRepoRootUnchangedByHostSplit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	withRemote := t.TempDir()
	gitCmd(t, withRemote, "init")
	gitCmd(t, withRemote, "remote", "add", "origin", "git@github.com:acme/foo.git")
	noRemote := t.TempDir()
	gitCmd(t, noRemote, "init")

	for _, cwd := range []string{withRemote, noRemote, t.TempDir(), ""} {
		root, _ := sessionRepoIdentity(cwd)
		if got := sessionRepoRoot(cwd); got != root {
			t.Errorf("sessionRepoRoot(%q) = %q, but sessionRepoIdentity returned root %q", cwd, got, root)
		}
	}
}
