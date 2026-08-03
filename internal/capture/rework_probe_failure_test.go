package capture

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// A COMMIT IS NEVER RECORDED AS ATTRIBUTED UNLESS ITS HUNKS WERE FOLDED.
//
// Deciding which commits sit on head's first-parent chain is a second `rev-list`,
// and "the probe says this commit is off the chain" and "the probe could not run"
// are different facts. Collapsed into one empty subset they INFLATE: nothing is
// foldable, so no hunk shifts a tracked span, while every commit is still
// attributed and written to the attributed-commits ledger — and a recorded commit
// is never revisited, so the spans stay at coordinates the file has moved past
// for good. The next rewrite of whatever now occupies them reports rework over
// code the AI never wrote.

// TestReworkProbeFailureDefersInsteadOfStrandingSpans drives that path through the
// real watcher loop: one poll cannot resolve the chain, and the requirement is that
// it leaves nothing behind — no attribution recorded, no span moved — so the very
// next poll does the whole job.
func TestReworkProbeFailureDefersInsteadOfStrandingSpans(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))

	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	filler := fillerLines("f", 20)
	writeCommitFile(t, ws, "a.go", filler)
	git("add", "-A")
	git("commit", "-m", "human writes a.go on main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev-probe", TaskRoot: ws}
	pollGitWatchWorkspace(sess)

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-probe", key, "a.go")
	writeCommitFile(t, ws, "a.go", filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai appends to a.go")
	pollGitWatchWorkspace(sess)
	wantOneSpan(t, key, "a.go", 21, 23, "setup: the ai commit did not seed its own span")

	// A human prepends ten lines, so head is p1..p10, f1..f20, a1..a3 and the AI
	// wrote 31..33. A poll that records this commit without folding it leaves the
	// span at 21..23, which at head is f11..f13 — human filler.
	prefix := fillerLines("p", 10)
	writeCommitFile(t, ws, "a.go", prefix+filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human prepends ten lines")
	shaPrepend := gitOut("rev-parse", "HEAD")

	restore := gitFirstParentProbe
	gitFirstParentProbe = func(string, string) (map[string]struct{}, bool) { return nil, false }
	pollGitWatchWorkspace(sess)
	gitFirstParentProbe = restore

	if n := countSha(attributedShas(t, state.OutboxPath()), shaPrepend); n != 0 {
		t.Fatalf("the prepend commit was attributed %d time(s) on a poll that could fold nothing, want 0", n)
	}
	wantOneSpan(t, key, "a.go", 21, 23, "a poll that folded nothing still moved the tracked span")

	// The retry, with a working probe: the deferred commit is attributed exactly
	// once and its hunks land, so the span follows the AI text to 31..33.
	pollGitWatchWorkspace(sess)
	if n := countSha(attributedShas(t, state.OutboxPath()), shaPrepend); n != 1 {
		t.Fatalf("the prepend commit was attributed %d time(s) after the retry, want exactly 1", n)
	}
	wantOneSpan(t, key, "a.go", 31, 33, "the retry did not fold the commit the failed probe deferred")

	// FABRICATION CHECK: rewrite f11..f13 in place — head lines 21..23, exactly
	// where a stranded span would still be pointing, and lines the AI never wrote.
	var churned strings.Builder
	for i := 1; i <= 20; i++ {
		if i >= 11 && i <= 13 {
			fmt.Fprintf(&churned, "x%d\n", i)
			continue
		}
		fmt.Fprintf(&churned, "f%d\n", i)
	}
	writeCommitFile(t, ws, "a.go", prefix+churned.String()+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("rework_verdict emitted over lines the AI never wrote: %v", got)
	}

	// RECOVERY CHECK: the AI's own lines, at 31..33, report exactly once — so a
	// missing verdict and a misplaced one cannot pass the same pair of checks.
	writeCommitFile(t, ws, "a.go", prefix+churned.String()+"z1\nz2\nz3\n")
	git("add", "-A")
	git("commit", "-m", "rewrite the ai lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("rework_verdicts = %v, want exactly one for a.go", got)
	}
}

// TestGcdCursorRecoveryProbesAWindowCoveringEveryReturnedCommit pins the other half
// of the same rule: absence from the foldable subset must mean the commit is
// genuinely off the first-parent chain, never that it fell outside a probe window
// scoped differently from the list it describes. The gc'd-cursor recovery path is
// where the two used to be bounded differently.
func TestGcdCursorRecoveryProbesAWindowCoveringEveryReturnedCommit(t *testing.T) {
	ws, _, gitOut, _, _, shaS1, _, shaM := mergeFoldRepo(t)

	commits, foldable, ok := gitNewCommits(ws, strings.Repeat("0", 40), shaM)
	if !ok {
		t.Fatalf("gitNewCommits declined the gc'd-cursor recovery path")
	}
	if len(commits) == 0 {
		t.Fatalf("gc'd-cursor recovery returned no commits")
	}

	chain := map[string]struct{}{}
	for _, sha := range parseRevListShas([]byte(gitOut("rev-list", "--first-parent", shaM))) {
		chain[sha] = struct{}{}
	}
	for _, sha := range commits {
		_, gotFoldable := foldable[sha]
		_, onChain := chain[sha]
		if gotFoldable != onChain {
			t.Errorf("commit %s: foldable=%v, on head's first-parent chain=%v", sha, gotFoldable, onChain)
		}
	}

	// The subset must still be a real narrowing, or the loop above would pass on a
	// probe that simply called everything foldable.
	if _, got := foldable[shaS1]; got {
		t.Errorf("the second-parent commit %s is foldable, want it narrowed out", shaS1)
	}
	if countSha(commits, shaS1) != 1 {
		t.Errorf("the second-parent commit %s is not in the detected range at all", shaS1)
	}
}
