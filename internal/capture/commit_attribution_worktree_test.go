package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in an EXISTING directory without initializing it — gitRepoAt
// always runs `git init`, which would clobber the `.git` FILE that marks a
// linked worktree and turn the second directory into an unrelated repository,
// quietly destroying the very thing these tests exist to exercise.
func gitIn(t *testing.T, dir string) (run func(args ...string), out func(args ...string) string) {
	t.Helper()
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
	return run, out
}

// AI-path evidence is recorded under the path of whichever checkout the agent
// actually edited in, so the SAME commit used to reconcile `likely_ai` from that
// checkout and `unknown` from a sibling worktree of the same repository. Running
// several worktrees against one repository is the ordinary move that hits it, and
// every one of them under-reported.
//
// EVERY TEST IN THIS FILE USES A GENUINE SECOND DIRECTORY (`git worktree add`).
// That is not incidental: the pre-existing
// TestReworkAdoptionRebuildsSpansAttributedByAnotherWorktree simulates the second
// worktree inside ONE directory, which is precisely what let this defect through
// — one directory is one ledger key, so the divergence never appears.

// worktreeAttributionRepo stands up the autostart-daemon layout with a real
// second checkout:
//
//	home/                      <- TaskRoot (the daemon's workspace)
//	  repos/proj/              <- the checkout the agent edits in
//	  work/proj-wt/            <- `git worktree add --detach`, same repository
//
// It records AI evidence for repos/proj/a.go exactly as dedupeFileDiff would
// (workspace-relative path, workspace root key), commits the file, and returns
// the SHA both checkouts can be asked to reconcile.
func worktreeAttributionRepo(t *testing.T) (home, primary, wt, sha, sessionID string) {
	t.Helper()
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	home = t.TempDir()
	primary = filepath.Join(home, "repos", "proj")
	git, gitOut := gitRepoAt(t, primary)
	git("commit", "--allow-empty", "-m", "base")

	sessionID = "sess-wt"
	// Exactly what dedupeFileDiff records: the workspace root key, and the path
	// relativized against the workspace — i.e. against the checkout the agent
	// edited in.
	recordAiTouchedPath(sessionID, gitWatchRootKey(home), "repos/proj/a.go")

	writeCommitFile(t, primary, "a.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds a.go")
	sha = gitOut("rev-parse", "HEAD")

	// A genuine second directory for the same repository, detached at the very
	// commit under test so both checkouts reconcile the identical SHA.
	wt = filepath.Join(home, "work", "proj-wt")
	git("worktree", "add", "--detach", wt, sha)

	return home, primary, wt, sha, sessionID
}

// attributionOf reconciles one commit from one checkout and returns the
// per-path attribution plus the representative session.
func attributionOf(t *testing.T, root, taskRoot, sha string) (map[string]string, string) {
	t.Helper()
	_, files, primarySession, ok := commitAttributionFromDiff(root, taskRoot, sha)
	if !ok {
		t.Fatalf("commitAttributionFromDiff(%s) reported no attributable change", root)
	}
	got := map[string]string{}
	for _, f := range files {
		for _, r := range f.LineRanges {
			got[f.Path] = r.Attribution
		}
	}
	return got, primarySession
}

// REGRESS: the same commit must reconcile the same way from every checkout of
// the repository. Before the fix the sibling worktree returned `unknown` with no
// session — the headline AI-attribution number, silently under-reported on every
// machine running more than one worktree.
func TestCommitAttributionAgreesAcrossWorktreesOfOneRepo(t *testing.T) {
	home, primary, wt, sha, sessionID := worktreeAttributionRepo(t)

	fromPrimary, primarySess := attributionOf(t, primary, home, sha)
	if fromPrimary["a.go"] != attributionLikelyAI {
		t.Fatalf("primary checkout: a.go = %q, want %q (the evidence was recorded here)", fromPrimary["a.go"], attributionLikelyAI)
	}
	if primarySess != sessionID {
		t.Fatalf("primary checkout: session = %q, want %q", primarySess, sessionID)
	}

	fromWorktree, wtSess := attributionOf(t, wt, home, sha)
	if fromWorktree["a.go"] != attributionLikelyAI {
		t.Fatalf("sibling worktree: a.go = %q, want %q — the same commit in the same repository must reconcile identically from either checkout", fromWorktree["a.go"], attributionLikelyAI)
	}
	if wtSess != sessionID {
		t.Fatalf("sibling worktree: session = %q, want %q — the recovered evidence must carry the session that produced it", wtSess, sessionID)
	}
}

