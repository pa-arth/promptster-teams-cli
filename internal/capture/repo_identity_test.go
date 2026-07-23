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
		root, host, tracked := sessionRepoIdentity(cwd)
		if root != "acme/foo" {
			t.Errorf("%s: repoRoot = %q, want acme/foo", name, root)
		}
		// Lowercased and port-stripped, or it will never compare equal to the
		// backend's provider host.
		if host != "gitlab.com" {
			t.Errorf("%s: repoHost = %q, want gitlab.com", name, host)
		}
		if !tracked {
			t.Errorf("%s: repoTracked = false, want true — a repo with a remote is a repo", name)
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
	root, host, _ := sessionRepoIdentity(repo)
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(root) {
		t.Errorf("no-remote repoRoot = %q, want a 16-hex opaque hash", root)
	}
	if host != "" {
		t.Errorf("no-remote repoHost = %q, want \"\"", host)
	}

	// Not a git repo at all → hash key, no host.
	if root, host, _ = sessionRepoIdentity(t.TempDir()); host != "" {
		t.Errorf("non-git repoHost = %q (root %q), want \"\"", host, root)
	}

	// Gone / empty cwd → both empty.
	gone := filepath.Join(t.TempDir(), "was-here-now-gone")
	if root, host, _ = sessionRepoIdentity(gone); root != "" || host != "" {
		t.Errorf("gone cwd = (%q, %q), want both empty", root, host)
	}
	if root, host, _ = sessionRepoIdentity(""); root != "" || host != "" {
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
		root, _, _ := sessionRepoIdentity(cwd)
		if got := sessionRepoRoot(cwd); got != root {
			t.Errorf("sessionRepoRoot(%q) = %q, but sessionRepoIdentity returned root %q", cwd, got, root)
		}
	}
}

// TestSessionRepoIdentityTrackedSeparatesTheTwoHashCases is the whole point of
// the change: "git repo with no origin remote" and "not a git repo at all"
// produce a structurally identical 16-hex key, and ONLY the tracked bit tells
// them apart. A remoteless repo is a real repo (keep its board row); a home or
// container directory is not (it must come off the board).
func TestSessionRepoIdentityTrackedSeparatesTheTwoHashCases(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)

	// Case 2: git repo, no origin remote → opaque hash, TRACKED.
	noRemote := t.TempDir()
	gitCmd(t, noRemote, "init")
	rootA, hostA, trackedA := sessionRepoIdentity(noRemote)
	if !hex16.MatchString(rootA) {
		t.Errorf("no-remote repoRoot = %q, want a 16-hex opaque hash", rootA)
	}
	if hostA != "" {
		t.Errorf("no-remote repoHost = %q, want \"\"", hostA)
	}
	if !trackedA {
		t.Error("no-remote repoTracked = false, want true — a repo without a remote is still a repo")
	}

	// A SUBDIRECTORY of that repo is tracked too: gitRootOf walks up, so depth
	// must not change the answer (the live bug was a session launched one dir
	// too high, not one too deep).
	sub := filepath.Join(noRemote, "packages", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, tracked := sessionRepoIdentity(sub); !tracked {
		t.Error("subdir of a no-remote repo: repoTracked = false, want true")
	}

	// Case 3: not a git repo at all → opaque hash of the SAME shape, NOT tracked.
	plain := t.TempDir()
	rootB, hostB, trackedB := sessionRepoIdentity(plain)
	if !hex16.MatchString(rootB) {
		t.Errorf("non-git repoRoot = %q, want a 16-hex opaque hash", rootB)
	}
	if hostB != "" {
		t.Errorf("non-git repoHost = %q, want \"\"", hostB)
	}
	if trackedB {
		t.Error("non-git repoTracked = true, want false — a container/home dir is not a repo")
	}

	// The keys really are indistinguishable by shape — which is why the bit has
	// to exist. If this ever fails, the premise of the change has changed.
	if len(rootA) != len(rootB) {
		t.Errorf("hash keys differ in shape (%q vs %q); the tracked bit's premise no longer holds", rootA, rootB)
	}
}

// TestSessionRepoIdentityTrackedFalseWhenCwdUnresolvable: an empty or gone cwd
// returns tracked=false, but that value is MEANINGLESS on its own — root is ""
// too, and the emitter must omit BOTH fields. Absent means "we did not look"
// (treated as tracked downstream); an emitted false would be a positive claim
// that the directory is not a working tree, which we never observed.
func TestSessionRepoIdentityTrackedFalseWhenCwdUnresolvable(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "was-here-now-gone")
	for _, cwd := range []string{gone, ""} {
		root, host, tracked := sessionRepoIdentity(cwd)
		if root != "" || host != "" {
			t.Errorf("unresolvable cwd %q = (%q, %q), want both empty", cwd, root, host)
		}
		if tracked {
			t.Errorf("unresolvable cwd %q: repoTracked = true, want the false zero value", cwd)
		}
	}
}

// TestSessionRepoIdentityRootByteIdenticalAcrossTheChange is the safety argument
// for adding the third return value: repoRoot's VALUE must not move in ANY case,
// because the backend keys rollups and the PR-count join on it. The pre-change
// resolution is reimplemented verbatim below and compared byte-for-byte — a
// separate implementation, so a regression in either one fails the test.
func TestSessionRepoIdentityRootByteIdenticalAcrossTheChange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// The exact body of sessionRepoIdentity BEFORE this change.
	legacyRoot := func(cwd string) string {
		if cwd == "" {
			return ""
		}
		dir := resolvePath(cwd)
		if _, err := os.Stat(dir); err != nil {
			return ""
		}
		if r, ok := gitRootOf(dir); ok {
			dir = r
		}
		if _, slug := gitRemote(dir); slug != "" {
			return slug
		}
		return workspaceHashKey(dir)
	}

	withRemote := t.TempDir()
	gitCmd(t, withRemote, "init")
	gitCmd(t, withRemote, "remote", "add", "origin", "git@github.com:acme/foo.git")
	noRemote := t.TempDir()
	gitCmd(t, noRemote, "init")
	gone := filepath.Join(t.TempDir(), "was-here-now-gone")

	for name, cwd := range map[string]string{
		"remote":   withRemote,
		"noremote": noRemote,
		"nongit":   t.TempDir(),
		"gone":     gone,
		"empty":    "",
	} {
		root, _, _ := sessionRepoIdentity(cwd)
		if want := legacyRoot(cwd); root != want {
			t.Errorf("%s: repoRoot = %q, want %q (byte-identical to pre-change)", name, root, want)
		}
	}
}
