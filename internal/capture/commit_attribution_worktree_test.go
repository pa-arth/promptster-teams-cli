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
// The recovery is GATED ON LINEAGE, IN ONE DIRECTION ONLY: a sibling's evidence
// counts while that checkout's HEAD REACHES the commit, and never because the
// commit descends from that HEAD. The descendant direction is the ordinary state
// of a worktree — `git worktree add -b feat` leaves the new checkout sitting on
// the branch point, and evidence is recorded when the agent WRITES, with no commit
// needed — so it admits every later commit on every branch and gates nothing. See
// TestCommitAttributionDoesNotCrossADivergentSiblingBranch and
// TestCommitAttributionDoesNotCrossToASiblingTheCommitMerelyDescendsFrom, both of
// which assert absence on all three of attribution, session and durability spans.
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
	// The lineage is resolved exactly as the watcher resolves it: one batched read
	// per sibling for the range being polled, never a per-commit probe.
	return attributionWith(t, root, taskRoot, sha, newSiblingLineage(root, []string{sha}))
}

// assertReaches pins a test's git geometry with `merge-base --is-ancestor`, a
// path that shares no code with siblingLineage — so a mutation of the lineage
// read cannot pass its own precondition check.
func assertReaches(t *testing.T, checkout, sha string, want bool) {
	t.Helper()
	err := exec.Command("git", "-C", checkout, "merge-base", "--is-ancestor", sha, "HEAD").Run()
	if got := err == nil; got != want {
		t.Fatalf("setup: %s HEAD reaches %s = %v, want %v — this test is only meaningful with a genuinely MIXED range", checkout, sha, got, want)
	}
}