// The evidence a deployed machine already holds is written under the checkout it
// was recorded in, and that checkout must keep reading it byte-for-byte as
// before. This is the case an `ai-paths.json` on disk today is in; a fix that
// recovers the sibling at the cost of the original would trade one under-count
// for another.
func TestCommitAttributionStillReadsEvidenceUnderItsOwnRecordedKey(t *testing.T) {
	home, primary, _, sha, sessionID := worktreeAttributionRepo(t)

	got, sess := attributionOf(t, primary, home, sha)
	if got["a.go"] != attributionLikelyAI || sess != sessionID {
		t.Fatalf("recording checkout: a.go = %q session = %q, want %q / %q", got["a.go"], sess, attributionLikelyAI, sessionID)
	}
}

// COLLISION #1 — two different repositories that happen to share a relative
// path. Evidence for `a.go` in repo A must never attribute `a.go` in repo B.
// Asserting the ABSENCE of attribution, not merely a count: a wrong answer and a
// missing answer must not pass the same check.
func TestCommitAttributionDoesNotCrossUnrelatedRepositories(t *testing.T) {
	home, _, _, _, _ := worktreeAttributionRepo(t)

	// A second, unrelated repository under the same workspace, with a file at the
	// same repo-relative path. No AI evidence is recorded for it at all.
	other := filepath.Join(home, "repos", "unrelated")
	git, gitOut := gitRepoAt(t, other)
	git("commit", "--allow-empty", "-m", "base")
	writeCommitFile(t, other, "a.go", "x1\nx2\nx3\n")
	git("add", "-A")
	git("commit", "-m", "human adds a.go")
	otherSha := gitOut("rev-parse", "HEAD")

	got, sess := attributionOf(t, other, home, otherSha)
	if got["a.go"] != attributionUnknown {
		t.Fatalf("unrelated repo: a.go = %q, want %q — a shared relative path is not shared evidence", got["a.go"], attributionUnknown)
	}
	if sess != "" {
		t.Fatalf("unrelated repo: session = %q, want no session at all", sess)
	}
}

// COLLISION #2 — a sibling worktree of the SAME repository, but a path that no
// agent ever wrote. The recovery must widen the checkouts it looks in, never the
// paths it accepts: an unrelated file committed from the worktree stays unknown
// even though its own repository does hold AI evidence for a different file.
func TestCommitAttributionWorktreeRecoveryDoesNotWidenToUntouchedPaths(t *testing.T) {
	home, _, wt, _, _ := worktreeAttributionRepo(t)

	git, gitOut := gitIn(t, wt)
	git("checkout", "-b", "wt-work")
	writeCommitFile(t, wt, "human.go", "h1\nh2\n")
	git("add", "-A")
	git("commit", "-m", "human adds human.go")
	humanSha := gitOut("rev-parse", "HEAD")

	got, sess := attributionOf(t, wt, home, humanSha)
	if got["human.go"] != attributionUnknown {
		t.Fatalf("untouched path: human.go = %q, want %q", got["human.go"], attributionUnknown)
	}
	if sess != "" {
		t.Fatalf("untouched path: session = %q, want no session at all", sess)
	}
}

// A CLONE is not a worktree. Two clones of one upstream have independent object
// stores, so evidence must NOT cross between them — the conservative direction,
// and the boundary the recovery is scoped against.
func TestCommitAttributionDoesNotCrossIntoASeparateClone(t *testing.T) {
	home, primary, _, sha, _ := worktreeAttributionRepo(t)

	clone := filepath.Join(home, "repos", "proj-clone")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	if o, err := exec.Command("git", "clone", "--quiet", primary, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, o)
	}

	got, sess := attributionOf(t, clone, home, sha)
	if got["a.go"] != attributionUnknown {
		t.Fatalf("separate clone: a.go = %q, want %q — a clone is a different checkout, not a worktree of this one", got["a.go"], attributionUnknown)
	}
	if sess != "" {
		t.Fatalf("separate clone: session = %q, want no session at all", sess)
	}
}

