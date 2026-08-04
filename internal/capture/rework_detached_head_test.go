package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// A `git rebase -i` ON A TRACKED FEATURE BRANCH USED TO LEAVE THAT BRANCH'S AI
// SPANS ADDRESSING LINES NOBODY WROTE.
//
// reworkScope returns scopeUnknown while HEAD is detached, which is exactly what
// a rebase in progress looks like. The scope switch deliberately has no clear
// there — a transient detach must not wipe a real branch's tracking — but
// `preMerge` is false, so every commit in the range was ATTRIBUTED and RECORDED
// while none of them was FOLDED. A recorded commit is never revisited, so those
// hunks never shifted the spans the root kept, and the next rewrite of the human
// lines that had moved into those coordinates emitted a rework_verdict over code
// the AI never wrote.
//
// It is routine usage, not a corner case: the git watcher polls every
// gitWatchInterval (60s) and an interactive rebase blocks on the engineer's
// editor for far longer than that. And it is not transient — the span stays
// wrong for good.
//
// THE FIX IS THE INVARIANT, NOT A SPECIAL CASE. A root's tracked spans are
// addressed in the line space the commits it has folded compose to. A scope that
// folds nothing while commits land is the one place that line space can move out
// from under them, so the spans are released there rather than carried. The cost
// is an undercount — a rebase drops the branch's rework tracking — which is the
// direction these ledgers always resolve toward, and a rebase rewrites the very
// history those coordinates were measured against anyway.

