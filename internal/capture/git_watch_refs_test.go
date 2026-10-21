package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// TestOffHeadPushedBranchCommitSurfaced reproduces the prod leak: an agent
// branches, commits, pushes, switches back and deletes the branch between two
// polls, so the commit never sits at a polled HEAD. The pushed remote-tracking
// ref is all that is left, and the commit must still be emitted — while a commit
// by another author on the same branch, touching the same AI-edited file, must
// not be.
func TestOffHeadPushedBranchCommitSurfaced(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	if _, err := sign.GenerateSessionKeypair(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	repo := filepath.Join(home, "repos", "proj")
	git, gitOut := gitRepoAt(t, repo)
	git("config", "user.email", "t@t") // gitRepoAt commits as t@t via env
	writeCommitFile(t, repo, "base.go", "package main\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	session := Session{DeviceID: "dev-offhead", TaskRoot: home}
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/ai.go")
	pollGitWatchWorkspace(session) // baseline: emits nothing
	if got := attributedShas(t, state.OutboxPath()); len(got) != 0 {
		t.Fatalf("baseline poll emitted %v", got)
	}

	git("switch", "-c", "feat")
	writeCommitFile(t, repo, "ai.go", "package main\n\nfunc ai() {}\n")
	git("add", "-A")
	git("commit", "-m", "agent work")
	mine := gitOut("rev-parse", "HEAD")
	writeCommitFile(t, repo, "ai.go", "package main\n\nfunc ai() { other() }\n")
	git("add", "-A")
	git("commit", "--author", "Other <other@example.com>", "-m", "someone else")
	theirs := gitOut("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/feat", theirs) // the push
	git("switch", "main")
	git("branch", "-D", "feat")

	pollGitWatchWorkspace(session)
	got := attributedShas(t, state.OutboxPath())
	if countSha(got, mine) != 1 {
		t.Fatalf("pushed off-HEAD commit %s not surfaced exactly once; outbox: %v", mine, got)
	}
	if countSha(got, theirs) != 0 {
		t.Fatalf("other author's commit %s was emitted; outbox: %v", theirs, got)
	}
	if sid := sessionForSha(t, mine); sid != "ai-sess-1" {
		t.Errorf("off-HEAD commit attributed to %q, want ai-sess-1", sid)
	}

	pollGitWatchWorkspace(session) // steady state: nothing new, no repeats
	assertNoRepeats(t, attributedShas(t, state.OutboxPath()))
}

func sessionForSha(t *testing.T, sha string) string {
	t.Helper()
	data, err := os.ReadFile(state.OutboxPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev event.Event
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Kind != "commit_attribution" {
			continue
		}
		if d, _ := ev.Data.(map[string]interface{}); d["commitSha"] == sha {
			return ev.SessionID
		}
	}
	return ""
}
