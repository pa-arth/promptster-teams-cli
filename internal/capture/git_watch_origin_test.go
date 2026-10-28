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

// The agent commits on a branch, pushes, and deletes the branch and HEAD's log
// (as a deleted worktree would). Its commit is still claimed; the fetched base it
// sits on, which provably arrived via origin/main, is not.
func TestGitDeviceClaimableCommitsPushedBranchAndFetchedBase(t *testing.T) {
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
	gitAs(t, desktop, me, "reflog", "expire", "--expire=now", "HEAD")

	claim := gitDeviceClaimableCommits(desktop, []string{a, base})
	if _, ok := claim[a]; !ok {
		t.Errorf("pushed local commit %s was refused: %v", a, claim)
	}
	if _, ok := claim[base]; ok {
		t.Errorf("fetched base %s was claimed", base)
	}
}

// localWorktreeCommit commits in a temporary linked worktree of desktop and
// returns the worktree path and the commit.
func localWorktreeCommit(t *testing.T, desktop string) (string, string) {
	t.Helper()
	const me = "me@example.com"
	wt := filepath.Join(t.TempDir(), "wt")
	gitAs(t, desktop, me, "worktree", "add", "-q", "-b", "agent", wt)
	writeCommitFile(t, wt, "agent.go", "package main\n")
	gitAs(t, wt, me, "add", "-A")
	gitAs(t, wt, me, "commit", "-q", "-m", "agent work")
	return wt, gitAs(t, wt, me, "rev-parse", "HEAD")
}

// Greptile #293: commit and push from a temporary worktree, create a second
// branch at that commit, then remove the original branch and the worktree. Only
// "update by push" and "branch: Created from" survive; neither shows the commit
// arriving from elsewhere, so it must still be claimed.
func TestGitDeviceClaimableCommitsDeletedWorktreeBranchCopy(t *testing.T) {
	_, _, desktop, _ := twoDevices(t)
	const me = "me@example.com"
	wt, c := localWorktreeCommit(t, desktop)
	gitAs(t, wt, me, "push", "-q", "origin", "agent")
	gitAs(t, desktop, me, "branch", "keep", c)
	gitAs(t, desktop, me, "worktree", "remove", "--force", wt)
	gitAs(t, desktop, me, "branch", "-q", "-D", "agent")

	if claim := gitDeviceClaimableCommits(desktop, []string{c}); claim != nil {
		if _, ok := claim[c]; !ok {
			t.Fatalf("locally written commit %s was refused after its worktree was deleted", c)
		}
	}
}

// Greptile #294: commit in a worktree, fast-forward the main checkout to it,
// then delete branch and worktree before the poll. The surviving
// "merge agent: Fast-forward" is local, not an arrival.
func TestGitDeviceClaimableCommitsDeletedWorktreeFastForward(t *testing.T) {
	_, _, desktop, _ := twoDevices(t)
	const me = "me@example.com"
	wt, c := localWorktreeCommit(t, desktop)
	gitAs(t, desktop, me, "merge", "-q", "--ff-only", "agent")
	gitAs(t, desktop, me, "worktree", "remove", "--force", wt)
	gitAs(t, desktop, me, "branch", "-q", "-D", "agent")

	if claim := gitDeviceClaimableCommits(desktop, []string{c}); claim != nil {
		if _, ok := claim[c]; !ok {
			t.Fatalf("locally written commit %s was refused after a local fast-forward", c)
		}
	}
}

func TestReflogArrived(t *testing.T) {
	for _, tc := range []struct {
		selector, subject string
		want              bool
	}{
		{"refs/remotes/origin/main@{0}", "pull --ff-only: fast-forward", true},
		{"refs/remotes/origin/feat@{0}", "fetch: storing head", true},
		{"refs/remotes/origin/feat@{0}", "update by push", false},
		{"refs/heads/main@{1}", "clone: from /x", true},
		{"HEAD@{3}", "pull --rebase (start): checkout 6a1f057", true},
		{"refs/heads/main@{0}", "pull https://x/y main: Fast-forward", true},
		{"refs/heads/main@{0}", "merge agent: Fast-forward", false},
		{"refs/heads/keep@{0}", "branch: Created from 6a1f057", false},
		{"HEAD@{0}", "checkout: moving from agent to main", false},
		{"HEAD@{0}", "reset: moving to HEAD~1", false},
	} {
		if got := reflogArrived(tc.selector, tc.subject); got != tc.want {
			t.Errorf("reflogArrived(%q, %q) = %v, want %v", tc.selector, tc.subject, got, tc.want)
		}
	}
}

// No reflog at all: nothing can be shown to have arrived, so the gate is off.
func TestGitDeviceClaimableCommitsNoReflogIsNoGate(t *testing.T) {
	ws, git, gitOut := gitRepo(t)
	git("config", "core.logAllRefUpdates", "false")
	git("commit", "--allow-empty", "-m", "A")
	if claim := gitDeviceClaimableCommits(ws, []string{gitOut("rev-parse", "HEAD")}); claim != nil {
		t.Fatalf("no reflog must disable the gate, got %v", claim)
	}
}