// attributionWith is attributionOf against a lineage the caller built, so a test
// can reconcile several commits of ONE range through the ONE lineage the watcher
// would have resolved for it.
func attributionWith(t *testing.T, root, taskRoot, sha string, lin siblingLineage) (map[string]string, string) {
	t.Helper()
	_, files, primarySession, ok := commitAttributionFromDiff(root, taskRoot, sha, lin)
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

// COLLISION #3, AND THE ONE THAT FABRICATES — a sibling worktree of the SAME
// repository, holding real AI evidence for the SAME relative path, but parked on
// a DIVERGENT branch. Sibling worktrees are how a repository holds several
// branches at once, so matching a sibling's evidence on the relative path alone
// hands an agent's write in the feature worktree to a human's commit on the
// default branch — an invented number, which outranks any under-count.
//
// The commit is not reachable from the agent worktree's HEAD, so the lookup
// MISSES: `unknown`, no session, and no durability span. Absence on all three, so
// a wrong answer and a missing answer cannot pass the same assertion.
func TestCommitAttributionDoesNotCrossADivergentSiblingBranch(t *testing.T) {
	home, primary, wt, _, _ := worktreeAttributionRepo(t)
	const t0 int64 = 1_000_000_000_000

	// The agent's checkout, moved onto a feature branch and given a commit of its
	// own so its history genuinely diverges from the default branch. The evidence
	// is recorded exactly as capture would: workspace root key, path relative to
	// the checkout the agent edited in.
	wtGit, _ := gitIn(t, wt)
	wtGit("checkout", "-b", "feat")
	recordAiTouchedPath("sess-feat", gitWatchRootKey(home), "work/proj-wt/internal/x.go")
	writeCommitFile(t, wt, "internal/x.go", "ai1\nai2\nai3\n")
	wtGit("add", "-A")
	wtGit("commit", "-m", "ai writes internal/x.go on feat")

	// A human hand-writes the SAME relative path in the OTHER checkout, on the
	// default branch, well inside the 7-day ai-paths TTL, and commits it there.
	git, gitOut := gitIn(t, primary)
	writeCommitFile(t, primary, "internal/x.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human writes internal/x.go on the default branch")
	humanSha := gitOut("rev-parse", "HEAD")

	got, sess := attributionOf(t, primary, home, humanSha)
	if got["internal/x.go"] != attributionUnknown {
		t.Fatalf("divergent sibling: internal/x.go = %q, want %q — a worktree on another branch is not evidence about this commit",
			got["internal/x.go"], attributionUnknown)
	}
	if sess != "" {
		t.Fatalf("divergent sibling: session = %q, want no session at all", sess)
	}

	primaryKey := gitWatchRootKey(primary)
	pollDurabilityCommit(primary, primaryKey, Session{DeviceID: "dev-primary", TaskRoot: home}, humanSha, t0,
		newSiblingLineage(primary, []string{humanSha}))
	if n := trackedLineCount(t, primaryKey, "internal/x.go"); n != 0 {
		t.Fatalf("a human's lines were seeded as AI off a divergent worktree's evidence: %d lines tracked, want 0 (ranges %+v)",
			n, ledgerRanges(t, primaryKey, "internal/x.go"))
	}
}

// commitsHeldBy IS THE GATE, AND PRODUCTION ALWAYS ASKS IT ABOUT A WHOLE POLL'S
// RANGE — never one commit at a time. `newSiblingLineage` passes every sha the
// poll surfaced in ONE read, so a batch mixing commits the sibling holds with
// commits it does not is the ordinary input, not an edge case.
//
// It matters because the interpretation is INVERTED from what git prints:
// `rev-list <shas...> --not HEAD` lists what the sibling does NOT hold, and held
// is the complement. Both ways of collapsing that on a mixed batch are silent and
// opposite — treat any output as "holds nothing" and a whole poll's real evidence
// is discarded; treat empty-for-some as "holds everything" and an unrelated
// sibling branch's evidence is admitted, which is the fabrication the gate exists
// to stop. Single-sha tests cannot tell those apart, because with one input the
// output is either empty or everything.
//
// The short-sha row is in the same batch deliberately: rev-list resolves an
// abbreviated rev and prints the FULL name, so the abbreviation never appears in
// the not-held set and would read as held without isFullObjectName — evidence
// admitted from a sibling that does not hold the commit at all.
// Requested by review on PR #136.
func TestCommitsHeldByAnswersEachShaInAMixedRange(t *testing.T) {
	home := t.TempDir()
	primary := filepath.Join(home, "repos", "proj")
	git, gitOut := gitRepoAt(t, primary)

	writeCommitFile(t, primary, "base.go", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	base := gitOut("rev-parse", "HEAD")

	// A sibling worktree branched at base, given one commit of its own. Its HEAD
	// therefore holds base and its own commit, and nothing the primary does after.
	wt := filepath.Join(home, "repos", "proj-wt")
	git("worktree", "add", "-b", "feat", wt)
	wtGit, wtOut := gitIn(t, wt)
	writeCommitFile(t, wt, "feat.go", "feat\n")
	wtGit("add", "-A")
	wtGit("commit", "-m", "on feat")
	onFeat := wtOut("rev-parse", "HEAD")

	// Two more commits on the default branch, after the worktree branched off.
	writeCommitFile(t, primary, "after1.go", "a1\n")
	git("add", "-A")
	git("commit", "-m", "after 1")
	after1 := gitOut("rev-parse", "HEAD")
	writeCommitFile(t, primary, "after2.go", "a2\n")
	git("add", "-A")
	git("commit", "-m", "after 2")
	after2 := gitOut("rev-parse", "HEAD")

	// ONE batch, interleaved so neither a prefix nor a suffix of the input could
	// produce the right answer by accident.
	shortBase := base[:8]
	held := commitsHeldBy(wt, []string{after1, base, after2, onFeat, shortBase})

	want := map[string]bool{
		base:   true,  // the branch point — the sibling holds it
		onFeat: true,  // the sibling's own commit
		after1: false, // landed on the default branch after it forked
		after2: false,
		// shortBase is NOT expected: rev-list prints full names, so an
		// abbreviation cannot be compared against the output it is judged by.
	}
	for sha, wantHeld := range want {
		if held[sha] != wantHeld {
			t.Errorf("commitsHeldBy(%s)[%s] = %v, want %v — a mixed range must be answered per-sha, not collapsed",
				wt, sha[:8], held[sha], wantHeld)
		}
	}
	if held[shortBase] {
		t.Errorf("an abbreviated rev (%s) read as held — rev-list prints FULL object names, so a short one can never match the output it is compared against, and admitting it lets a sibling contribute evidence about a commit it may not hold", shortBase)
	}
	if len(held) != 2 {
		t.Errorf("held = %v, want exactly the two commits the sibling reaches", held)
	}

	// The same batch through the production entry point, to pin that the gate the
	// attribution loop actually consults agrees with the read above. Keyed by
	// resolvePath because gitSiblingWorktrees resolves every checkout it returns —
	// on macOS a t.TempDir() path is a symlink into /private, so the raw path is
	// not the key production uses.
	sib := resolvePath(wt)
	lin := newSiblingLineage(primary, []string{after1, base, after2, onFeat})
	if !lin.reaches(sib, base) || !lin.reaches(sib, onFeat) {
		t.Errorf("siblingLineage lost evidence the sibling genuinely holds: base=%v onFeat=%v", lin.reaches(sib, base), lin.reaches(sib, onFeat))
	}
	if lin.reaches(sib, after1) || lin.reaches(sib, after2) {
		t.Errorf("siblingLineage admitted commits the sibling forked away from: after1=%v after2=%v", lin.reaches(sib, after1), lin.reaches(sib, after2))
	}
}

// A sibling that cannot be read at all must hold NOTHING. The inverted rev-list
// makes the safe direction the non-obvious one: a failed read produces no output,
// and "no output" is also what a sibling holding every sha produces, so anything
// that keys off the output rather than the error admits an entire poll's evidence
// from a checkout it could not even inspect.
func TestCommitsHeldByHoldsNothingWhenTheSiblingCannotBeRead(t *testing.T) {
	home := t.TempDir()
	primary := filepath.Join(home, "repos", "proj")
	git, gitOut := gitRepoAt(t, primary)
	git("commit", "--allow-empty", "-m", "base")
	sha := gitOut("rev-parse", "HEAD")

	if held := commitsHeldBy(filepath.Join(home, "no-such-checkout"), []string{sha}); len(held) != 0 {
		t.Errorf("held = %v, want empty — an unreadable checkout is not evidence of anything", held)
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
// sibling-worktree lookup reaches its path-level seed gate too. Both halves are
// asserted in ONE test against ONE ledger so a fix that opened the gate for
// everything could not pass half of it:
//
//	positive — a path an agent wrote in the sibling checkout IS seeded here, for
//	           a commit that sibling's HEAD REACHES (this half fails before the
//	           sibling lookup existed: 0 lines tracked);
//	negative — a path NO agent wrote anywhere is NOT, even though the very same
//	           commit carries a path that is. Absence, not a count.
//
// The commit is made in the checkout that holds the evidence and polled from the
// other one, which is the ONLY shape the gate admits: the evidence-holding
// checkout's HEAD is the commit. A sibling the commit merely descends from is the
// separate, and forbidden, shape — see
// TestCommitAttributionDoesNotCrossToASiblingTheCommitMerelyDescendsFrom.
func TestDurabilitySeedsFromSiblingWorktreeEvidenceButNotUntouchedPaths(t *testing.T) {
	home, primary, wt, _, _ := worktreeAttributionRepo(t)
	const t0 int64 = 1_000_000_000_000

	// The agent writes a SECOND file, in the primary checkout, exactly as capture
	// would record it: workspace root key, path relative to that checkout.
	recordAiTouchedPath("sess-wt", gitWatchRootKey(home), "repos/proj/agent.go")

	// One commit made in the checkout the agent worked in, carrying both files:
	// agent.go, which the agent wrote here, and human.go, which no agent ever
	// touched in any checkout. The primary's HEAD is therefore this commit.
	git, gitOut := gitIn(t, primary)
	writeCommitFile(t, primary, "agent.go", "a1\na2\na3\n")
	writeCommitFile(t, primary, "human.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "agent.go plus an untouched human.go")
	sha := gitOut("rev-parse", "HEAD")

	// Polled from the OTHER checkout, whose own ledger key holds nothing for
	// either path.
	wtKey := gitWatchRootKey(wt)
	sess := Session{DeviceID: "dev-wt", TaskRoot: home}
	pollDurabilityCommit(wt, wtKey, sess, sha, t0, newSiblingLineage(wt, []string{sha}))

	if got := trackedLineCount(t, wtKey, "agent.go"); got != 3 {
		t.Errorf("agent.go tracked = %d, want 3 — evidence recorded in %s must seed a commit polled from %s",
			got, primary, wt)
	}
	if got := trackedLineCount(t, wtKey, "human.go"); got != 0 {
		t.Errorf("a path NO agent wrote in ANY checkout was seeded as AI: %d lines tracked, want 0 (ranges %+v)",
			got, ledgerRanges(t, wtKey, "human.go"))
	}
}

// THE FABRICATION THE ONE-DIRECTIONAL GATE EXISTS FOR, in the state a worktree is
// ORDINARILY left in — not a corner case, the common case.
//
// `git worktree add -b feat` leaves the new checkout standing on the branch point,
// and AI evidence is recorded when the agent WRITES a file, with no commit needed.
// So the agent worktree's HEAD is the base commit, and EVERY later commit on EVERY
// branch of the repository descends from it. A gate that also accepted "the commit
// descends from this sibling's HEAD" would therefore admit that sibling's evidence
// for a human's commit on the default branch — which is a human's lines reported
// as AI, the one error class that outranks any under-count.
//
// Absence on all three of attribution, session and durability spans, so a wrong
// answer and a missing answer cannot pass the same assertion.
func TestCommitAttributionDoesNotCrossToASiblingTheCommitMerelyDescendsFrom(t *testing.T) {
	home, primary, wt, base, _ := worktreeAttributionRepo(t)
	const t0 int64 = 1_000_000_000_000

	// The agent's checkout: a branch of its own, still parked on the branch point,
	// with NO commit of its own. This is what `git worktree add -b` produces.
	wtGit, wtOut := gitIn(t, wt)
	wtGit("checkout", "-b", "feat")
	if head := wtOut("rev-parse", "HEAD"); head != base {
		t.Fatalf("sibling HEAD = %s, want the branch point %s — this test is only meaningful while the sibling has no commit of its own", head, base)
	}
	recordAiTouchedPath("sess-feat", gitWatchRootKey(home), "work/proj-wt/internal/x.go")

	// A human hand-writes the SAME relative path in the OTHER checkout, on the
	// default branch, on top of that same branch point.
	git, gitOut := gitIn(t, primary)
	writeCommitFile(t, primary, "internal/x.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human writes internal/x.go on the default branch")
	humanSha := gitOut("rev-parse", "HEAD")

	got, sess := attributionOf(t, primary, home, humanSha)
	if got["internal/x.go"] != attributionUnknown {
		t.Fatalf("descendant sibling: internal/x.go = %q, want %q — a worktree the commit merely descends from is not evidence about that commit",
			got["internal/x.go"], attributionUnknown)
	}
	if sess != "" {
		t.Fatalf("descendant sibling: session = %q, want no session at all", sess)
	}

	primaryKey := gitWatchRootKey(primary)
	pollDurabilityCommit(primary, primaryKey, Session{DeviceID: "dev-primary", TaskRoot: home}, humanSha, t0,
		newSiblingLineage(primary, []string{humanSha}))
	if n := trackedLineCount(t, primaryKey, "internal/x.go"); n != 0 {
		t.Fatalf("a human's lines were seeded as AI off a worktree the commit merely descends from: %d lines tracked, want 0 (ranges %+v)",
			n, ledgerRanges(t, primaryKey, "internal/x.go"))
	}
}

// THE SEED GATE AND THE TOMBSTONE BOOKKEEPING READ THE SAME MARKS THROUGH
// DIFFERENT VIEWS, and this is the first of the two defects that collapsing them
// into one produces.
//
// A tombstone records evidence already SPENT on a path. Pruning it is only safe
// while the gate it guards can never fire again, so the pruner must ask whether
// ANY checkout of the repository still holds that path's evidence — not whether
// the checkout being polled right now happens to hold the commit. Judged by the
// narrow view, an agent worktree moving onto its own branch DELETES a live
// tombstone, and the next purely-human commit to that path is seeded as fresh AI.
func TestDurabilityTombstoneSurvivesASiblingThatMovedOffTheCommit(t *testing.T) {
	home, primary, wt, _, _ := worktreeAttributionRepo(t)
	const t0 int64 = 1_000_000_000_000

	// The ONLY evidence for p.go lives under the sibling's path space...
	recordAiTouchedPath("sess-feat", gitWatchRootKey(home), "work/proj-wt/p.go")
	// ...and that sibling then moves onto a branch of its own, so it no longer
	// holds anything the polled checkout commits.
	wtGit, _ := gitIn(t, wt)
	wtGit("checkout", "-b", "feat")
	writeCommitFile(t, wt, "feat.go", "f1\n")
	wtGit("add", "-A")
	wtGit("commit", "-m", "feat work")

	primaryKey := gitWatchRootKey(primary)
	mutateDurabilityLedger(func(led *durabilityLedger) {
		tombstoneSeededPath(led, primaryKey, "p.go", 500)
	})

	// Any commit at all in the polled checkout runs the pruner.
	git, gitOut := gitIn(t, primary)
	writeCommitFile(t, primary, "other.go", "o1\n")
	git("add", "-A")
	git("commit", "-m", "an unrelated commit")
	sha := gitOut("rev-parse", "HEAD")
	pollDurabilityCommit(primary, primaryKey, Session{DeviceID: "dev-primary", TaskRoot: home}, sha, t0,
		newSiblingLineage(primary, []string{sha}))

	led := loadDurabilityLedger()
	if _, ok := led.Seeded[primaryKey]["p.go"]; !ok {
		t.Fatalf("the tombstone for p.go was pruned while a sibling worktree still held its evidence: Seeded = %+v", led.Seeded)
	}
}

// The SECOND defect of that collapse, and the one that fabricates: a tombstone
// written through the narrow view records 0 — "there was never any per-write
// evidence here" — while a sibling still holds the real write stamp. The gate is
// STRICTLY NEWER, so that same already-spent write then clears it and seeds the
// path a second time, with a fresh lineage and a fresh birth stamp.
//
// The sibling deliberately moves off the commit for the churn-out and back onto
// it for the re-entry: that is exactly the window in which the two views disagree,
// and asserting the ABSENCE of the second seed is what distinguishes a blocked
// re-entry from a merely smaller one.
func TestDurabilitySpentEvidenceCannotSeedTwiceAcrossASiblingThatMoved(t *testing.T) {
	home, primary, _, _, _ := worktreeAttributionRepo(t)
	const t0 int64 = 1_000_000_000_000

	git, gitOut := gitIn(t, primary)
	writeCommitFile(t, primary, "p.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "p.go enters")
	seedSha := gitOut("rev-parse", "HEAD")

	// A second worktree created AT that commit, holding the only evidence for p.go.
	agentWt := filepath.Join(home, "work", "proj-agent")
	git("worktree", "add", "--detach", agentWt, seedSha)
	recordAiTouchedPath("sess-agent", gitWatchRootKey(home), "work/proj-agent/p.go")

	primaryKey := gitWatchRootKey(primary)
	sess := Session{DeviceID: "dev-primary", TaskRoot: home}
	pollDurabilityCommit(primary, primaryKey, sess, seedSha, t0, newSiblingLineage(primary, []string{seedSha}))
	if got := trackedLineCount(t, primaryKey, "p.go"); got != 3 {
		t.Fatalf("setup: p.go tracked = %d, want 3 — the sibling's evidence must seed the commit its HEAD holds", got)
	}

	// The agent worktree moves onto its own branch: from here on it does not hold
	// the polled checkout's commits, and only the WIDE view can see its stamp.
	agentGit, _ := gitIn(t, agentWt)
	agentGit("checkout", "-b", "feat")
	writeCommitFile(t, agentWt, "feat.go", "f1\n")
	agentGit("add", "-A")
	agentGit("commit", "-m", "feat work")

	// p.go is rewritten end to end, so every seeded span churns out and the path
	// leaves the ledger — the route that writes the tombstone.
	writeCommitFile(t, primary, "p.go", "x1\nx2\nx3\n")
	git("add", "-A")
	git("commit", "-m", "p.go fully rewritten")
	churnSha := gitOut("rev-parse", "HEAD")
	pollDurabilityCommit(primary, primaryKey, sess, churnSha, t0+1, newSiblingLineage(primary, []string{churnSha}))
	if got := trackedLineCount(t, primaryKey, "p.go"); got != 0 {
		t.Fatalf("setup: p.go tracked = %d after a full rewrite, want 0", got)
	}

	// A purely human append, and the agent worktree back on this line of history —
	// so the gate CAN see the sibling's stamp again. Nothing new was written by any
	// agent, so the tombstone must still hold.
	writeCommitFile(t, primary, "p.go", "x1\nx2\nx3\nh4\n")
	git("add", "-A")
	git("commit", "-m", "a human appends to p.go")
	reentrySha := gitOut("rev-parse", "HEAD")
	agentGit("checkout", "--detach", reentrySha)

	pollDurabilityCommit(primary, primaryKey, sess, reentrySha, t0+2, newSiblingLineage(primary, []string{reentrySha}))
	if n := trackedLineCount(t, primaryKey, "p.go"); n != 0 {
		t.Fatalf("already-spent AI evidence seeded p.go a second time: %d lines tracked, want 0 (ranges %+v)",
			n, ledgerRanges(t, primaryKey, "p.go"))
	}
}

// THE MIXED RANGE HAS TO SURVIVE THE WHOLE WAY DOWN, not just out of the gate.
//
// TestCommitsHeldByAnswersEachShaInAMixedRange already pins the gate's own answer:
// `commitsHeldBy` complements `rev-list <shas…> --not HEAD` per sha, and
// `siblingLineage.reaches` agrees with it, on a batch mixing held and unheld
// commits. This test starts where that one stops. `resolveLedgerScope` consults
// the gate ONCE PER COMMIT, so between a correct lineage and a correct verdict
// there is a step that a range-shaped answer can still be collapsed at — and
// collapsing it there is invisible to every assertion made against the lineage
// itself.
//
// So this reconciles TWO commits of ONE range through ONE lineage, with the
// sibling's HEAD sitting between them, and follows both through to the
// consequence:
//
//	positive — the sha the sibling HEAD reaches attributes to the recording
//	           session and seeds its lines;
//	negative — the sha it does NOT reach attributes to nothing. Absence on all
//	           three of attribution, session and durability spans, so a wrong
//	           answer and a missing answer cannot pass the same assertion.
//
// MUTATION-TESTED, and the mutation is the reason this exists rather than a
// second copy of the gate test. Make `resolveLedgerScope` ask a RANGE-level
// question — "does this sibling hold anything in the poll's range" instead of
// "does it hold THIS commit" — which is exactly what an optimisation that hoists
// the per-commit gate out of the commit loop would produce. It is
// behaviour-identical for a single-commit range, so it leaves EVERY OTHER TEST IN
// THE PACKAGE GREEN, including the gate test above; this is the only test that
// fails, and it fails in the fabrication direction: the unheld commit attributes
// `likely_ai`, carries a session it has no claim to, and seeds three of a human's
// lines as AI off a sibling that never held that commit.
func TestSiblingLineageSeparatesHeldFromUnheldWithinOneRange(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	const t0 int64 = 1_000_000_000_000

	home := t.TempDir()
	primary := filepath.Join(home, "repos", "proj")
	git, gitOut := gitRepoAt(t, primary)
	git("commit", "--allow-empty", "-m", "base")

	// Both files are the agent's work, and BOTH are recorded under the SIBLING's
	// path space — so neither resolves from the polled checkout's own key and the
	// lineage gate is the only thing that can decide either one.
	const sessionID = "sess-range"
	wtKeySpace := "work/proj-wt/"
	recordAiTouchedPath(sessionID, gitWatchRootKey(home), wtKeySpace+"held.go")
	recordAiTouchedPath(sessionID, gitWatchRootKey(home), wtKeySpace+"unheld.go")

	// Commit one, then park the sibling exactly on it.
	writeCommitFile(t, primary, "held.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "held.go")
	heldSha := gitOut("rev-parse", "HEAD")

	wt := filepath.Join(home, "work", "proj-wt")
	git("worktree", "add", "--detach", wt, heldSha)

	// Commit two lands AFTER the sibling was parked, so the sibling's HEAD reaches
	// heldSha and not this one — one range, one lineage, two answers.
	writeCommitFile(t, primary, "unheld.go", "u1\nu2\nu3\n")
	git("add", "-A")
	git("commit", "-m", "unheld.go")
	unheldSha := gitOut("rev-parse", "HEAD")

	// The geometry is asserted with RAW GIT, never through siblingLineage: a
	// precondition checked with the code under test would swallow the very
	// mutation this test exists to catch, failing in setup instead of in the two
	// assertions that name what actually went wrong.
	assertReaches(t, wt, heldSha, true)
	assertReaches(t, wt, unheldSha, false)

	// Exactly what a poll builds: ONE lineage for the WHOLE range.
	lin := newSiblingLineage(primary, []string{heldSha, unheldSha})

	held, heldSess := attributionWith(t, primary, home, heldSha, lin)
	if held["held.go"] != attributionLikelyAI {
		t.Errorf("held sha in a mixed range: held.go = %q, want %q — the sibling's HEAD reaches this commit, so its evidence counts",
			held["held.go"], attributionLikelyAI)
	}
	if heldSess != sessionID {
		t.Errorf("held sha in a mixed range: session = %q, want %q", heldSess, sessionID)
	}

	unheld, unheldSess := attributionWith(t, primary, home, unheldSha, lin)
	if unheld["unheld.go"] != attributionUnknown {
		t.Errorf("unheld sha in a mixed range: unheld.go = %q, want %q — a sibling that does not hold the commit is not evidence about it",
			unheld["unheld.go"], attributionUnknown)
	}
	if unheldSess != "" {
		t.Errorf("unheld sha in a mixed range: session = %q, want no session at all", unheldSess)
	}

	primaryKey := gitWatchRootKey(primary)
	sess := Session{DeviceID: "dev-range", TaskRoot: home}
	pollDurabilityCommit(primary, primaryKey, sess, heldSha, t0, lin)
	pollDurabilityCommit(primary, primaryKey, sess, unheldSha, t0+1, lin)

	if n := trackedLineCount(t, primaryKey, "held.go"); n != 3 {
		t.Errorf("held sha in a mixed range: held.go tracked = %d, want 3", n)
	}
	if n := trackedLineCount(t, primaryKey, "unheld.go"); n != 0 {
		t.Errorf("unheld sha in a mixed range: %d lines seeded off a sibling that does not hold the commit, want 0 (ranges %+v)",
			n, ledgerRanges(t, primaryKey, "unheld.go"))
	}
}
