package capture

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// A NEW LOCAL COPY used to lose the branch's whole AI history.
//
// `git worktree add` produces a new absolute path, which is a new
// gitWatchRootKey, which pollGitWatch COLD-STARTS: the cursor is baselined
// straight to head and none of the branch's existing commits are surfaced.
// adoptReworkBranch still fires (a new key has no recorded branch), so the root
// declares its replay owed and finishes it in the same poll having replayed
// nothing — the branch reads as holding no AI work at all in that copy, and
// every later rewrite of its AI lines emits no rework_verdict.
//
// It is the same silent under-report PR #128 closed for the adoption route,
// reached through cold start instead of through the attributed-commits skip.
// TestReworkAdoptionRebuildsSpansAttributedByAnotherWorktree does NOT cover it:
// that test plants the attributed SHA on a root that has ALREADY been polled, so
// the root has a cursor and takes the ordinary detect-then-skip path. The tests
// below never poll before the branch exists, so the very first poll is the
// cold start.
//
// WHAT THESE TESTS EXERCISE IS THE ROUTE, NOT AN END-TO-END PAYOFF. They stand a
// cursorless root up inside ONE directory, so the AI-path evidence they record is
// keyed to the very root that polls it. A genuine second directory is a different
// key, and AI-path evidence is still recorded under whichever checkout the agent
// actually edited in (the known gap in AGENTS.md), so a real `git worktree add`
// replays exactly these commits and finds no AI ranges to seed. Read every
// assertion below as "the replay ran, over the right range, positioned at head" —
// not as "a fresh worktree now shows the branch's spans".

// coldStartRepo stands up a repo on main with one baseline commit and an outbox
// to assert against, and — the whole point — takes NO poll, so the root has no
// cursor at all when the test finally polls it.
func coldStartRepo(t *testing.T) (ws string, git func(...string), gitOut func(...string) string, key string, sess Session) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))

	ws, git, gitOut = gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key = gitWatchRootKey(ws)
	sess = Session{DeviceID: "dev-cold", TaskRoot: ws}
	return ws, git, gitOut, key, sess
}

// coldStartBranch builds the shared geometry: 20 human filler lines on the
// default branch, then a `feature` branch holding an AI append (span 21..23) and
// a human prepend that shifts it to 31..33. Both branch commits are recorded as
// already attributed by another copy of the repository, which is what makes this
// root's replay a RECOVERY rather than an import. It returns them newest-last.
func coldStartBranch(t *testing.T, ws string, git func(...string), gitOut func(...string) string, key string) (filler, prepended string, sha1, sha2 string) {
	t.Helper()
	filler = fillerLines("f", 20)
	writeCommitFile(t, ws, "a.go", filler)
	git("add", "-A")
	git("commit", "-m", "human writes a.go")

	git("checkout", "-b", "feature")

	recordAiTouchedPath("sess-cold", key, "a.go")
	writeCommitFile(t, ws, "a.go", filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai appends to a.go")
	sha1 = gitOut("rev-parse", "HEAD")

	prepended = fillerLines("h", 10)
	writeCommitFile(t, ws, "a.go", prepended+filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human prepends to a.go")
	sha2 = gitOut("rev-parse", "HEAD")

	recordAttributedCommits([]string{sha1, sha2}, time.Now().UnixMilli())
	return filler, prepended, sha1, sha2
}

// wantOneSpan asserts the root tracks EXACTLY one AI range for path, at start..end.
// Coordinates rather than a count: a replay that folded the wrong range, or folded
// one twice, lands somewhere else rather than losing the span outright.
func wantOneSpan(t *testing.T, key, path string, start, end int, what string) {
	t.Helper()
	spans := loadReworkLedger().Roots[key][path]
	if len(spans) != 1 || spans[0].Start != start || spans[0].End != end {
		t.Fatalf("%s: spans = %+v, want exactly one at %d..%d", what, spans, start, end)
	}
}

// commitAt commits the index with an explicit committer AND author date, which is
// the only way to build the clock skew `git rev-list`'s default ordering is
// sensitive to. gitRepo's own closure pins the identity but not the clock.
func commitAt(t *testing.T, ws, date, msg string) {
	t.Helper()
	cmd := exec.Command("git", "-C", ws, "commit", "-m", msg)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
	)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit %q at %s: %v\n%s", msg, date, err, o)
	}
}

