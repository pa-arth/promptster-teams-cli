package capture

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gitRepo spins up an isolated git repo under a temp dir and returns a closure
// that runs git in it (failing the test on error) plus one that returns trimmed
// stdout. Mirrors the harness in census_test.go.
func gitRepo(t *testing.T) (dir string, run func(args ...string), out func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir = t.TempDir()
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
	run("init")
	return dir, run, out
}

// TestGitWatchDetectsNewCommits: cold start baselines silently, a later commit
// is reported exactly once (not the baseline commit), and a steady-state poll
// reports nothing.
func TestGitWatchDetectsNewCommits(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	git("commit", "--allow-empty", "-m", "A")

	roots := []string{ws}
	key := gitWatchRootKey(ws)

	// Cold start: record HEAD as baseline, report no new commits.
	if d := pollGitWatch(roots); len(d) != 0 {
		t.Fatalf("cold start must report no commits, got %v", d)
	}

	// New commit B → reported exactly, and only B (not the baseline A).
	git("commit", "--allow-empty", "-m", "B")
	headB := gitOut("rev-parse", "HEAD")
	d := pollGitWatch(roots)
	if len(d[key]) != 1 || d[key][0] != headB {
		t.Fatalf("expected only B (%s), got %v", headB, d[key])
	}

	// Idle poll → cursor already at HEAD, nothing new.
	if d := pollGitWatch(roots); len(d) != 0 {
		t.Fatalf("idle poll must report nothing, got %v", d)
	}
}

// assertLikelyAI builds attribution for one detected commit and asserts `path`
// is attributed likely_ai. When wantStart >= 0 it also pins the first committed
// @@ span. Reuses the PR3 filesByPath helper (same package).
func assertLikelyAI(t *testing.T, root, sha, path string, wantStart, wantEnd float64) {
	t.Helper()
	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev", TaskRoot: root}, root, sha)
	if !ok {
		t.Fatalf("expected an emittable event for %s", sha)
	}
	files := filesByPath(t, ev)
	f, present := files[path]
	if !present {
		t.Fatalf("%s missing from attribution: %+v", path, files)
	}
	ranges := f["lineRanges"].([]interface{})
	if len(ranges) == 0 {
		t.Fatalf("%s has no lineRanges", path)
	}
	r := ranges[0].(map[string]interface{})
	if r["attribution"] != attributionLikelyAI {
		t.Errorf("%s attribution = %v, want likely_ai", path, r["attribution"])
	}
	if wantStart >= 0 && (r["start"] != wantStart || r["end"] != wantEnd) {
		t.Errorf("%s committed span = %v..%v, want %v..%v", path, r["start"], r["end"], wantStart, wantEnd)
	}
}

// TestGitWatchForwardCommitAttributed (regression): a plain forward commit that
// touches an AI-touched file is detected exactly once and attributed likely_ai.
func TestGitWatchForwardCommitAttributed(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")

	roots := []string{ws}
	key := gitWatchRootKey(ws)
	pollGitWatch(roots) // cold-start baseline at base

	recordAiTouchedPath("sess-fwd", gitWatchRootKey(ws), "ai.go")
	writeCommitFile(t, ws, "ai.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds ai.go")
	sha := gitOut("rev-parse", "HEAD")

	d := pollGitWatch(roots)
	if len(d[key]) != 1 || d[key][0] != sha {
		t.Fatalf("want only %s detected, got %v", sha, d[key])
	}
	assertLikelyAI(t, ws, sha, "ai.go", 1, 3)
}

