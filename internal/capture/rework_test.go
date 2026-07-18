package capture

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// reworkVerdictFor pulls the rework verdict event for a given path out of a
// slice, or fails. Verdicts are emitted one per (commit, path).
func reworkVerdictFor(t *testing.T, evs []event.Event, path string) map[string]interface{} {
	t.Helper()
	for _, ev := range evs {
		if ev.Kind != "rework_verdict" {
			t.Fatalf("kind = %q, want rework_verdict", ev.Kind)
		}
		data, ok := ev.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("event Data is %T, want map", ev.Data)
		}
		if data["path"] == path {
			return data
		}
	}
	t.Fatalf("no rework_verdict for %q in %d event(s)", path, len(evs))
	return nil
}

// reworkCovered flattens a root's tracked rework ranges for a path into a set of
// covered line numbers, for order-independent assertions.
func reworkCovered(rootKey, path string) map[int]bool {
	covered := map[int]bool{}
	for _, r := range loadReworkLedger().Roots[rootKey][path] {
		for ln := r.Start; ln <= r.End; ln++ {
			covered[ln] = true
		}
	}
	return covered
}

// commitDiffFiles reruns the attribution engine's single `git show` for a commit
// and returns the raw diff + reconciled files, exactly as the working-HEAD loop
// feeds them into rework.
func commitDiffFiles(t *testing.T, root, sha string) (string, []attrFile) {
	t.Helper()
	diff, files, _, ok := commitAttributionFromDiff(root, sha)
	if !ok {
		t.Fatalf("commitAttributionFromDiff(%s) not ok", sha)
	}
	return diff, files
}

// TestReworkChurnEmitsVerdict: an AI line seeded from an earlier pre-merge branch
// commit and then rewritten by a LATER pre-merge commit emits a rework_verdict
// carrying exactly the reworked (churned) AI range. Reuses §2's remap/churn math.
func TestReworkChurnEmitsVerdict(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	// Feature branch: the AI writes feature.go (3 AI lines).
	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-rw", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds feature.go")
	sha1 := gitOut("rev-parse", "HEAD")

	diff1, files1 := commitDiffFiles(t, ws, sha1)
	if seed := pollReworkCommit(sess, ws, sha1, diff1, files1, t0); len(seed) != 0 {
		t.Fatalf("seeding a first-touch AI commit must emit no rework, got %+v", seed)
	}
	if c := reworkCovered(key, "feature.go"); !c[1] || !c[2] || !c[3] {
		t.Fatalf("rework ledger = %+v, want 1..3 seeded", c)
	}

	// A LATER pre-merge commit rewrites AI line 2.
	recordAiTouchedPath("sess-rw", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "l1\nCHANGED\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai reworks line 2")
	sha2 := gitOut("rev-parse", "HEAD")

	diff2, files2 := commitDiffFiles(t, ws, sha2)
	verdicts := pollReworkCommit(sess, ws, sha2, diff2, files2, t0+dayMs)
	data := reworkVerdictFor(t, verdicts, "feature.go")
	reworked := rangeSet(t, data, "reworkedRanges")
	if !reworked["2..2"] {
		t.Errorf("reworkedRanges = %+v, want line 2 reworked", reworked)
	}
	if data["commitSha"] != sha2 {
		t.Errorf("commitSha = %v, want the reworking commit %s", data["commitSha"], sha2)
	}
	// Lines 1 and 3 survive as tracked; line 2 is gone (reworked, emitted once).
	if c := reworkCovered(key, "feature.go"); !c[1] || c[2] || !c[3] {
		t.Errorf("rework ledger = %+v, want {1,3} (line 2 reworked out)", c)
	}
}

// TestReworkFirstTouchOnly: a reworked AI line is emitted ONCE and dropped — a
// later commit that rewrites the SAME line again finds nothing tracked there, so
// it does not re-count (first-touch seeding, matching durability's honesty rule;
// a conservative undercount, never inflation).
func TestReworkFirstTouchOnly(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-rw", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds feature.go")
	sha1 := gitOut("rev-parse", "HEAD")
	diff1, files1 := commitDiffFiles(t, ws, sha1)
	pollReworkCommit(sess, ws, sha1, diff1, files1, t0)

	// Rework line 2 → emitted once, dropped.
	recordAiTouchedPath("sess-rw", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "l1\nCHANGED\nl3\n")
	git("add", "-A")
	git("commit", "-m", "reworks line 2")
	sha2 := gitOut("rev-parse", "HEAD")
	diff2, files2 := commitDiffFiles(t, ws, sha2)
	if v := pollReworkCommit(sess, ws, sha2, diff2, files2, t0+dayMs); len(v) == 0 {
		t.Fatal("first rework of line 2 must emit a verdict")
	}

	// Rework the SAME line 2 again → nothing tracked there anymore, no re-count.
	recordAiTouchedPath("sess-rw", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "l1\nAGAIN\nl3\n")
	git("add", "-A")
	git("commit", "-m", "reworks line 2 again")
	sha3 := gitOut("rev-parse", "HEAD")
	diff3, files3 := commitDiffFiles(t, ws, sha3)
	if v := pollReworkCommit(sess, ws, sha3, diff3, files3, t0+2*dayMs); len(v) != 0 {
		t.Errorf("a reworked line must not re-count on a later rewrite, got %+v", v)
	}
}

// TestReworkScopedToPreMergeBranch (the gate): rework is seeded ONLY while the
// working branch is ahead of the default branch. A commit that lands directly on
// the DEFAULT branch must NOT enter the rework ledger — its churn is durability's
// concern, not pre-merge rework. Driven through the real poll cycle.
func TestReworkScopedToPreMergeBranch(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, _ := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}

	pollGitWatchWorkspace(sess) // baseline cursors on main

	// AI commits DIRECTLY on main (head == default tip → not pre-merge).
	recordAiTouchedPath("sess-rw", key, "ai.go")
	writeCommitFile(t, ws, "ai.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds ai.go on main")
	pollGitWatchWorkspace(sess)

	if led := loadReworkLedger().Roots[key]; len(led) != 0 {
		t.Fatalf("a commit on the default branch must not seed rework, got %+v", led)
	}
}

// TestReworkClearedAfterMerge: once the feature branch is merged and the default
// branch is checked back out, the root's rework tracking is dropped — its
// surviving AI lines are now the durability engine's, and reworked ones already
// emitted. Without the clear, a future branch could remap against stale ranges.
func TestReworkClearedAfterMerge(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, _ := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}

	pollGitWatchWorkspace(sess) // baseline

	// Feature branch: AI writes feature.go → rework seeds it (pre-merge).
	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-rw", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds feature.go")
	pollGitWatchWorkspace(sess)
	if c := reworkCovered(key, "feature.go"); !c[1] || !c[2] || !c[3] {
		t.Fatalf("rework ledger = %+v, want 1..3 seeded on the feature branch", c)
	}

	// Squash-merge into main and check main back out (head == default tip again).
	git("checkout", "main")
	git("merge", "--squash", "feature")
	git("commit", "-m", "squash: feature")
	pollGitWatchWorkspace(sess)

	if led := loadReworkLedger().Roots[key]; len(led) != 0 {
		t.Errorf("rework tracking must be cleared once back on the default branch, got %+v", led)
	}
}