// fillerLines renders n numbered lines with the given prefix, so a test can build
// a file whose AI and human regions are told apart by line number alone.
func fillerLines(prefix string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s%d\n", prefix, i)
	}
	return b.String()
}

// TestReworkColdStartWorktreeRecoversTheBranchesAiHistory is the defect, end to
// end through the real watcher loop on a root that has never been polled.
//
// The geometry is built so a right answer and a wrong one are DIFFERENT
// coordinates rather than different counts: the AI span sits at 21..23 after c1
// and at 31..33 after c2, so a replay that stopped mid-branch is visible, and the
// two rewrites at the end separate recovery from fabrication. Rewriting head
// lines 21..23 — human filler — must report NOTHING; rewriting 31..33, the AI's
// own lines, must report exactly one verdict. Asserting the ABSENCE is half the
// test: a wrong number and a missing number must not pass the same check.
func TestReworkColdStartWorktreeRecoversTheBranchesAiHistory(t *testing.T) {
	ws, git, gitOut, key, sess := coldStartRepo(t)

	// 20 human filler lines land on the default branch, so they are never part of
	// the range the cold start replays.
	filler := fillerLines("f", 20)
	writeCommitFile(t, ws, "a.go", filler)
	git("add", "-A")
	git("commit", "-m", "human writes a.go")

	git("checkout", "-b", "feature")

	// c1: the AI appends three lines, far below the top of the file → span 21..23.
	recordAiTouchedPath("sess-cold", key, "a.go")
	writeCommitFile(t, ws, "a.go", filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai appends to a.go")
	sha1 := gitOut("rev-parse", "HEAD")

	// c2: a human prepends ten lines. That hunk is nowhere near the AI span, so it
	// SHIFTS it rather than churning it → 31..33.
	head := fillerLines("h", 10)
	writeCommitFile(t, ws, "a.go", head+filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human prepends to a.go")
	sha2 := gitOut("rev-parse", "HEAD")

	// Another copy of this repository — the worktree this one was cut from —
	// attributed both commits before this root ever existed. That is what makes
	// this root skip them, and it is the only trace of them this device holds.
	recordAttributedCommits([]string{sha1, sha2}, time.Now().UnixMilli())

	// The FIRST poll this root has ever had: a genuine cold start.
	if _, hadCursor := loadGitWatchCursors()[key]; hadCursor {
		t.Fatal("setup: the root must have no cursor — this test is the cold-start route")
	}
	pollGitWatchWorkspace(sess)

	spans := loadReworkLedger().Roots[key]["a.go"]
	if len(spans) != 1 || spans[0].Start != 31 || spans[0].End != 33 {
		t.Fatalf("the new copy did not recover the branch's AI spans at its head: %+v", spans)
	}

	// The replay is for STATE only. The copy that made these commits already
	// attributed them; re-attributing here is the double-count the skip prevents.
	if n := countSha(attributedShas(t, state.OutboxPath()), sha1); n != 0 {
		t.Errorf("commit %s attributed %d times by this root, want 0 — the other copy owns it", sha1, n)
	}
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("the replay emitted verdicts for a range already accounted for: %v", got)
	}

	// FABRICATION CHECK. Rewrite head lines 21..23 — human filler, and exactly
	// where a replay that stopped after c1 would have left the AI span. A correct
	// ledger reports nothing at all for this commit.
	reworked := strings.Builder{}
	for i := 1; i <= 20; i++ {
		if i >= 11 && i <= 13 {
			fmt.Fprintf(&reworked, "x%d\n", i)
			continue
		}
		fmt.Fprintf(&reworked, "f%d\n", i)
	}
	writeCommitFile(t, ws, "a.go", head+reworked.String()+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("rework_verdict emitted over lines the AI never wrote: %v", got)
	}

	// RECOVERY CHECK, and the loss that actually reaches the product: rewriting
	// the AI's own lines must now report, where before the fix it reported nothing.
	writeCommitFile(t, ws, "a.go", head+reworked.String()+"z1\nz2\nz3\n")
	git("add", "-A")
	git("commit", "-m", "rewrite the ai lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("rework_verdicts = %v, want exactly one for a.go", got)
	}

	// The recovered branch must not leave the root permanently authorized to fold
	// commits into live state — that authorization is what makes a cursor-recovery
	// batch re-apply hunks it has already applied.
	if loadReworkLedger().Adopting[key] {
		t.Error("the cold-start replay left the root still owing one")
	}
	if n := countSha(attributedShas(t, state.OutboxPath()), sha2); n != 0 {
		t.Errorf("commit %s attributed %d times by this root, want 0", sha2, n)
	}
}

// TestReworkColdStartFreshInstallReplaysNothing is the other half of the
// contract, and the one that keeps the fix from becoming a worse bug than the one
// it closes.
//
// A genuinely fresh install has an empty attributed-commits ledger: it has never
// measured any of this branch's commits, so there is no history to RECOVER — only
// history to IMPORT, which cold start has always and deliberately refused. The
// replay is gated on this device already holding attribution for at least one
// commit in the range precisely so that machine takes exactly the path it took
// before: baseline, silence, no `git show` over the branch.
//
// The branch here is identical to the one above (same AI evidence, same shape);
// the ONLY difference is that nothing was ever attributed.
func TestReworkColdStartFreshInstallReplaysNothing(t *testing.T) {
	ws, git, _, key, sess := coldStartRepo(t)

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-fresh", key, "a.go")
	writeCommitFile(t, ws, "a.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds a.go")

	// No recordAttributedCommits: this machine has never seen this repo.
	pollGitWatchWorkspace(sess)

	if c := reworkCovered(key, "a.go"); len(c) != 0 {
		t.Fatalf("a fresh install replayed pre-existing history it never measured: covered=%+v", c)
	}
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("a fresh install emitted verdicts on its baseline poll: %v", got)
	}

	// And it must still work forward from the baseline, exactly as before: the
	// NEXT commit is seeded and reworked normally.
	recordAiTouchedPath("sess-fresh", key, "b.go")
	writeCommitFile(t, ws, "b.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds b.go")
	pollGitWatchWorkspace(sess)
	if c := reworkCovered(key, "b.go"); len(c) != 3 {
		t.Fatalf("forward tracking broke after the baseline: covered=%+v", c)
	}
}

