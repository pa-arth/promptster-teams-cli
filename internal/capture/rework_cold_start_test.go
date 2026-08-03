package capture

import (
	"fmt"
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
