package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// AN ORDINARY `git merge feature` ONTO THE DEFAULT BRANCH USED TO FOLD THE SAME
// EDITS TWICE INTO THE DURABILITY LEDGER.
//
// It is the exact defect TestReworkLiveMergeFoldsEachEditOnce pins for the
// sibling rework ledger, left open on this side because pollDurability took
// gitNewCommits' full range and DISCARDED the first-parent subset. Every commit
// in `cursor..tip` then reached pollDurabilityCommit, which reads each one with
// `git show -m --first-parent` — so a merge's own diff already carried everything
// the merged-away branch brought in, and folding that branch's own commits too
// applied the same hunks a second time, in a coordinate space no checkout ever
// had. The two ledgers therefore disagreed with each other on any history
// containing merges, and neither was authoritative over the other there.
//
// The fix moves the narrowing into durability's OWN enumerator
// (gitNewDefaultBranchCommits) rather than filtering at the fold, because
// durability's cursor advances per commit inside each commit's ledger
// transaction. TestDurabilityDrainsAMergeWhoseSideBranchFillsThePerPollCap below
// is the shape that rules the filter-at-the-fold version out.

// durabilityChurnPaths returns the path of every durability_verdict that reports
// a CHURN, in order.
//
// It filters on a non-empty churnedRanges rather than on the event kind, because
// inventoryLiving emits the same kind for a root's living spans and would
// otherwise read as a churn. Ordered, so a test can assert on REPEATS.
func durabilityChurnPaths(t *testing.T) []string {
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
		if ev.Kind != "durability_verdict" {
			continue
		}
		d, ok := ev.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("durability_verdict Data is %T, want map", ev.Data)
		}
		if arr, _ := d["churnedRanges"].([]interface{}); len(arr) == 0 {
			continue
		}
		path, _ := d["path"].(string)
		paths = append(paths, path)
	}
	return paths
}

// wantOneDurabilitySpan asserts the root tracks EXACTLY one AI range for path, at
// start..end. The COORDINATE is the assertion, not the count: a double fold also
// leaves exactly one span, just one addressing a line space the file never had.
func wantOneDurabilitySpan(t *testing.T, key, path string, start, end int, what string) {
	t.Helper()
	spans := loadDurabilityLedger().Roots[key][path]
	if len(spans) != 1 || spans[0].Start != start || spans[0].End != end {
		t.Fatalf("%s: spans = %+v, want exactly one at %d..%d", what, spans, start, end)
	}
}

// durabilityMergeGeometry is the file at each stage of the shape below. The AI's
// three lines sit in the MIDDLE of a.go on purpose: a double fold then parks the
// span over HUMAN lines that still exist, so the fabrication check has something
// to land on instead of running off the end of the file.
type durabilityMergeGeometry struct {
	head    string // f1..f20
	tail    string // f21..f40
	prefix  string // p1..p5, prepended on the side branch
	suffix  string // w1..w4, appended on main
	aiLines string // a1..a3
}

func newDurabilityMergeGeometry() durabilityMergeGeometry {
	return durabilityMergeGeometry{
		head:    fillerLines("f", 20),
		tail:    fillerRange("f", 21, 40),
		prefix:  fillerLines("p", 5),
		suffix:  fillerLines("w", 4),
		aiLines: "a1\na2\na3\n",
	}
}

// fillerRange renders lines lo..hi with the given prefix — fillerLines, offset,
// so a file can be split into two human regions around an AI one.
func fillerRange(prefix string, lo, hi int) string {
	var b strings.Builder
	for i := lo; i <= hi; i++ {
		fmt.Fprintf(&b, "%s%d\n", prefix, i)
	}
	return b.String()
}

