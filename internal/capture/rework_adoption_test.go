package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Adopting a branch whose commits this device ALREADY attributed used to lose
// that branch's rework state permanently.
//
// The two halves compose into a silent loss. adoptReworkBranch drops the root's
// tracked spans, tombstones and branch on every branch change — correct, since
// the state describes one branch. The working-HEAD loop then skips any SHA
// already in the attributed ledger, and that skip covers rework as well as
// attribution. So the commits that would rebuild the adopted branch's spans are
// exactly the ones never replayed: the branch comes back with an empty ledger,
// and every later rewrite of its AI lines emits no rework_verdict at all.
//
// This is the LOSS direction, not the fabrication direction — it under-reports
// AI work rather than inventing it — but it is the same class the rest of this
// branch closes: real work recorded as if it never happened. Both routes the
// review finding names are pinned below, plus the two things the fix must NOT
// do (re-attribute a commit, or re-emit its verdicts).

// reworkVerdictPaths returns the path of every rework_verdict on the outbox, in
// order, so a test can assert on REPEATS rather than mere presence — a rebuild
// that re-emitted a replayed commit's verdicts would double-count the churn the
// attribution skip exists to prevent.
func reworkVerdictPaths(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(state.OutboxPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read outbox: %v", err)
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev event.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal outbox line: %v", err)
		}
		if ev.Kind != "rework_verdict" {
			continue
		}
		d, ok := ev.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("rework_verdict Data is %T, want map", ev.Data)
		}
		p, _ := d["path"].(string)
		paths = append(paths, p)
	}
	return paths
}

// adoptionRepo stands up a repo on main with one baseline commit, an outbox to
// assert against, and a cold-start poll already taken.
func adoptionRepo(t *testing.T) (ws string, git func(...string), gitOut func(...string) string, key string, sess Session) {
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
	sess = Session{DeviceID: "dev-adopt", TaskRoot: ws}
	pollGitWatchWorkspace(sess) // cold-start baseline on main
	return ws, git, gitOut, key, sess
}

// TestReworkAdoptionRebuildsSpansAfterAnEarlierCheckout is the first route the
// finding names, reproduced end-to-end through the real watcher loop with no
// planted state: work on a feature branch, park on main, come back.
//
// The visit to main clears the root (correct — on the default branch those lines
// are durability's). Coming back re-detects the branch's commits, but they are
// already attributed, so the skip drops them before pollReworkCommit and the
// spans are never rebuilt. The rewrite that follows is real AI rework and used to
// be reported as nothing at all.
func TestReworkAdoptionRebuildsSpansAfterAnEarlierCheckout(t *testing.T) {
	ws, git, gitOut, key, sess := adoptionRepo(t)

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-adopt", key, "a.go")
	recordAiTouchedPath("sess-adopt", key, "b.go")
	writeCommitFile(t, ws, "a.go", "l1\nl2\nl3\n")
	writeCommitFile(t, ws, "b.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds a.go and b.go")
	sha1 := gitOut("rev-parse", "HEAD")
	pollGitWatchWorkspace(sess)
	if c := reworkCovered(key, "a.go"); len(c) != 3 {
		t.Fatalf("setup: want a.go seeded with 3 lines, got %+v", c)
	}

	// One of them is reworked BEFORE the branch switch, so its verdict is already
	// on the wire. Anything the replay emits later is a duplicate of this.
	writeCommitFile(t, ws, "a.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "rewrite a.go")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("setup: rework_verdicts = %v, want exactly one for a.go", got)
	}

	// Park on the default branch. This clear is correct and must stay.
	git("checkout", "main")
	pollGitWatchWorkspace(sess)
	if len(loadReworkLedger().Roots[key]) != 0 {
		t.Fatal("setup: standing on main must clear the root's rework state")
	}

	// Back to the branch. Both commits are already attributed, so the loop skips
	// them — and used to leave the branch holding nothing.
	git("checkout", "feature")
	pollGitWatchWorkspace(sess)
	if c := reworkCovered(key, "b.go"); len(c) != 3 {
		t.Fatalf("adopted branch did not get its AI spans back: covered=%+v", c)
	}

	// The loss that actually reaches the product: rewriting the surviving AI lines.
	writeCommitFile(t, ws, "b.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "rewrite b.go")
	pollGitWatchWorkspace(sess)

	// Exactly one verdict per real rework. A replay that emitted what it folded
	// would re-ship a.go's churn here — the double-count the skip exists to stop.
	if got := reworkVerdictPaths(t); len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Fatalf("rework_verdicts = %v, want exactly [a.go b.go]", got)
	}

	// The rebuild must not weaken the skip it works around: the replayed commit
	// stays attributed exactly once.
	if n := countSha(attributedShas(t, state.OutboxPath()), sha1); n != 1 {
		t.Errorf("commit %s attributed %d times, want exactly 1", sha1, n)
	}
}