// TestGitWatchAmendReattributed: `git commit --amend` rewrites the tip to a new
// SHA whose object survives (parent unchanged), so the plain `lastSeen..head`
// range still surfaces it and it re-attributes as likely_ai. Asserts the amended
// SHA — not the pre-amend one — is what flows through.
func TestGitWatchAmendReattributed(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")

	recordAiTouchedPath("sess-amend", gitWatchRootKey(ws), "ai.go")
	writeCommitFile(t, ws, "ai.go", "l1\nl2\n")
	git("add", "-A")
	git("commit", "-m", "ai commit A")
	preAmend := gitOut("rev-parse", "HEAD")

	roots := []string{ws}
	key := gitWatchRootKey(ws)
	pollGitWatch(roots) // baseline at A

	writeCommitFile(t, ws, "ai.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "--amend", "--no-edit")
	amended := gitOut("rev-parse", "HEAD")
	if amended == preAmend {
		t.Fatal("amend did not change the SHA")
	}

	d := pollGitWatch(roots)
	if len(d[key]) != 1 || d[key][0] != amended {
		t.Fatalf("want only amended %s detected, got %v", amended, d[key])
	}
	assertLikelyAI(t, ws, amended, "ai.go", 1, 3)
}

// TestGitWatchSquashMergeAttributed (headline gate): two feature commits touching
// an AI-touched file are squash-merged into main as ONE commit S; polling the
// main root detects S and attributes ai.go likely_ai against the committed span.
func TestGitWatchSquashMergeAttributed(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	mainBranch := gitOut("rev-parse", "--abbrev-ref", "HEAD")

	git("checkout", "-b", "feature")
	writeCommitFile(t, ws, "ai.go", "l1\n")
	git("add", "-A")
	git("commit", "-m", "feature c1")
	writeCommitFile(t, ws, "ai.go", "l1\nl2\n")
	git("add", "-A")
	git("commit", "-m", "feature c2")

	git("checkout", mainBranch)
	roots := []string{ws}
	key := gitWatchRootKey(ws)
	pollGitWatch(roots) // baseline main at base

	recordAiTouchedPath("sess-squash", gitWatchRootKey(ws), "ai.go")
	git("merge", "--squash", "feature")
	git("commit", "-m", "squash feature")
	sha := gitOut("rev-parse", "HEAD")

	d := pollGitWatch(roots)
	if len(d[key]) != 1 || d[key][0] != sha {
		t.Fatalf("want only squash %s detected, got %v", sha, d[key])
	}
	// The squashed commit adds ai.go whole against main (which lacked it): 1..2.
	assertLikelyAI(t, ws, sha, "ai.go", 1, 2)
}

// TestGitWatchCherryPickAttributed: cherry-picking an AI-file commit onto main
// creates a new SHA; polling main detects it and attributes likely_ai.
func TestGitWatchCherryPickAttributed(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	mainBranch := gitOut("rev-parse", "--abbrev-ref", "HEAD")

	git("checkout", "-b", "feature")
	writeCommitFile(t, ws, "ai.go", "l1\nl2\n")
	git("add", "-A")
	git("commit", "-m", "ai on feature")
	srcSha := gitOut("rev-parse", "HEAD")

	git("checkout", mainBranch)
	// Diverge main so the cherry-pick lands on a different parent (else it would
	// recreate srcSha byte-for-byte and produce an identical SHA).
	writeCommitFile(t, ws, "main.txt", "diverge\n")
	git("add", "-A")
	git("commit", "-m", "main diverges")
	roots := []string{ws}
	key := gitWatchRootKey(ws)
	pollGitWatch(roots) // baseline main at the divergence commit

	recordAiTouchedPath("sess-cp", gitWatchRootKey(ws), "ai.go")
	git("cherry-pick", srcSha)
	picked := gitOut("rev-parse", "HEAD")
	if picked == srcSha {
		t.Fatal("cherry-pick produced the same SHA")
	}

	d := pollGitWatch(roots)
	if len(d[key]) != 1 || d[key][0] != picked {
		t.Fatalf("want only cherry-picked %s detected, got %v", picked, d[key])
	}
	assertLikelyAI(t, ws, picked, "ai.go", 1, 2)
}

// TestGitWatchResetBackwardReportsNothing: cursor ahead of HEAD (a `git reset
// --hard` to an older commit) yields an empty `lastSeen..head`, so the poll
// reports nothing — no false positives, no panic.
func TestGitWatchResetBackwardReportsNothing(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	git("commit", "--allow-empty", "-m", "base")
	git("commit", "--allow-empty", "-m", "A")
	shaA := gitOut("rev-parse", "HEAD")
	git("commit", "--allow-empty", "-m", "B")

	roots := []string{ws}
	pollGitWatch(roots) // baseline at B

	git("reset", "--hard", shaA)
	if d := pollGitWatch(roots); len(d) != 0 {
		t.Fatalf("reset backward must report nothing, got %v", d)
	}
}

// TestGitWatchBurstDrains: a HEAD far from the cursor (five commits, cap 3) is
// never processed in one poll — each poll takes the OLDEST cap and advances the
// cursor only to the newest returned SHA, so the remainder drains on subsequent
// polls IN ORDER. Every commit surfaces exactly once, oldest→newest, and the
// per-poll batch never exceeds the cap.
func TestGitWatchBurstDrains(t *testing.T) {
	orig := gitWatchMaxCommitsPerPoll
	gitWatchMaxCommitsPerPoll = 3
	defer func() { gitWatchMaxCommitsPerPoll = orig }()

	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	git("commit", "--allow-empty", "-m", "base")
	base := gitOut("rev-parse", "HEAD")

	var want []string // the five burst commits, oldest→newest
	for i := 0; i < 5; i++ {
		git("commit", "--allow-empty", "-m", "burst")
		want = append(want, gitOut("rev-parse", "HEAD"))
	}

	roots := []string{ws}
	key := gitWatchRootKey(ws)
	saveGitWatchCursors(map[string]string{key: base}) // cursor far behind head

	var got []string
	for poll := 0; poll < 4 && len(got) < len(want); poll++ {
		batch := pollGitWatch(roots)[key]
		if len(batch) > gitWatchMaxCommitsPerPoll {
			t.Fatalf("poll %d exceeded cap %d: got %d", poll, gitWatchMaxCommitsPerPoll, len(batch))
		}
		for i := len(batch) - 1; i >= 0; i-- { // batch is newest-first; collect oldest→newest
			got = append(got, batch[i])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("want all %d commits drained across polls, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commit %d out of order: got %s, want %s", i, got[i], want[i])
		}
	}
	if d := pollGitWatch(roots); len(d) != 0 {
		t.Fatalf("after draining, poll must report nothing, got %v", d)
	}
}

// TestGitWatchGcdCursorRecovered: a cursor pointing at an object that is no longer
// valid (gc'd after an aggressive rewrite) makes `rev-list lastSeen..head` ERROR.
// The bounded recovery window (`rev-list -n cap head`) must still surface the tip
// so it is attributed instead of silently skipped and lost.
func TestGitWatchGcdCursorRecovered(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "ai.go", "l1\n")
	git("add", "-A")
	git("commit", "-m", "A")
	writeCommitFile(t, ws, "ai.go", "l1\nl2\n")
	git("add", "-A")
	git("commit", "-m", "B")
	head := gitOut("rev-parse", "HEAD")

	roots := []string{ws}
	key := gitWatchRootKey(ws)
	// Fabricate a gc'd cursor: a well-formed SHA that is not a valid object.
	saveGitWatchCursors(map[string]string{key: strings.Repeat("0", 40)})

	recordAiTouchedPath("sess-gc", gitWatchRootKey(ws), "ai.go")
	d := pollGitWatch(roots)
	if len(d[key]) == 0 {
		t.Fatal("gc'd cursor must recover the tip, got nothing (silent skip)")
	}
	if d[key][0] != head {
		t.Errorf("recovery window must be newest-first: got %s, want head %s", d[key][0], head)
	}
	assertLikelyAI(t, ws, d[key][0], "ai.go", -1, -1)
}

// TestGitWatchHandlesDegenerateRoots asserts no panic and no false positives on
// a repo with zero commits, a non-git directory, and a detached HEAD.
func TestGitWatchHandlesDegenerateRoots(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	// Repo with no commits: rev-parse HEAD fails → skipped cleanly.
	empty, _, _ := gitRepo(t)
	if d := pollGitWatch([]string{empty}); len(d) != 0 {
		t.Errorf("repo with no commits must report nothing, got %v", d)
	}

	// Not a git repo at all → skipped cleanly.
	if d := pollGitWatch([]string{t.TempDir()}); len(d) != 0 {
		t.Errorf("non-repo must report nothing, got %v", d)
	}

	// Detached HEAD: rev-parse still resolves, so it baselines then idles.
	ws, git, _ := gitRepo(t)
	git("commit", "--allow-empty", "-m", "A")
	git("commit", "--allow-empty", "-m", "B")
	git("checkout", "--detach", "HEAD~1")
	pollGitWatch([]string{ws}) // cold-start baseline at detached HEAD
	if d := pollGitWatch([]string{ws}); len(d) != 0 {
		t.Errorf("detached HEAD steady state must report nothing, got %v", d)
	}
}