// durabilityMergeRepo builds the shape, entirely on the DEFAULT branch — which is
// the only branch durability advances on:
//
//	main ── h  (human writes a.go: f1..f40)
//	         └─ c1 (ai inserts a1..a3 after f20 → span 21..23, seeded by its own poll)
//	             ├── s1 (human prepends p1..p5)  ← reachable only via the 2nd parent
//	             └── c2 (human appends w1..w4)
//	                  M = merge(c2, s1)          ← main's tip
//
// At the merge a.go is p1..p5, f1..f20, a1..a3, f21..f40, w1..w4 (52 lines), so
// the AI's three lines sit at 26..28. Folding main's own first-parent chain
// (c2 then M) lands there. Folding s1 as well applies the same five-line prepend
// twice and lands at 31..33 — f23..f25, three human lines.
func durabilityMergeRepo(t *testing.T) (ws string, git func(...string), gitOut func(...string) string, key string, sess Session, g durabilityMergeGeometry, shaS1, shaC2, shaM string) {
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
	sess = Session{DeviceID: "dev-dur-merge", TaskRoot: ws}
	g = newDurabilityMergeGeometry()

	// Baseline poll: durability cold-starts its cursor at the current default tip
	// without processing pre-existing history, so everything below is live range.
	pollGitWatchWorkspace(sess)

	writeCommitFile(t, ws, "a.go", g.head+g.tail)
	git("add", "-A")
	git("commit", "-m", "human writes a.go on main")
	pollGitWatchWorkspace(sess)

	// The AI commit gets its OWN poll so the span is seeded through the live path;
	// the merge below has to land in a range where the span is already tracked.
	recordAiTouchedPath("sess-dur-merge", key, "a.go")
	writeCommitFile(t, ws, "a.go", g.head+g.aiLines+g.tail)
	git("add", "-A")
	git("commit", "-m", "ai inserts into a.go on main")
	pollGitWatchWorkspace(sess)
	wantOneDurabilitySpan(t, key, "a.go", 21, 23, "setup: the ai commit did not seed its own span")

	git("checkout", "-b", "side")
	writeCommitFile(t, ws, "a.go", g.prefix+g.head+g.aiLines+g.tail)
	git("add", "-A")
	git("commit", "-m", "human prepends to a.go on the side branch")
	shaS1 = gitOut("rev-parse", "HEAD")

	git("checkout", "main")
	writeCommitFile(t, ws, "a.go", g.head+g.aiLines+g.tail+g.suffix)
	git("add", "-A")
	git("commit", "-m", "human appends to a.go on main")
	shaC2 = gitOut("rev-parse", "HEAD")

	git("merge", "--no-ff", "-m", "merge side", "side")
	shaM = gitOut("rev-parse", "HEAD")
	return ws, git, gitOut, key, sess, g, shaS1, shaC2, shaM
}

// TestDurabilityLiveMergeFoldsEachEditOnce is the defect, through the real
// watcher loop: one poll surfaces the merge and both sides of it, and the
// durability ledger must end up describing the merge's tree rather than that tree
// plus the merged-away branch counted a second time.
func TestDurabilityLiveMergeFoldsEachEditOnce(t *testing.T) {
	ws, git, _, key, sess, g, _, _, _ := durabilityMergeRepo(t)

	pollGitWatchWorkspace(sess)

	// The exact coordinate is the whole assertion: a double fold also leaves one
	// span, just one sitting five lines lower than any AI line ever was.
	wantOneDurabilitySpan(t, key, "a.go", 26, 28, "the merge folded the side branch's hunks twice")

	// FABRICATION CHECK. head is p1..p5, f1..f20, a1..a3, f21..f40, w1..w4, so
	// lines 31..33 are f23..f25 — human lines the AI never touched, and exactly
	// where a double fold parks the AI span. No churn is owed here, so assert its
	// ABSENCE: a wrong number and a missing number must not pass the same check.
	churnedTail := strings.Replace(g.tail, "f23\nf24\nf25\n", "x23\nx24\nx25\n", 1)
	if churnedTail == g.tail {
		t.Fatal("setup: the fabrication rewrite changed nothing")
	}
	writeCommitFile(t, ws, "a.go", g.prefix+g.head+g.aiLines+churnedTail+g.suffix)
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)
	if got := durabilityChurnPaths(t); len(got) != 0 {
		t.Fatalf("durability_verdict churned over lines the AI never wrote: %v", got)
	}

	// RECOVERY CHECK: the AI's own lines, at 26..28, report a churn exactly once.
	// A ledger that slid them to 31..33 reports nothing here, so the absence above
	// and the presence here together discriminate a wrong span from a missing one.
	writeCommitFile(t, ws, "a.go", g.prefix+g.head+"z1\nz2\nz3\n"+churnedTail+g.suffix)
	git("add", "-A")
	git("commit", "-m", "rewrite the ai lines")
	pollGitWatchWorkspace(sess)
	if got := durabilityChurnPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("durability churn verdicts = %v, want exactly one for a.go", got)
	}
}

