package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
