package capture

import (
	"os/exec"
	"strconv"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Device-origin gate: only the device that CREATED a commit claims it.
//
// The author gate (gitOwnCommits) keeps a colleague's pulled commits out, but it
// cannot tell one engineer's two machines apart — both carry the same
// user.email. So a commit written on the laptop and pushed reaches the desktop
// through a fetch (the off-HEAD pass reads remote-tracking tips) or a pull (the
// HEAD pass), passes the author gate, and is matched against the DESKTOP's
// ai-paths ledger. Prod 2026-10-28: on one org 145 SHAs were claimed by the same
// engineer's sessions on two devices, typically days apart, still arriving
// after 0.40.3 shipped the author gate.
//
// The only local record of where a commit came from is the reflog. A commit made
// here leaves an entry whose subject names the action that wrote it ("commit:",
// "commit (amend):", "cherry-pick:", "revert:", a rebase "(pick)", "Merge made
// by"); a commit that arrived from a remote only ever moves a ref with "fetch:",
// "pull: Fast-forward", "clone:" or "checkout:", and intermediate commits of a
// fast-forward get no entry at all.
//
// One gap the direct entries leave is the agent that commits on a branch, pushes
// and deletes branch AND worktree inside one poll: those reflogs go with them.
// What survives is the remote-tracking ref's "update by push" entry — a push
// from THIS device. So commits reachable from a pushed tip are ours too, minus
// anything reachable from a ref position that came from elsewhere (so a pushed
// branch built on a fetched base does not claim the base).
//
// nil means NO GATE: the repository has no reflog at all (core.logAllRefUpdates
// off, or a fresh clone with nothing in it), so nothing can be told apart and
// the caller keeps its other gates only. A probe failure returns an EMPTY set:
// nothing is proven local, so nothing is claimed — an undercount, never a
// misattribution, matching gitOwnCommits.

// maxPushedAncestry bounds the pushed-tip expansion's rev-list.
const maxPushedAncestry = 2000

// reflogCreatedHere reports whether a reflog subject ("<action>: <message>")
// records a commit being WRITTEN on this device.
func reflogCreatedHere(subject string) bool {
	action, msg, _ := strings.Cut(subject, ": ")
	switch {
	case strings.HasPrefix(action, "commit"), // commit, commit (initial|amend|merge)
		strings.HasPrefix(action, "cherry-pick"),
		strings.HasPrefix(action, "revert"),
		action == "am", strings.HasPrefix(action, "am "):
		return true
	}
	for _, step := range []string{"(pick)", "(reword)", "(edit)", "(squash)", "(fixup)"} {
		if strings.Contains(action, step) { // rebase -i (pick), pull --rebase (pick)
			return true
		}
	}
	return strings.HasPrefix(msg, "Merge made by") // a non-fast-forward merge or pull
}

// reflogEntries returns every reflog (sha, subject) this repository's checkouts
// hold: all refs once, plus each linked worktree's own HEAD log.
func reflogEntries(root string) (shas, subjects []string, ok bool) {
	probes := [][]string{{"-C", root, "log", "-g", "--all", "--format=%H%x09%gs"}}
	for _, sib := range gitSiblingWorktrees(root) {
		probes = append(probes, []string{"-C", sib, "log", "-g", "--format=%H%x09%gs", "HEAD"})
	}
	for i, args := range probes {
		// #nosec G204 -- constant argv; the dirs are checkouts this install already polls. Read-only.
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			if i == 0 {
				return nil, nil, false
			}
			continue // a sibling with no HEAD log adds nothing
		}
		for _, line := range strings.Split(string(out), "\n") {
			sha, subject, found := strings.Cut(line, "\t")
			if !found || sha == "" {
				continue
			}
			shas = append(shas, sha)
			subjects = append(subjects, subject)
		}
	}
	return shas, subjects, true
}

// gitLocallyCreatedCommits returns the subset of shas this device wrote (see
// the header), nil when there is no reflog to judge by.
func gitLocallyCreatedCommits(root string, shas []string) map[string]struct{} {
	logShas, subjects, ok := reflogEntries(root)
	if !ok {
		state.HookDebugf("git-watch: reflog probe failed on %s; claiming none of %d commit(s)", gitWatchRootKey(root), len(shas))
		return map[string]struct{}{}
	}
	if len(logShas) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(shas))
	for _, s := range shas {
		want[s] = struct{}{}
	}
	local := map[string]struct{}{}
	var pushed, elsewhere []string
	for i, sha := range logShas {
		action, _, _ := strings.Cut(subjects[i], ": ")
		switch {
		case reflogCreatedHere(subjects[i]):
			if _, w := want[sha]; w {
				local[sha] = struct{}{}
			}
		case action == "update by push":
			pushed = append(pushed, sha)
		default:
			elsewhere = append(elsewhere, sha)
		}
	}
	if len(pushed) == 0 || len(local) == len(want) {
		return local
	}
	var in strings.Builder
	for _, p := range pushed {
		in.WriteString(p + "\n")
	}
	for _, e := range elsewhere {
		in.WriteString("^" + e + "\n")
	}
	// #nosec G204 -- constant argv; SHAs on stdin come from this repo's own reflog. Read-only.
	cmd := exec.Command("git", "-C", root, "rev-list", "--ignore-missing", "--stdin", "-n", strconv.Itoa(maxPushedAncestry))
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		return local // the direct entries still stand
	}
	for _, sha := range parseRevListShas(out) {
		if _, w := want[sha]; w {
			local[sha] = struct{}{}
		}
	}
	return local
}

// intersectGates combines two optional gates, where nil means "no gate".
func intersectGates(a, b map[string]struct{}) map[string]struct{} {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	both := map[string]struct{}{}
	for sha := range a {
		if _, ok := b[sha]; ok {
			both[sha] = struct{}{}
		}
	}
	return both
}

// gitClaimableCommits is the gate every attribution pass applies: authored by
// one of this repository's identities (gitOwnCommits) AND written on this
// device (gitLocallyCreatedCommits). nil only when neither gate can judge.
func gitClaimableCommits(root string, shas []string) map[string]struct{} {
	if len(shas) == 0 {
		return gitOwnCommits(root, shas)
	}
	return intersectGates(gitOwnCommits(root, shas), gitLocallyCreatedCommits(root, shas))
}
