package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The seam between workspaceMatchRoots and projectClaudeMdTokens.
//
// Both halves were individually correct and individually green, which is why
// this test did not exist and the defect lived in production instead.
// workspaceMatchRoots returns every path belonging to the repo — right for the
// transcript WATCHER, which must match a claude process running in any linked
// worktree. projectClaudeMdTokens summed the roots it was handed — right for a
// set of DISTINCT repositories. Composed, the census expanded one checkout back
// out to all of its worktrees and summed their instruction files, reporting a
// 5,856-token CLAUDE.md as 239,088 on a 42-worktree repo (measured 2026-08-18)
// and pricing the difference as $5,593/mo of recoverable config tax.
//
// The composed figure is what these tests assert. Run them against the
// pre-correction code and the first fails with 4x its expected value — that is
// the check that this test is worth trusting.
//
// A repo with ONE checkout reports identically before and after, which is
// exactly why the fixture below has several: the defect is invisible on the
// single-checkout shape and on every demo.

// gitInit makes dir a git repo with an origin remote and one commit — enough for
// `git worktree add`, and enough for workspaceKey to resolve a slug.
func gitInitWithRemote(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "remote", "add", "origin", "git@github.com:acme/multi.git")
	gitCmd(t, dir, "config", "user.email", "t@example.com")
	gitCmd(t, dir, "config", "user.name", "T")
}

// isolateCaptureRoots points state.StateDir() at a temp dir so
// workspaceMatchRoots reads an EMPTY registered-roots file rather than the
// developer's real one — otherwise this machine's own capture roots (and their
// worktrees) join the fixture and the assertion measures the wrong tree.
func isolateCaptureRoots(t *testing.T) {
	t.Helper()
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
}

func TestCensusSizesOneCheckoutNotTheWorktreeSet(t *testing.T) {
	isolateCaptureRoots(t)
	repo := t.TempDir()
	gitInitWithRemote(t, repo)

	// COMMITTED, so `git worktree add` populates each checkout with its own copy
	// the way a real worktree does. Writing the copies by hand would test a
	// fixture; this tests the thing the engineer actually has on disk.
	writeClaudeFixture(t, repo, "CLAUDE.md", 400) // 100 tokens
	gitCmd(t, repo, "add", "CLAUDE.md")
	gitCmd(t, repo, "commit", "-m", "init")

	// Three linked worktrees, one of them under `.claude/worktrees/` — the layout
	// this repo's own engineering guidance mandates and where 65 of the 73
	// CLAUDE.md copies on the audited machine live. The hidden-directory skip in
	// the nested walk does NOT protect the root branch: workspaceMatchRoots hands
	// it that path explicitly.
	inside := filepath.Join(repo, ".claude", "worktrees", "wt-a")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "worktree", "add", inside, "-b", "wt-a")
	gitCmd(t, repo, "worktree", "add", filepath.Join(t.TempDir(), "wt-b"), "-b", "wt-b")
	gitCmd(t, repo, "worktree", "add", filepath.Join(t.TempDir(), "wt-c"), "-b", "wt-c")

	// The watcher's half is UNCHANGED and must stay that way: capture depends on
	// every worktree being matchable.
	// At least four: the checkout plus its three worktrees. On macOS the seed
	// path (/var/...) and git's resolved path (/private/var/...) are different
	// strings for one tree and both survive the dedup, so this is a floor rather
	// than an equality — and it is worth noticing that the pre-correction sum was
	// therefore larger than the worktree count alone predicts.
	roots := workspaceMatchRoots(repo)
	if len(roots) < 4 {
		t.Fatalf("workspaceMatchRoots = %v (%d roots), want >= 4 — the watcher still needs every worktree", roots, len(roots))
	}
	for _, wt := range []string{"wt-a", "wt-b", "wt-c"} {
		found := false
		for _, r := range roots {
			if filepath.Base(r) == wt {
				found = true
			}
		}
		if !found {
			t.Fatalf("worktree %s missing from workspaceMatchRoots = %v — capture would stop matching it", wt, roots)
		}
	}

	// The sizing half, composed with it. 100 tokens — the one checkout a session
	// loads — NOT 400, the sum across four trees holding the same file.
	data := buildConfigCensus(censusEnv{claudeDir: t.TempDir(), workspaceRoots: roots})
	if data.ProjectClaudeMdTokens != 100 {
		t.Fatalf("projectClaudeMdTokens = %d, want 100 (one checkout, not the sum over %d)",
			data.ProjectClaudeMdTokens, len(roots))
	}
	if data.ProjectClaudeMdPosition != claudeMdPositionRoot {
		t.Errorf("position = %q, want %q", data.ProjectClaudeMdPosition, claudeMdPositionRoot)
	}
	// The size and the key describe the SAME tree — the property that makes the
	// figure comparable across devices at all.
	if data.WorkspaceKey != "acme/multi" {
		t.Errorf("workspaceKey = %q, want %q", data.WorkspaceKey, "acme/multi")
	}
}

// §1.5 — the nested fallback keeps its coverage. `config-census-per-repo-collapse`
// restored the path that lifts a false 0 for a repo whose memory lives in a
// sub-package; scoping the sizing to one root must not undo it, and must not
// multiply it by the worktree count either.
func TestCensusNestedFallbackSurvivesOneCheckoutScoping(t *testing.T) {
	isolateCaptureRoots(t)
	repo := t.TempDir()
	gitInitWithRemote(t, repo)

	writeClaudeFixture(t, repo, "app/CLAUDE.md", 240)       // 60 tokens — the largest nested
	writeClaudeFixture(t, repo, "packages/x/CLAUDE.md", 40) // 10 tokens — a sibling, never added
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-m", "init")
	gitCmd(t, repo, "worktree", "add", filepath.Join(t.TempDir(), "wt-a"), "-b", "wt-a")
	gitCmd(t, repo, "worktree", "add", filepath.Join(t.TempDir(), "wt-b"), "-b", "wt-b")

	data := buildConfigCensus(censusEnv{
		claudeDir:      t.TempDir(),
		workspaceRoots: workspaceMatchRoots(repo),
	})
	if data.ProjectClaudeMdTokens != 60 {
		t.Fatalf("projectClaudeMdTokens = %d, want 60 (largest nested file in ONE checkout)",
			data.ProjectClaudeMdTokens)
	}
	// Covered, but LATENT — Claude Code loads a sub-package memory lazily and
	// never re-injects it after /compact, so the backend prices it at $0. A
	// scoping change must not quietly promote it to always-on.
	if data.ProjectClaudeMdPosition != claudeMdPositionNested {
		t.Errorf("position = %q, want %q", data.ProjectClaudeMdPosition, claudeMdPositionNested)
	}
}