// TestReworkReplayOnlyRunsOnAdoption pins the guard that makes the replay safe.
//
// Replaying is only sound because adoption has just emptied the root, so there is
// no live state to apply a commit to twice. gitNewCommits' recovery path
// re-surfaces the newest commits WHOLESALE whenever a cursor becomes unreachable
// (a rebase, a pruned worktree, a gc) with no branch change at all, and those
// commits are skipped too. Replaying one of those re-applies the seeding commit's
// own insertion hunk to spans that already exist and slides them clean out of the
// file's real line space — a fabrication, reported against lines nobody wrote.
func TestReworkReplayOnlyRunsOnAdoption(t *testing.T) {
	ws, git, _, key, sess := adoptionRepo(t)

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-recover", key, "steady.go")
	writeCommitFile(t, ws, "steady.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds steady.go")
	pollGitWatchWorkspace(sess)
	before := loadReworkLedger().Roots[key]["steady.go"]
	if len(before) != 1 || before[0].Start != 1 || before[0].End != 3 {
		t.Fatalf("setup: want a single 1..3 span, got %+v", before)
	}
	// The authorization must not simply become permanent to survive a multi-poll
	// range: this root drained in the poll that adopted it, so it owes nothing.
	if loadReworkLedger().Adopting[key] {
		t.Fatal("setup: a drained root still owes a replay")
	}

	// Point the cursor at an object that is not in the repo — what a rebase or a
	// pruned worktree leaves behind — so the commit re-surfaces and is skipped
	// with the branch unchanged.
	bogus := map[string]string{}
	for k := range loadGitWatchCursors() {
		bogus[k] = "0000000000000000000000000000000000000000"
	}
	saveGitWatchCursors(bogus)
	pollGitWatchWorkspace(sess)

	after := loadReworkLedger().Roots[key]["steady.go"]
	if len(after) != 1 || after[0].Start != 1 || after[0].End != 3 {
		t.Fatalf("a non-adopting recovery poll moved the tracked spans: %+v -> %+v", before, after)
	}
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Errorf("a recovery poll invented rework_verdicts: %v", got)
	}
}

// TestReworkAdoptionRebuildsSpansAttributedByAnotherWorktree is the second route,
// and the one that matters most in practice: several worktrees against one
// repository.
//
// The attributed-commits ledger is keyed by SHA ALONE and shared across roots
// (loadAttributedCommits, one read per poll), precisely so a commit reachable
// from a repo and from its worktrees is attributed once. The rework ledger is
// keyed by ROOT. So a worktree that adopts a branch another worktree already
// attributed skips every one of its commits while holding no spans for them —
// the loss with no branch-switch involved at all. Recording the SHA is exactly
// what the other worktree's poll does, and is what makes this root skip it.
func TestReworkAdoptionRebuildsSpansAttributedByAnotherWorktree(t *testing.T) {
	ws, git, gitOut, key, sess := adoptionRepo(t)

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-adopt-wt", key, "shared.go")
	writeCommitFile(t, ws, "shared.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds shared.go")
	sha1 := gitOut("rev-parse", "HEAD")

	// Another worktree on this repo attributed and reworked that commit before
	// this root ever polled the branch.
	recordAttributedCommits([]string{sha1}, time.Now().UnixMilli())
	pollGitWatchWorkspace(sess)
	if c := reworkCovered(key, "shared.go"); len(c) != 3 {
		t.Fatalf("branch adopted from another worktree has no AI spans: covered=%+v", c)
	}

	writeCommitFile(t, ws, "shared.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "rewrite shared.go")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "shared.go" {
		t.Fatalf("rework_verdicts = %v, want exactly one for shared.go", got)
	}

	// The other worktree already emitted this commit's attribution; replaying it
	// for state must not emit a second one.
	if n := countSha(attributedShas(t, state.OutboxPath()), sha1); n != 0 {
		t.Errorf("commit %s attributed %d times by this root, want 0 — the other worktree owns it", sha1, n)
	}
}

// ── The replay outlives the poll that authorized it ──────────────────────────
//
// Adoption empties the root, but the commits that rebuild it are handed over by
// pollGitWatch under two caps — a per-poll budget shared across roots, and a
// per-root burst clamp — so an adopted branch's range routinely spans several
// polls. Every poll after the first sees an UNCHANGED branch, so an
// authorization derived from "this poll changed the branch" expires with the
// range still half-replayed. The two tests below are the two ways that happens,
// and they fail differently: one loses the branch outright, the other leaves
// spans stranded mid-branch for a later commit to churn into a verdict about
// lines nobody wrote.