// gitAllowFail runs git in dir and returns its combined output plus whether it
// succeeded. gitRepo's own closure fails the test on a non-zero exit, and the
// whole point below is a `git rebase --exec` that DOES exit non-zero — that is
// how the rebase is parked mid-flight with HEAD detached.
func gitAllowFail(t *testing.T, dir string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// detachedHeadRepo builds a tracked `feature` branch whose AI span is seeded by a
// live poll, with the default branch advanced underneath it so a rebase has real
// work to replay:
//
//	main ── h   (human writes a.go: f1..f40)          ← polled, so the root has a cursor
//	         ├── feature/c1 (ai inserts a1..a3 after f20 → span 21..23, polled)
//	         └── m1  (human prepends p1..p5 on main)
//
// After the rebase a.go is p1..p5, f1..f20, a1..a3, f21..f40 — the AI's lines at
// 26..28, and the ledger's untouched 21..23 pointing at f16..f18.
func detachedHeadRepo(t *testing.T) (ws string, git func(...string), gitOut func(...string) string, key string, sess Session, g durabilityMergeGeometry) {
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
	sess = Session{DeviceID: "dev-detached", TaskRoot: ws}
	g = newDurabilityMergeGeometry()

	writeCommitFile(t, ws, "a.go", g.head+g.tail)
	git("add", "-A")
	git("commit", "-m", "human writes a.go on main")
	// Polled BEFORE the branch exists, so the root has a real cursor and none of
	// what follows is the cold-start path.
	pollGitWatchWorkspace(sess)

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-detached", key, "a.go")
	writeCommitFile(t, ws, "a.go", g.head+g.aiLines+g.tail)
	git("add", "-A")
	git("commit", "-m", "ai inserts into a.go on the feature branch")
	pollGitWatchWorkspace(sess)
	wantOneSpan(t, key, "a.go", 21, 23, "setup: the ai commit did not seed its own span")

	return ws, git, gitOut, key, sess, g
}

// TestReworkDetachedRebaseDoesNotStrandTheBranchesSpans is the defect, through
// the real watcher loop, with a REAL rebase: the poll lands while git has HEAD
// detached mid-rebase, which is the only shape that reaches scopeUnknown through
// ordinary usage. Nothing here stubs a scope or flips a flag.
func TestReworkDetachedRebaseDoesNotStrandTheBranchesSpans(t *testing.T) {
	ws, git, gitOut, key, sess, g := detachedHeadRepo(t)

	// Advance the default branch so the rebase has something to replay onto.
	git("checkout", "main")
	writeCommitFile(t, ws, "a.go", g.prefix+g.head+g.tail)
	git("add", "-A")
	git("commit", "-m", "human prepends to a.go on main")
	shaMain := gitOut("rev-parse", "HEAD")
	git("checkout", "feature")

	// Park the rebase mid-flight. The exec is a git command that always exits
	// non-zero, so git stops right after replaying the branch's commit and leaves
	// HEAD detached — the state an interactive rebase sits in while the engineer's
	// editor is open.
	if out, ok := gitAllowFail(t, ws, "rebase", "main", "--exec", "git rev-parse --verify refs/heads/definitely-not-a-branch"); ok {
		t.Fatalf("setup: the rebase was expected to stop on the failing exec:\n%s", out)
	}
	if ref, _ := gitAllowFail(t, ws, "symbolic-ref", "--quiet", "HEAD"); strings.TrimSpace(ref) != "" {
		t.Fatalf("setup: HEAD is not detached (%q) — this test is not exercising scopeUnknown", strings.TrimSpace(ref))
	}
	shaReplayed := gitOut("rev-parse", "HEAD")

	// THE POLL THAT USED TO STRAND THE SPANS. Both commits are attributed and
	// recorded; neither can be folded, because nothing here can tell a feature
	// branch from a bisect.
	pollGitWatchWorkspace(sess)

	// Recorded, not silently dropped: the fix must not buy its correctness by
	// making the loop stop attributing. This is also what makes the emptiness
	// below meaningful rather than vacuous.
	shas := attributedShas(t, state.OutboxPath())
	for _, tc := range []struct{ name, sha string }{
		{"the default branch's own commit", shaMain},
		{"the commit the rebase replayed while HEAD was detached", shaReplayed},
	} {
		if n := countSha(shas, tc.sha); n != 1 {
			t.Errorf("%s (%s) attributed %d times, want exactly 1", tc.name, tc.sha, n)
		}
	}

	// NO SPAN IS OWED HERE, so assert its ABSENCE rather than a coordinate: after
	// a rebase the branch's tracking is genuinely gone, and a stale 21..23 and a
	// correct emptiness must not pass the same check.
	if spans := loadReworkLedger().Roots[key]["a.go"]; len(spans) != 0 {
		t.Fatalf("a.go still tracks %+v after a rebase this root could not fold", spans)
	}
	// The tombstone is the other half, and without it the release itself would
	// open an inflation path: an untracked path with live ai-paths presence is a
	// FIRST TOUCH again, so the next purely-human commit would enter the ledger as
	// fresh AI. The rewrite below proves it stays shut.
	if _, ok := loadReworkLedger().Seeded[key]["a.go"]; !ok {
		t.Fatal("a.go left the ledger with no seed tombstone — path-level inference can re-seed it")
	}

	if out, ok := gitAllowFail(t, ws, "rebase", "--continue"); !ok {
		t.Fatalf("setup: could not finish the rebase:\n%s", out)
	}
	pollGitWatchWorkspace(sess)

	// FABRICATION CHECK. head is p1..p5, f1..f20, a1..a3, f21..f40, so lines
	// 21..23 are f16..f18 — human lines, and exactly where the stranded span sat.
	churnedHead := strings.Replace(g.head, "f16\nf17\nf18\n", "x16\nx17\nx18\n", 1)
	if churnedHead == g.head {
		t.Fatal("setup: the fabrication rewrite changed nothing")
	}
	writeCommitFile(t, ws, "a.go", g.prefix+churnedHead+g.aiLines+g.tail)
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("rework_verdict emitted over lines the AI never wrote: %v", got)
	}

	// INFLATION CHECK. The commit above rewrote a path the ai-paths ledger still
	// holds, so without the tombstone it would have re-seeded those human lines as
	// AI — and this second rewrite of the same region would report them.
	twiceChurned := strings.Replace(churnedHead, "x16\nx17\nx18\n", "y16\ny17\ny18\n", 1)
	writeCommitFile(t, ws, "a.go", g.prefix+twiceChurned+g.aiLines+g.tail)
	git("add", "-A")
	git("commit", "-m", "human rewrites its own lines again")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 0 {
		t.Fatalf("the release re-armed first-touch seeding — human lines came back as AI: %v", got)
	}
}

// TestReworkDetachedHeadWithoutNewCommitsKeepsTheBranchesSpans is the other half
// of the gate, and it is what keeps the release from being "drop on every
// unknown scope".
//
// A detached HEAD that produces NO commit has moved nothing: `git bisect`, a CI
// checkout, `git checkout --detach` to read something. The tracked spans still
// describe the line space they were measured in, so they are kept — and they are
// kept LIVE, which the rewrite at the end is what proves. Asserting only that the
// ledger is non-empty would pass on a ledger nothing can use.
func TestReworkDetachedHeadWithoutNewCommitsKeepsTheBranchesSpans(t *testing.T) {
	ws, git, _, key, sess, g := detachedHeadRepo(t)

	git("checkout", "--detach", "HEAD")
	pollGitWatchWorkspace(sess)
	wantOneSpan(t, key, "a.go", 21, 23, "a detached HEAD that committed nothing dropped the branch's spans")

	git("checkout", "feature")
	pollGitWatchWorkspace(sess)

	// head is f1..f20, a1..a3, f21..f40, so 21..23 are the AI's own lines.
	writeCommitFile(t, ws, "a.go", g.head+"z1\nz2\nz3\n"+g.tail)
	git("add", "-A")
	git("commit", "-m", "rewrite the ai lines")
	pollGitWatchWorkspace(sess)
	if got := reworkVerdictPaths(t); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("rework_verdicts = %v, want exactly one for a.go", got)
	}
}
