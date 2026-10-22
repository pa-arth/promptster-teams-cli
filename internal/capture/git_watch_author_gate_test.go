package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// gitAs runs git in dir with author+committer set to email.
func gitAs(t *testing.T, dir, email string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=x", "GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME=x", "GIT_COMMITTER_EMAIL="+email)
	o, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, o)
	}
	return strings.TrimSpace(string(o))
}

// TestGitWatchPullOverColleagueCommitNotAttributed: `git pull` fast-forwards
// HEAD over a colleague's commit that touches a path this device's AI edited.
// The HEAD pass must not attribute it to the local session — and, since the
// backend counts any shipped SHA as a PR link, must not emit it at all — while
// this engineer's own commit in the same poll is still attributed.
func TestGitWatchPullOverColleagueCommitNotAttributed(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	if _, err := sign.GenerateSessionKeypair(); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	upstream := t.TempDir()
	gitAs(t, upstream, "other@example.com", "init")
	writeCommitFile(t, upstream, "foo.go", "package main\n")
	gitAs(t, upstream, "other@example.com", "add", "-A")
	gitAs(t, upstream, "other@example.com", "commit", "-m", "baseline")

	home := t.TempDir()
	repo := filepath.Join(home, "repos", "proj")
	gitAs(t, home, "me@example.com", "clone", "-q", upstream, repo)
	gitAs(t, repo, "me@example.com", "config", "user.email", "me@example.com")

	session := Session{DeviceID: "dev-gate", TaskRoot: home}
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/mine.go")
	pollGitWatchWorkspace(session) // cold-start baseline

	// A colleague changes the AI-edited file upstream; this engineer pulls.
	writeCommitFile(t, upstream, "foo.go", "package main\n\nfunc colleague() {}\n")
	gitAs(t, upstream, "other@example.com", "commit", "-am", "colleague edits foo")
	theirs := gitAs(t, upstream, "other@example.com", "rev-parse", "HEAD")
	gitAs(t, repo, "me@example.com", "pull", "-q", "--ff-only")

	// Then commits their own AI-edited file before the next poll.
	writeCommitFile(t, repo, "mine.go", "package main\n\nfunc mine() {}\n")
	gitAs(t, repo, "me@example.com", "add", "-A")
	gitAs(t, repo, "me@example.com", "commit", "-m", "mine")
	ours := gitAs(t, repo, "me@example.com", "rev-parse", "HEAD")

	pollGitWatchWorkspace(session)
	got := attributedShas(t, state.OutboxPath())
	if countSha(got, theirs) != 0 {
		t.Errorf("colleague commit %s was emitted as commit_attribution: %v", theirs, got)
	}
	if countSha(got, ours) != 1 {
		t.Errorf("own commit %s must be attributed exactly once, got %v", ours, got)
	}
}

// TestGitOwnCommitsNoIdentityKeepsEverything: with no user.email configured the
// gate is off (nil), so a misconfigured machine keeps today's capture.
func TestGitOwnCommitsNoIdentityKeepsEverything(t *testing.T) {
	ws, git, gitOut := gitRepo(t)
	git("commit", "--allow-empty", "-m", "A")
	if own := gitOwnCommits(ws, []string{gitOut("rev-parse", "HEAD")}); own != nil {
		t.Fatalf("no identity must disable the gate, got %v", own)
	}
}

// A foreign commit is recorded in the attributed-commits ledger (so a cursor
// recovery can't fold it twice), which routes it into the adoption replay on the
// next branch switch. That replay rebuilds attributable files from this device's
// ai-paths ledger, so without the author verdict it SEEDED rework spans for a
// colleague's lines — a later rewrite of them then emitted a rework_verdict
// against code no local agent wrote.
func TestReworkAdoptionReplayDoesNotSeedColleaguesCommit(t *testing.T) {
	ws, git, gitOut, key, sess := adoptionRepo(t)
	git("config", "user.email", "me@example.com") // commits below are authored t@t: foreign

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-foreign", key, "a.go")
	writeCommitFile(t, ws, "a.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "colleague edits a.go")
	recordAttributedCommits([]string{gitOut("rev-parse", "HEAD")}, time.Now().UnixMilli())

	pollGitWatchWorkspace(sess) // adopts feature and replays the attributed commit
	if c := reworkCovered(key, "a.go"); len(c) != 0 {
		t.Fatalf("adoption replay seeded a colleague's commit as AI work: covered=%+v", c)
	}
}

// Same hole through the cold-start replay (a brand-new worktree of a branch this
// device already recorded).
func TestReworkColdStartReplayDoesNotSeedColleaguesCommit(t *testing.T) {
	ws, git, gitOut, key, sess := coldStartRepo(t)
	git("config", "user.email", "me@example.com") // fixture commits are authored t@t: foreign
	_, _, _, sha2 := coldStartBranch(t, ws, git, gitOut, key)

	nowMs := time.Now().UnixMilli()
	adoptReworkBranch(key, "feature")
	replayReworkForColdStartBranch(sess, ws, sha2, loadAttributedCommits(nowMs), nowMs)
	if got := loadReworkLedger().Roots[key]["a.go"]; len(got) != 0 {
		t.Fatalf("cold-start replay seeded a colleague's commits as AI work: %+v", got)
	}
}