// TestReworkColdStartOnTheDefaultBranchReplaysNothing pins the scope of the
// replay to pre-merge work, which is the only thing rework tracks.
//
// A new copy checked out on the DEFAULT branch has no pre-merge range at all —
// its surviving AI lines are durability's, and durability keeps its own cursor
// and its own cold-start discipline. Seeding rework spans there would hand a
// future feature branch ranges it never wrote, which is the stale-range remap
// clearReworkLedger exists to prevent.
func TestReworkColdStartOnTheDefaultBranchReplaysNothing(t *testing.T) {
	ws, git, gitOut, key, sess := coldStartRepo(t)

	recordAiTouchedPath("sess-main", key, "a.go")
	writeCommitFile(t, ws, "a.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds a.go on main")
	recordAttributedCommits([]string{gitOut("rev-parse", "HEAD")}, time.Now().UnixMilli())

	pollGitWatchWorkspace(sess)

	if c := reworkCovered(key, "a.go"); len(c) != 0 {
		t.Fatalf("a cold start on the default branch seeded rework spans: covered=%+v", c)
	}
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("a cold start on the default branch emitted verdicts: %v", got)
	}
}

// TestReworkColdStartOverTheCapReplaysNothing pins the refusal, which is the
// one place this fix chooses a LARGER undercount on purpose.
//
// A cold-start replay is one-shot: the cursor is already at head, so no later
// poll re-surfaces anything the first one skipped. That makes a partial replay a
// replay that STARTS MID-BRANCH, and both ends of the cut fabricate rather than
// merely lose:
//
//   - the OLDEST slice — clampCommitBurst's direction, correct for a range that
//     drains over later polls — leaves the spans a commit behind head, at 21..23,
//     which at head is human filler. The next rewrite of those lines reports the
//     AI's work over code it never wrote.
//   - the NEWEST slice ends at head, but makes c2 look like a FIRST TOUCH of
//     a.go, so the ten lines a human prepended in it are seeded as AI by the
//     path-presence inference that is only correct at a branch's real base. This
//     was measured, not predicted: it is what the first draft of this fix did.
//
// So the range is taken whole or not at all. Under a cap of one commit this
// two-commit branch is refused: NO spans, and no verdict for either region. The
// branch under-reports exactly as it did before the fix, which is the direction
// these ledgers always resolve toward.
func TestReworkColdStartOverTheCapReplaysNothing(t *testing.T) {
	orig := gitWatchMaxCommitsPerPoll
	gitWatchMaxCommitsPerPoll = 1
	defer func() { gitWatchMaxCommitsPerPoll = orig }()

	ws, git, gitOut, key, sess := coldStartRepo(t)

	filler := fillerLines("f", 20)
	writeCommitFile(t, ws, "a.go", filler)
	git("add", "-A")
	git("commit", "-m", "human writes a.go")

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-clampcold", key, "a.go")
	writeCommitFile(t, ws, "a.go", filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai appends to a.go")
	sha1 := gitOut("rev-parse", "HEAD")

	head := fillerLines("h", 10)
	writeCommitFile(t, ws, "a.go", head+filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human prepends to a.go")
	sha2 := gitOut("rev-parse", "HEAD")

	recordAttributedCommits([]string{sha1, sha2}, time.Now().UnixMilli())
	pollGitWatchWorkspace(sess)

	// No span at all. This catches BOTH clamp directions at once: the oldest slice
	// would leave 21..23 here, the newest slice 1..10.
	if spans := loadReworkLedger().Roots[key]["a.go"]; len(spans) != 0 {
		t.Fatalf("an over-cap branch replayed a partial range: %+v", spans)
	}

	// Rewrite head lines 21..23 — where an oldest-slice clamp would have stranded
	// the AI span, and human filler at head. A correct ledger reports nothing.
	var reworked strings.Builder
	for i := 1; i <= 20; i++ {
		if i >= 11 && i <= 13 {
			fmt.Fprintf(&reworked, "x%d\n", i)
			continue
		}
		fmt.Fprintf(&reworked, "f%d\n", i)
	}
	writeCommitFile(t, ws, "a.go", head+reworked.String()+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)

	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("rework_verdict emitted over lines the AI never wrote: %v", got)
	}
}

// TestReworkColdStartReplaysTheBaselinedHeadNotAMovingOne pins the range against
// the SHA the cursor was baselined to.
//
// pollGitWatch reads head and saves the cursor; the replay runs much later in the
// same poll, after pollDurability has walked every root and the attributed-commits
// ledger has been read. A commit landing inside that window is invisible to the
// cursor and visible to a second read of `HEAD`, so a replay that resolves
// `default..HEAD` folds it while the cursor still sits behind it — and the NEXT
// poll, seeing it as new, folds its hunks again and slides every recovered span by
// an offset already applied. This test opens that window deliberately.
func TestReworkColdStartReplaysTheBaselinedHeadNotAMovingOne(t *testing.T) {
	ws, git, gitOut, key, sess := coldStartRepo(t)
	filler, prepended, _, sha2 := coldStartBranch(t, ws, git, gitOut, key)

	// The cursor half of the poll, exactly as pollGitWatchWorkspace runs it. The
	// SHA it hands back is the contract: the replay must resolve its range against
	// that commit, not against whatever `HEAD` says by the time it runs.
	_, _, coldStart := pollGitWatch([]string{ws})
	if coldStart[key] != sha2 {
		t.Fatalf("cold start reported head %q, want the SHA it cursored (%s)", coldStart[key], sha2)
	}

	// THE WINDOW: an engineer commits between the baseline and the replay.
	interloper := fillerLines("g", 5)
	writeCommitFile(t, ws, "a.go", interloper+prepended+filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human commits during the poll")

	nowMs := time.Now().UnixMilli()
	adoptReworkBranch(key, "feature")
	replayReworkForColdStartBranch(sess, ws, sha2, loadAttributedCommits(nowMs), nowMs)

	// The replay must stand where the cursor stands: at sha2, where the AI span is
	// 31..33. Folding the interloper here would put it at 36..38 a poll early.
	wantOneSpan(t, key, "a.go", 31, 33, "the replay folded past the SHA the cursor was baselined to")

	// And now the interloper drains through the ORDINARY path exactly once, which
	// is the half a live-HEAD replay breaks: having folded it already, this second
	// fold slides the span to 41..43 — past the end of a 38-line file.
	pollGitWatchWorkspace(sess)
	wantOneSpan(t, key, "a.go", 36, 38, "the interloper's hunks were folded twice")

	// FABRICATION CHECK. head is g1..g5, h1..h10, f1..f20, a1..a3; rewrite f16..f18
	// — human lines, immediately above the AI span. A correct ledger reports nothing.
	rewritten := strings.Builder{}
	for i := 1; i <= 20; i++ {
		if i >= 16 && i <= 18 {
			fmt.Fprintf(&rewritten, "x%d\n", i)
			continue
		}
		fmt.Fprintf(&rewritten, "f%d\n", i)
	}
	writeCommitFile(t, ws, "a.go", interloper+prepended+rewritten.String()+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("rework_verdict emitted over lines the AI never wrote: %v", got)
	}

	// RECOVERY CHECK: the AI's own lines, at 36..38, still report.
	writeCommitFile(t, ws, "a.go", interloper+prepended+rewritten.String()+"z1\nz2\nz3\n")
	git("add", "-A")
	git("commit", "-m", "rewrite the ai lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("rework_verdicts = %v, want exactly one for a.go", got)
	}
}

// TestReworkColdStartDoesNotRefoldASurvivingLedger pins the invariant the replay
// gate exists for: THE SAME COMMIT'S HUNKS ARE NEVER FOLDED TWICE INTO THE SAME
// LEDGER.
//
// The replay assumes the empty ledger adoptReworkBranch leaves behind. The
// `Adopting` marker does NOT say that — PR #128 deliberately made it outlive its
// poll, because an adopted branch's commits arrive over several polls. So a root
// can cold-start on a branch it has ALREADY recorded, with tracked spans still in
// place and the obligation still owed: the cursors file is best-effort and a
// version bump or an unreadable file reads as no cursors at all, while the rework
// ledger is a separate file that survives. Gating on the marker rather than on
// adoption having fired re-applies the whole range over spans it already holds.
func TestReworkColdStartDoesNotRefoldASurvivingLedger(t *testing.T) {
	ws, git, gitOut, key, sess := coldStartRepo(t)
	_, prepended, _, sha2 := coldStartBranch(t, ws, git, gitOut, key)

	// An earlier poll adopted this branch and rebuilt its state; an over-cap burst
	// leaves exactly this shape, with the obligation still owed.
	nowMs := time.Now().UnixMilli()
	adoptReworkBranch(key, "feature")
	replayReworkForColdStartBranch(sess, ws, sha2, loadAttributedCommits(nowMs), nowMs)
	wantOneSpan(t, key, "a.go", 31, 33, "setup")
	if !loadReworkLedger().Adopting[key] {
		t.Fatal("setup: the adoption obligation must still be owed")
	}

	// The CURSOR is what is lost, not the ledger — so the very next poll cold-starts
	// a root whose recorded branch already matches, and adoption does not fire.
	if _, hadCursor := loadGitWatchCursors()[key]; hadCursor {
		t.Fatal("setup: no poll has run, so there must be no cursor")
	}
	pollGitWatchWorkspace(sess)

	wantOneSpan(t, key, "a.go", 31, 33, "the cold-start replay folded the range a second time over a ledger that already held it")

	// FABRICATION CHECK. A second fold puts the span at 44..46 — past the end of a
	// 33-line file — and leaves head lines 31..33, the AI's own, untracked. Rewrite
	// the human filler first: a correct ledger reports nothing for it.
	rewritten := strings.Builder{}
	for i := 1; i <= 20; i++ {
		if i >= 11 && i <= 13 {
			fmt.Fprintf(&rewritten, "x%d\n", i)
			continue
		}
		fmt.Fprintf(&rewritten, "f%d\n", i)
	}
	writeCommitFile(t, ws, "a.go", prepended+rewritten.String()+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("rework_verdict emitted over lines the AI never wrote: %v", got)
	}

	// RECOVERY CHECK: rewriting the AI's own lines must still report exactly once.
	writeCommitFile(t, ws, "a.go", prepended+rewritten.String()+"z1\nz2\nz3\n")
	git("add", "-A")
	git("commit", "-m", "rewrite the ai lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("rework_verdicts = %v, want exactly one for a.go", got)
	}
}

// TestReworkColdStartSkewedClockFoldsTheBranchInHeadsLineSpace builds the shape
// that breaks reverse-commit-date ordering, from real committer dates.
//
// `git rev-list`'s default is reverse COMMIT-DATE order, which git does not
// promise is topological. A parent enters the traversal only when a child pops,
// so a linear chain is safe whatever the clocks say — but a FORK is not: with two
// children of P in the range, P can pop while the second child is still queued,
// and reversed that folds P AFTER its own child. This branch is that fork, with
// the side commit's committer clock a full four days behind the parent it was
// written on:
//
//	main ── P (ai appends a1..a3)          committed 2026-01-05
//	         ├── C1 (human prepends h1..h10)  committed 2026-01-10
//	         └── C2 (human appends t1..t5)    committed 2026-01-01  ← skew
//	              M = merge(C1, C2)            committed now
//
// Default order returns M, C1, P, C2. Reversed that folds C2 into an EMPTY
// ledger, so the five lines a human appended are seeded as AI by the first-touch
// inference, and P's real span is then blocked by its own tombstone — a
// fabricated span where the AI wrote nothing and no span where it did.
//
// The merge is not incidental either: gitCommitRawDiff reads every commit with
// `-m --first-parent`, so M's diff ALREADY carries the t-lines C2 added. Folding
// both applies the same hunks twice. `--topo-order --first-parent` returns
// M, C1, P — reversed, P then C1 then M, each diff composed onto the parent it
// was taken against, ending at head, where the AI span is 31..33.
func TestReworkColdStartSkewedClockFoldsTheBranchInHeadsLineSpace(t *testing.T) {
	ws, git, gitOut, key, sess := coldStartRepo(t)

	filler := fillerLines("f", 20)
	writeCommitFile(t, ws, "a.go", filler)
	git("add", "-A")
	git("commit", "-m", "human writes a.go")

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-skew", key, "a.go")
	writeCommitFile(t, ws, "a.go", filler+"a1\na2\na3\n")
	git("add", "-A")
	commitAt(t, ws, "2026-01-05T00:00:00", "ai appends to a.go")
	shaP := gitOut("rev-parse", "HEAD")

	// The side branch, committed on a machine whose clock runs BEHIND its parent's.
	git("checkout", "-b", "sidework")
	tail := fillerLines("t", 5)
	writeCommitFile(t, ws, "a.go", filler+"a1\na2\na3\n"+tail)
	git("add", "-A")
	commitAt(t, ws, "2026-01-01T06:00:00", "human appends to a.go on a skewed clock")
	shaC2 := gitOut("rev-parse", "HEAD")

	git("checkout", "feature")
	prepended := fillerLines("h", 10)
	writeCommitFile(t, ws, "a.go", prepended+filler+"a1\na2\na3\n")
	git("add", "-A")
	commitAt(t, ws, "2026-01-10T00:00:00", "human prepends to a.go")
	shaC1 := gitOut("rev-parse", "HEAD")

	git("merge", "--no-ff", "-m", "merge sidework", "sidework")
	shaM := gitOut("rev-parse", "HEAD")
	recordAttributedCommits([]string{shaP, shaC1, shaC2, shaM}, time.Now().UnixMilli())

	// The skew is the premise, so assert it rather than trusting the dates: git's
	// default ordering must actually put P ahead of its own child here.
	if order := gitBranchCommitsSinceDefault(ws, shaM); len(order) != 3 || order[0] != shaM || order[1] != shaC1 || order[2] != shaP {
		t.Fatalf("replay range = %v, want the first-parent chain %v newest-first", order, []string{shaM, shaC1, shaP})
	}

	if _, hadCursor := loadGitWatchCursors()[key]; hadCursor {
		t.Fatal("setup: the root must have no cursor — this test is the cold-start route")
	}
	pollGitWatchWorkspace(sess)

	// head is h1..h10, f1..f20, a1..a3, t1..t5 — the AI wrote 31..33 and nothing else.
	wantOneSpan(t, key, "a.go", 31, 33, "the skewed branch was folded out of head's line space")

	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("the replay emitted verdicts for a range already accounted for: %v", got)
	}

	// FABRICATION CHECK. Rewrite head lines 34..38 — the t-lines a human appended,
	// and exactly what a fold that started at C2 would have seeded as AI.
	churnedTail := fillerLines("x", 5)
	writeCommitFile(t, ws, "a.go", prepended+filler+"a1\na2\na3\n"+churnedTail)
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("rework_verdict emitted over lines the AI never wrote: %v", got)
	}

	// RECOVERY CHECK: the AI's own lines, at 31..33, report exactly once.
	writeCommitFile(t, ws, "a.go", prepended+filler+"z1\nz2\nz3\n"+churnedTail)
	git("add", "-A")
	git("commit", "-m", "rewrite the ai lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("rework_verdicts = %v, want exactly one for a.go", got)
	}
}
