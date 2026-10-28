package capture

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

func TestReflogCreatedHere(t *testing.T) {
	for subject, want := range map[string]bool{
		"commit: five":                                  true,
		"commit (initial): one":                         true,
		"commit (amend): fix":                           true,
		"commit (merge): Merge branch 'x'":              true,
		"cherry-pick: five":                             true,
		"revert: Revert \"x\"":                          true,
		"rebase (pick): six":                            true,
		"rebase -i (squash): six":                       true,
		"pull --rebase (pick): seven":                   true,
		"merge feat: Merge made by the 'ort' strategy.": true,
		"pull: Merge made by the 'ort' strategy.":       true,
		"pull --ff-only: Fast-forward":                  false,
		"merge feat: Fast-forward":                      false,
		"pull --rebase (start): checkout 6a1f057":       false,
		"fetch: fast-forward":                           false,
		"clone: from /x":                                false,
		"checkout: moving from main to feat":            false,
		"reset: moving to HEAD~1":                       false,
		"update by push":                                false,
		"":                                              false,
	} {
		if got := reflogCreatedHere(subject); got != want {
			t.Errorf("reflogCreatedHere(%q) = %v, want %v", subject, got, want)
		}
	}
}

// twoDevices sets up an upstream and two clones of it standing in for one
// engineer's laptop and desktop: same user.email on both.
func twoDevices(t *testing.T) (upstream, laptop, desktop, home string) {
	t.Helper()
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
	const me = "me@example.com"
	upstream = t.TempDir()
	gitAs(t, upstream, me, "init", "-q", "--bare", "-b", "main")
	laptop = t.TempDir()
	gitAs(t, laptop, me, "clone", "-q", upstream, ".")
	gitAs(t, laptop, me, "config", "user.email", me)
	writeCommitFile(t, laptop, "foo.go", "package main\n")
	gitAs(t, laptop, me, "add", "-A")
	gitAs(t, laptop, me, "commit", "-q", "-m", "baseline")
	gitAs(t, laptop, me, "push", "-q", "origin", "main")

	home = t.TempDir()
	desktop = filepath.Join(home, "repos", "proj")
	gitAs(t, home, me, "clone", "-q", upstream, desktop)
	gitAs(t, desktop, me, "config", "user.email", me)
	return upstream, laptop, desktop, home
}

// One engineer, two devices. The laptop commits (same author email) and pushes;
// the desktop pulls it fast-forward and also fetches a pushed branch. The
// desktop's agent edited the same file, so path-level attribution would match —
// but only the device that wrote a commit may claim it. The desktop's own commit
// in the same poll is still attributed.
func TestGitWatchOwnCommitFromOtherDeviceNotClaimed(t *testing.T) {
	_, laptop, desktop, home := twoDevices(t)
	const me = "me@example.com"
	session := Session{DeviceID: "dev-desktop", TaskRoot: home}
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/mine.go")
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/branch.go")
	pollGitWatchWorkspace(session) // cold-start baseline

	// Laptop: a commit on main and one on a pushed branch.
	writeCommitFile(t, laptop, "foo.go", "package main\n\nfunc laptop() {}\n")
	gitAs(t, laptop, me, "commit", "-q", "-am", "laptop edits foo")
	onMain := gitAs(t, laptop, me, "rev-parse", "HEAD")
	gitAs(t, laptop, me, "push", "-q", "origin", "main")
	gitAs(t, laptop, me, "switch", "-q", "-c", "feat")
	writeCommitFile(t, laptop, "branch.go", "package main\n")
	gitAs(t, laptop, me, "add", "-A")
	gitAs(t, laptop, me, "commit", "-q", "-m", "laptop branch work")
	onBranch := gitAs(t, laptop, me, "rev-parse", "HEAD")
	gitAs(t, laptop, me, "push", "-q", "origin", "feat")

	// Desktop: pull (HEAD pass) + the fetch brings origin/feat (off-HEAD pass),
	// then its own commit.
	gitAs(t, desktop, me, "pull", "-q", "--ff-only")
	writeCommitFile(t, desktop, "mine.go", "package main\n\nfunc mine() {}\n")
	gitAs(t, desktop, me, "add", "-A")
	gitAs(t, desktop, me, "commit", "-q", "-m", "desktop's own")
	ours := gitAs(t, desktop, me, "rev-parse", "HEAD")

	pollGitWatchWorkspace(session)
	got := attributedShas(t, state.OutboxPath())
	if countSha(got, onMain) != 0 {
		t.Errorf("laptop commit %s pulled onto the desktop's HEAD was claimed: %v", onMain, got)
	}
	if countSha(got, onBranch) != 0 {
		t.Errorf("laptop branch commit %s fetched to the desktop was claimed: %v", onBranch, got)
	}
	if countSha(got, ours) != 1 {
		t.Errorf("desktop's own commit %s must be attributed exactly once, got %v", ours, got)
	}
}

// The agent commits on a branch, pushes, and deletes the branch: its reflog goes
// with it, and only the remote-tracking ref's "update by push" survives. Commits
// under a pushed tip are this device's — but not the fetched base they sit on.
func TestGitLocallyCreatedCommitsFollowsPushedTip(t *testing.T) {
	_, laptop, desktop, _ := twoDevices(t)
	const me = "me@example.com"
	writeCommitFile(t, laptop, "foo.go", "package main\n\nfunc laptop() {}\n")
	gitAs(t, laptop, me, "commit", "-q", "-am", "laptop")
	gitAs(t, laptop, me, "push", "-q", "origin", "main")
	gitAs(t, desktop, me, "pull", "-q", "--ff-only")
	base := gitAs(t, desktop, me, "rev-parse", "HEAD")

	gitAs(t, desktop, me, "switch", "-q", "-c", "feat")
	writeCommitFile(t, desktop, "a.go", "package main\n")
	gitAs(t, desktop, me, "add", "-A")
	gitAs(t, desktop, me, "commit", "-q", "-m", "a")
	a := gitAs(t, desktop, me, "rev-parse", "HEAD")
	gitAs(t, desktop, me, "push", "-q", "origin", "feat")
	gitAs(t, desktop, me, "switch", "-q", "main")
	gitAs(t, desktop, me, "branch", "-q", "-D", "feat")
	// Drop HEAD's log too, as a deleted worktree would take it.
	gitAs(t, desktop, me, "reflog", "expire", "--expire=now", "HEAD")

	local := gitLocallyCreatedCommits(desktop, []string{a, base})
	if _, ok := local[a]; !ok {
		t.Errorf("pushed commit %s not recognised as this device's: %v", a, local)
	}
	if _, ok := local[base]; ok {
		t.Errorf("fetched base %s claimed as this device's", base)
	}
}

// No reflog at all: nothing can be told apart, so the gate is off.
func TestGitLocallyCreatedCommitsNoReflogIsNoGate(t *testing.T) {
	ws, git, gitOut := gitRepo(t)
	git("config", "core.logAllRefUpdates", "false")
	git("commit", "--allow-empty", "-m", "A")
	if local := gitLocallyCreatedCommits(ws, []string{gitOut("rev-parse", "HEAD")}); local != nil {
		t.Fatalf("no reflog must disable the gate, got %v", local)
	}
}