// gitSiblingWorktrees is the primitive the entire scoping argument rests on: it
// is what says a checkout belongs to THIS repository and nothing else does. All
// three answers are pinned together — none, one, and never-a-clone — because the
// widening is only as sound as this list is tight.
//
// It also pins the property that keeps the memo from hiding a fresh worktree: a
// repo with no linked worktrees short-circuits BEFORE the cache and is therefore
// never cached, so the first `git worktree add` is visible immediately rather
// than up to one poll interval later.
func TestGitSiblingWorktreesIsScopedToOneRepository(t *testing.T) {
	home := t.TempDir()
	primary := filepath.Join(home, "repos", "proj")
	git, gitOut := gitRepoAt(t, primary)
	git("commit", "--allow-empty", "-m", "base")
	sha := gitOut("rev-parse", "HEAD")

	// An unrelated repository, and a clone of the primary. Neither is a worktree
	// of the primary, and both are created BEFORE the primary is first asked —
	// so a list that leaked them would be leaking them at the first opportunity.
	otherGit, _ := gitRepoAt(t, filepath.Join(home, "repos", "unrelated"))
	otherGit("commit", "--allow-empty", "-m", "base")
	clone := filepath.Join(home, "repos", "proj-clone")
	if o, err := exec.Command("git", "clone", "--quiet", primary, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, o)
	}

	if got := gitSiblingWorktrees(primary); len(got) != 0 {
		t.Fatalf("before any worktree add: siblings = %v, want none", got)
	}

	wt := filepath.Join(home, "work", "proj-wt")
	git("worktree", "add", "--detach", wt, sha)

	// Immediately, with no interval elapsed: the short-circuit means the empty
	// answer above was never cached.
	got := gitSiblingWorktrees(primary)
	if len(got) != 1 || got[0] != resolvePath(wt) {
		t.Fatalf("after worktree add: siblings = %v, want exactly [%s] — a clone and an unrelated repo must not appear, and a new worktree must not wait for the memo to expire",
			got, resolvePath(wt))
	}
	// And symmetrically from the new checkout.
	back := gitSiblingWorktrees(wt)
	if len(back) != 1 || back[0] != resolvePath(primary) {
		t.Fatalf("from the worktree: siblings = %v, want exactly [%s]", back, resolvePath(primary))
	}
	// The clone shares an upstream, not an object store: it is nobody's sibling.
	if g := gitSiblingWorktrees(clone); len(g) != 0 {
		t.Fatalf("clone: siblings = %v, want none", g)
	}
}

// The DURABILITY ledger reads the same evidence through the same scope, so the
// sibling-worktree lookup reaches its path-level seed gate too. Both directions
// have to hold, and they are asserted in ONE test against ONE ledger so a fix
// that opened the gate for everything could not pass half of it:
//
//	positive — a path an agent wrote in the sibling checkout IS seeded here
//	           (this half fails before the fix: 0 lines tracked);
//	negative — a path NO agent wrote anywhere is NOT, even though the very same
//	           commit carries a path that is. Absence, not a count.
func TestDurabilitySeedsFromSiblingWorktreeEvidenceButNotUntouchedPaths(t *testing.T) {
	home, primary, wt, _, _ := worktreeAttributionRepo(t)
	const t0 int64 = 1_000_000_000_000

	// The agent writes a SECOND file, in the primary checkout, exactly as capture
	// would record it: workspace root key, path relative to that checkout.
	recordAiTouchedPath("sess-wt", gitWatchRootKey(home), "repos/proj/agent.go")

	// One commit made from the SIBLING worktree carrying both files: agent.go,
	// which the agent wrote in the other checkout, and human.go, which no agent
	// ever touched in any checkout.
	git, gitOut := gitIn(t, wt)
	git("checkout", "-b", "wt-durability")
	writeCommitFile(t, wt, "agent.go", "a1\na2\na3\n")
	writeCommitFile(t, wt, "human.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "agent.go plus an untouched human.go")

	wtKey := gitWatchRootKey(wt)
	sess := Session{DeviceID: "dev-wt", TaskRoot: home}
	pollDurabilityCommit(wt, wtKey, sess, gitOut("rev-parse", "HEAD"), t0)

	if got := trackedLineCount(t, wtKey, "agent.go"); got != 3 {
		t.Errorf("agent.go tracked = %d, want 3 — evidence recorded in %s must seed a commit polled from %s",
			got, primary, wt)
	}
	if got := trackedLineCount(t, wtKey, "human.go"); got != 0 {
		t.Errorf("a path NO agent wrote in ANY checkout was seeded as AI: %d lines tracked, want 0 (ranges %+v)",
			got, ledgerRanges(t, wtKey, "human.go"))
	}
}
