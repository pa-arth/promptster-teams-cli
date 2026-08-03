package capture

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// AN ORDINARY `git merge sidebranch` USED TO FOLD THE SAME EDITS TWICE.
//
// gitCommitRawDiff reads every commit with `-m --first-parent`, so a merge's own
// diff already carries everything the second-parent side brought in. The live
// range is `rev-list lastSeen..head`, which returns the merge AND the side
// commits, so the side branch's hunks were applied once in the side branch's
// coordinate space and then again in the merge's — sliding every tracked AI span
// by an offset already applied, until it addressed lines nobody wrote.
//
// The split that fixes it is deliberate and asymmetric, and both halves are
// pinned below: ATTRIBUTION EMISSION keeps the full range (a second-parent commit
// is still attributed exactly as before), and only THE REWORK FOLD is narrowed to
// the first-parent chain.

// mergeFoldRepo builds the shape: a tracked `feature` branch whose AI span is
// seeded by a live poll, then a side branch and a real merge, arranged so the
// side branch's hunk lands ABOVE the AI span and a double fold is therefore a
// different COORDINATE rather than a different count.
//
//	main ── c1 (ai appends a1..a3, span 21..23, seeded by its own poll)
//	         ├── s1  (human prepends p1..p5)   ← reachable only via the 2nd parent
//	         └── c2  (human appends w1..w4)
//	              M = merge(c2, s1)            ← head, a.go is 32 lines
//
// At head a.go is p1..p5, f1..f20, a1..a3, w1..w4, so the AI wrote 26..28.
// Folding the first-parent chain (c2 then M) lands there. Folding s1 as well
// applies the same five-line prepend twice and lands at 35..37, past the end of
// the file.
func mergeFoldRepo(t *testing.T) (ws string, git func(...string), gitOut func(...string) string, key string, sess Session, shaS1, shaC2, shaM string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))

	ws, git, gitOut = gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	filler := fillerLines("f", 20)
	writeCommitFile(t, ws, "a.go", filler)
	git("add", "-A")
	git("commit", "-m", "human writes a.go on main")

	key = gitWatchRootKey(ws)
	sess = Session{DeviceID: "dev-merge", TaskRoot: ws}

	// Baseline poll, then the AI commit gets its OWN poll so the span is seeded
	// through the live path — the merge below has to land in the live range, not
	// in a cold-start replay.
	pollGitWatchWorkspace(sess)

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-merge", key, "a.go")
	writeCommitFile(t, ws, "a.go", filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai appends to a.go")
	pollGitWatchWorkspace(sess)
	wantOneSpan(t, key, "a.go", 21, 23, "setup: the ai commit did not seed its own span")

	git("checkout", "-b", "side")
	prefix := fillerLines("p", 5)
	writeCommitFile(t, ws, "a.go", prefix+filler+"a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "human prepends to a.go on the side branch")
	shaS1 = gitOut("rev-parse", "HEAD")

	git("checkout", "feature")
	suffix := fillerLines("w", 4)
	writeCommitFile(t, ws, "a.go", filler+"a1\na2\na3\n"+suffix)
	git("add", "-A")
	git("commit", "-m", "human appends to a.go on the feature branch")
	shaC2 = gitOut("rev-parse", "HEAD")

	git("merge", "--no-ff", "-m", "merge side", "side")
	shaM = gitOut("rev-parse", "HEAD")
	return ws, git, gitOut, key, sess, shaS1, shaC2, shaM
}

// TestReworkLiveMergeFoldsEachEditOnce is the defect, through the live watcher
// loop: one poll surfaces the merge and both sides of it, and the ledger must end
// up describing head rather than head plus the side branch counted twice.
func TestReworkLiveMergeFoldsEachEditOnce(t *testing.T) {
	ws, git, _, key, sess, _, _, _ := mergeFoldRepo(t)

	pollGitWatchWorkspace(sess)

	// The exact coordinate is the whole assertion: a double fold also leaves a
	// non-empty ledger, just one describing a file 35 lines long that is 32.
	wantOneSpan(t, key, "a.go", 26, 28, "the merge folded the side branch's hunks twice")

	// FABRICATION CHECK. head is p1..p5, f1..f20, a1..a3, w1..w4. Rewrite f5..f7
	// (head lines 10..12) — human lines the AI never touched.
	prefix := fillerLines("p", 5)
	suffix := fillerLines("w", 4)
	var churned strings.Builder
	for i := 1; i <= 20; i++ {
		if i >= 5 && i <= 7 {
			fmt.Fprintf(&churned, "x%d\n", i)
			continue
		}
		fmt.Fprintf(&churned, "f%d\n", i)
	}
	writeCommitFile(t, ws, "a.go", prefix+churned.String()+"a1\na2\na3\n"+suffix)
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("rework_verdict emitted over lines the AI never wrote: %v", got)
	}

	// RECOVERY CHECK: the AI's own lines, at 26..28, report exactly once. A ledger
	// that slid them to 35..37 reports nothing here, so the absence and the
	// presence check together discriminate a wrong number from a missing one.
	writeCommitFile(t, ws, "a.go", prefix+churned.String()+"z1\nz2\nz3\n"+suffix)
	git("add", "-A")
	git("commit", "-m", "rewrite the ai lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("rework_verdicts = %v, want exactly one for a.go", got)
	}
}

// TestMergeSecondParentCommitIsStillAttributed is what makes the fix a DECOUPLING
// rather than a narrowing of the whole live range.
//
// Only the rework fold takes the first-parent chain. Whether work that arrived
// through a merge counts as AI-written is a product question, and it is not
// answered here: every commit in the detected range is still attributed exactly
// once, including the one reachable ONLY through the merge's second parent.
func TestMergeSecondParentCommitIsStillAttributed(t *testing.T) {
	_, _, _, _, sess, shaS1, shaC2, shaM := mergeFoldRepo(t)

	pollGitWatchWorkspace(sess)

	shas := attributedShas(t, state.OutboxPath())
	for _, tc := range []struct {
		name string
		sha  string
	}{
		{"the side commit, reachable only through the merge's second parent", shaS1},
		{"the first-parent commit", shaC2},
		{"the merge itself", shaM},
	} {
		if n := countSha(shas, tc.sha); n != 1 {
			t.Errorf("%s (%s) attributed %d times, want exactly 1", tc.name, tc.sha, n)
		}
	}
}