// TestReworkAdoptionReplaysAfterABudgetDeferredPoll is the LOSS shape. The
// adopting poll reaches this root with the shared budget already spent, so the
// root is deferred WHOLE: its cursor does not move and it surfaces no commits at
// all — after adoption has already dropped its state. Every later poll sees the
// branch it already recorded, so nothing ever replays and the branch keeps an
// empty ledger permanently.
func TestReworkAdoptionReplaysAfterABudgetDeferredPoll(t *testing.T) {
	ws, git, gitOut, key, sess := adoptionRepo(t)

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-defer", key, "a.go")
	writeCommitFile(t, ws, "a.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds a.go")
	sha1 := gitOut("rev-parse", "HEAD")

	// Another worktree already attributed it, so this root will skip it.
	recordAttributedCommits([]string{sha1}, time.Now().UnixMilli())

	// The adopting poll: the budget is gone before this root is reached, so
	// pollGitWatch defers it untouched and the loop never sees a commit for it.
	orig := gitWatchMaxCommitsPerPollTotal
	defer func() { gitWatchMaxCommitsPerPollTotal = orig }()
	gitWatchMaxCommitsPerPollTotal = 0
	pollGitWatchWorkspace(sess)
	gitWatchMaxCommitsPerPollTotal = orig

	if c := reworkCovered(key, "a.go"); len(c) != 0 {
		t.Fatalf("setup: a deferred root cannot have folded anything yet, got %+v", c)
	}

	// The deferred commit drains on the next poll — with the branch unchanged.
	pollGitWatchWorkspace(sess)
	if c := reworkCovered(key, "a.go"); len(c) != 3 {
		t.Fatalf("the deferred range never replayed, so the branch holds no AI spans: covered=%+v", c)
	}

	// The loss that reaches the product: rewriting those AI lines.
	writeCommitFile(t, ws, "a.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "rewrite a.go")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("rework_verdicts = %v, want exactly one for a.go", got)
	}
}

// TestReworkAdoptionClampedBurstDoesNotStrandSpansMidBranch is the FABRICATION
// shape, and it is worse than the loss: the adopting poll replays only the
// OLDEST slice of the branch (clampCommitBurst keeps the tail and advances the
// cursor to it), so the ledger ends up holding spans positioned as of the middle
// of the branch while the working tree stands at its head. Those coordinates
// address whatever happens to live there now — human lines — and the next commit
// that rewrites them emits a rework_verdict about code the AI never wrote.
//
// The geometry is built so the two states are distinguishable and only one of
// them is right: the AI span sits at 21..23 after the first commit and at 31..33
// after the second, and the final commit rewrites head lines 21..23, which are
// human filler. A correct ledger reports NOTHING for it. Asserting absence is
// the point — a wrong count and a missing count must not pass the same check.
func TestReworkAdoptionClampedBurstDoesNotStrandSpansMidBranch(t *testing.T) {
	origPerRoot := gitWatchMaxCommitsPerPoll
	gitWatchMaxCommitsPerPoll = 1 // one commit per poll: the branch drains over two
	defer func() { gitWatchMaxCommitsPerPoll = origPerRoot }()

	ws, git, gitOut, key, sess := adoptionRepo(t)

	// 20 human filler lines land on the default branch, so they are never part of
	// the range the adoption replays.
	var filler strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&filler, "f%d\n", i)
	}
	writeCommitFile(t, ws, "a.go", filler.String())
	git("add", "-A")
	git("commit", "-m", "human writes a.go")
	pollGitWatchWorkspace(sess)

	git("checkout", "-b", "feature")

	// c1: the AI appends three lines, far below the top of the file → span 21..23.
	recordAiTouchedPath("sess-clamp", key, "a.go")
	writeCommitFile(t, ws, "a.go", filler.String()+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai appends to a.go")
	sha1 := gitOut("rev-parse", "HEAD")

	// c2: a human prepends ten lines. That hunk is nowhere near the AI span, so it
	// SHIFTS it rather than churning it → 31..33.
	var head strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&head, "h%d\n", i)
	}
	writeCommitFile(t, ws, "a.go", head.String()+filler.String()+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human prepends to a.go")
	sha2 := gitOut("rev-parse", "HEAD")

	recordAttributedCommits([]string{sha1, sha2}, time.Now().UnixMilli())

	pollGitWatchWorkspace(sess) // adopts, replays only the oldest commit (c1)
	pollGitWatchWorkspace(sess) // c2 drains here, with the branch unchanged

	spans := loadReworkLedger().Roots[key]["a.go"]
	if len(spans) != 1 || spans[0].Start != 31 || spans[0].End != 33 {
		t.Errorf("the adopted branch's spans are not where its head put them: %+v", spans)
	}

	// Rewrite head lines 21..23 — human filler, and exactly where the stranded
	// spans would have been.
	var reworked strings.Builder
	for i := 1; i <= 20; i++ {
		if i >= 11 && i <= 13 {
			fmt.Fprintf(&reworked, "x%d\n", i)
			continue
		}
		fmt.Fprintf(&reworked, "f%d\n", i)
	}
	writeCommitFile(t, ws, "a.go", head.String()+reworked.String()+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)

	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("rework_verdict emitted over lines the AI never wrote: %v", got)
	}
}