// TestDurabilityMergeSecondParentCommitIsStillAttributed keeps the fix a
// DECOUPLING rather than a narrowing of the whole poll.
//
// Only the durability FOLD takes the default branch's first-parent chain.
// Whether work that arrived through a merge counts as AI-written is a product
// question, and it is not answered here: every commit in the detected range is
// still attributed exactly once, including the one reachable ONLY through the
// merge's second parent.
func TestDurabilityMergeSecondParentCommitIsStillAttributed(t *testing.T) {
	_, _, _, _, sess, _, shaS1, shaC2, shaM := durabilityMergeRepo(t)

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

// TestDurabilityDrainsAMergeWhoseSideBranchFillsThePerPollCap rules out the
// obvious version of this fix, and is why the narrowing lives in the ENUMERATOR.
//
// Filtering at the fold — take gitNewCommits' full range, skip the commits that
// are not on the chain — leaves the durability cursor with nowhere safe to land.
// Advance it on a skipped commit and it parks OFF the chain, where `cursor..tip`
// re-includes chain commits already folded. Do not advance it and a batch that
// happens to hold no chain commit never moves the cursor at all: clampCommitBurst
// keeps the OLDEST cap commits of `cursor..tip`, so a merged branch longer than
// the cap fills the whole batch with second-parent commits, the next poll
// enumerates the identical batch, and durability stalls for that root FOREVER.
//
// Enumerating the chain directly has no such batch: every commit returned is one
// durability folds, so the cursor always advances on a fold and the root always
// drains. The assertion is the coordinate after the root has had every chance to
// drain — which a double fold (the shipped defect) and a stall (the wrong fix)
// both miss, in opposite directions.
func TestDurabilityDrainsAMergeWhoseSideBranchFillsThePerPollCap(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))

	// Lowered so the test builds a handful of commits instead of hundreds; the
	// wedge is about the batch being FULL of second-parent commits, not about 100.
	prev := gitWatchMaxCommitsPerPoll
	gitWatchMaxCommitsPerPoll = 3
	t.Cleanup(func() { gitWatchMaxCommitsPerPoll = prev })

	ws, git, _ := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev-dur-wedge", TaskRoot: ws}
	g := newDurabilityMergeGeometry()
	pollGitWatchWorkspace(sess)

	writeCommitFile(t, ws, "a.go", g.head+g.tail)
	git("add", "-A")
	git("commit", "-m", "human writes a.go on main")
	pollGitWatchWorkspace(sess)

	recordAiTouchedPath("sess-dur-wedge", key, "a.go")
	writeCommitFile(t, ws, "a.go", g.head+g.aiLines+g.tail)
	git("add", "-A")
	git("commit", "-m", "ai inserts into a.go on main")
	pollGitWatchWorkspace(sess)
	wantOneDurabilitySpan(t, key, "a.go", 21, 23, "setup: the ai commit did not seed its own span")

	// A side branch longer than the per-poll cap, whose commits are all older than
	// the merge — so the oldest-cap slice of `cursor..tip` is entirely off the
	// default branch's chain.
	git("checkout", "-b", "side")
	for i := 1; i <= 5; i++ {
		writeCommitFile(t, ws, fmt.Sprintf("side%d.txt", i), "s\n")
		git("add", "-A")
		git("commit", "-m", fmt.Sprintf("side %d", i))
	}
	writeCommitFile(t, ws, "a.go", g.prefix+g.head+g.aiLines+g.tail)
	git("add", "-A")
	git("commit", "-m", "human prepends to a.go on the side branch")

	git("checkout", "main")
	git("merge", "--no-ff", "-m", "merge side", "side")

	// Poll well past what draining this range can need, bounded so a stalled
	// cursor fails the assertion rather than hanging the suite.
	for i := 0; i < 10; i++ {
		pollGitWatchWorkspace(sess)
	}
	wantOneDurabilitySpan(t, key, "a.go", 26, 28, "the merge did not land at head's coordinates (a double fold, or a cursor that never advanced)")
}
