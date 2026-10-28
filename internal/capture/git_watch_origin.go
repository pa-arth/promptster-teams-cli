package capture

import (
	"cmp"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Device-origin gate: a commit that ARRIVED from another machine is not this
// device's to claim.
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
// THE RULE IS "REFUSE ONLY WHAT PROVABLY ARRIVED", NOT "CLAIM ONLY WHAT
// PROVABLY ORIGINATED HERE". Losing a local commit is worse than a double
// claim — the backend can see a double claim (multi-session-sha-claims), it
// cannot see a commit that was never sent — and local evidence is fragile:
// deleting a worktree takes its HEAD log, deleting a branch takes the branch
// log, and what survives ("branch: Created from", "merge x: Fast-forward",
// "checkout:") says nothing about where the commit was written. Remote arrival
// is recorded robustly instead: every fetch and pull moves a remote-tracking
// ref, whose reflog lives in the common dir and outlives every worktree.
//
// So a candidate commit is refused only when it is reachable from a position
// that came from a remote — a refs/remotes/* reflog entry other than this
// device's own "update by push", a "clone", or a pull's fast-forward / rebase
// upstream — AND it is not reachable from anything this device pushed, AND no
// reflog entry records this device writing it. Everything else is claimed.
//
// nil means NO GATE: no reflog at all (core.logAllRefUpdates off), or the probe
// failed. Either way nothing is proven to have arrived, so the caller keeps its
// other gates only — in doubt, claim.

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

// reflogArrived reports whether a reflog entry records a position that came
// from a remote. selector is %gD ("refs/remotes/origin/main@{0}", "HEAD@{2}").
func reflogArrived(selector, subject string) bool {
	action, msg, _ := strings.Cut(subject, ": ")
	if action == "update by push" {
		return false // this device's own push
	}
	if strings.HasPrefix(selector, "refs/remotes/") || strings.HasPrefix(action, "clone") {
		return true
	}
	// `git pull <url>` without a remote updates no remote-tracking ref; the
	// branch/HEAD entry is then the only record. A pull --rebase's "(start)"
	// checks out the upstream it is about to replay onto.
	return strings.HasPrefix(action, "pull") &&
		(strings.HasPrefix(msg, "Fast-forward") || strings.Contains(action, "(start)"))
}

// reflogEntry is one reflog line. ref is the log it belongs to ("HEAD" of a
// given checkout, "refs/heads/x", "refs/remotes/origin/x"); t is when the ref
// moved, in unix seconds on THIS machine's clock.
type reflogEntry struct {
	sha, ref, subject string
	t                 int64
}

// reflogEntries returns every reflog entry this repository's checkouts hold —
// all refs once, plus each linked worktree's own HEAD log — newest-first per ref.
func reflogEntries(root string) ([]reflogEntry, bool) {
	const format = "--format=%H%x09%gD%x09%gs"
	probes := [][]string{{"-C", root, "log", "-g", "--all", "--date=unix", format}}
	for _, sib := range gitSiblingWorktrees(root) {
		probes = append(probes, []string{"-C", sib, "log", "-g", "--date=unix", format, "HEAD"})
	}
	var out []reflogEntry
	for i, args := range probes {
		// #nosec G204 -- constant argv; the dirs are checkouts this install already polls. Read-only.
		raw, err := exec.Command("git", args...).Output()
		if err != nil {
			if i == 0 {
				return nil, false
			}
			continue // a sibling with no HEAD log adds nothing
		}
		for _, line := range strings.Split(string(raw), "\n") {
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) != 3 || parts[0] == "" {
				continue
			}
			ref, at, _ := strings.Cut(parts[1], "@{")
			t, _ := strconv.ParseInt(strings.TrimSuffix(at, "}"), 10, 64)
			if ref == "HEAD" && i > 0 {
				ref = "HEAD#" + strconv.Itoa(i) // each worktree's HEAD is its own log
			}
			out = append(out, reflogEntry{sha: parts[0], ref: ref, subject: parts[2], t: t})
		}
	}
	return out, true
}

// candidateMeta reads, in one spawn, each candidate's parents and committer
// time: the parents bound every range walk, and the oldest time bounds which
// reflog entries can matter (nothing arrives before it was committed).
func candidateMeta(root string, shas []string, want map[string]struct{}) (outside []string, oldest int64, ok bool) {
	// #nosec G204 -- constant argv; SHAs on stdin come from rev-list output. Read-only.
	cmd := exec.Command("git", "-C", root, "log", "--no-walk=unsorted", "--stdin", "--format=%H %ct %P")
	cmd.Stdin = strings.NewReader(strings.Join(shas, "\n") + "\n")
	raw, err := cmd.Output()
	if err != nil {
		return nil, 0, false
	}
	seen := map[string]struct{}{}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if ct, err := strconv.ParseInt(f[1], 10, 64); err == nil && (oldest == 0 || ct < oldest) {
			oldest = ct
		}
		for _, p := range f[2:] {
			if _, in := want[p]; in {
				continue
			}
			if _, dup := seen[p]; !dup {
				seen[p] = struct{}{}
				outside = append(outside, p)
			}
		}
	}
	return outside, oldest, true
}

// maxReflogRanges caps the range walks per call. Over the cap the oldest ranges
// go unexamined, which can only leave an arrival unproven — a claim, never a loss.
const maxReflogRanges = 64

// clockSkewSlack allows for the other machine's clock being ahead of this one.
const clockSkewSlack = 24 * 60 * 60

// gitArrivedCommits returns the subset of shas that provably arrived from
// another machine (see the header), nil when there is no reflog to judge by.
//
// Each reflog entry moves one ref from old to new, so new ^old is exactly what
// that entry brought — per ref, which is the point: a fast-forward of a LOCAL
// branch to a local commit, or a branch created at one, is not an arrival, and
// nothing about those entries can make the commit look foreign.
func gitArrivedCommits(root string, shas []string) map[string]struct{} {
	entries, ok := reflogEntries(root)
	if !ok {
		state.HookDebugf("git-watch: reflog probe failed on %s; origin gate off for %d commit(s)", gitWatchRootKey(root), len(shas))
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	arrived := map[string]struct{}{}
	if len(shas) == 0 {
		return arrived
	}
	want := make(map[string]struct{}, len(shas))
	for _, s := range shas {
		want[s] = struct{}{}
	}
	outside, oldest, metaOK := candidateMeta(root, shas, want)
	horizon := int64(0)
	if metaOK && oldest > 0 {
		horizon = oldest - clockSkewSlack
	}

	type moved struct {
		newSha, oldSha string
		t              int64
	}
	written := map[string]struct{}{}
	var fetches, pushes []moved
	var arrivalTips []moved // every arrival, any age: what a push stood on
	byRef := map[string][]reflogEntry{}
	var refs []string
	for _, e := range entries {
		if _, seen := byRef[e.ref]; !seen {
			refs = append(refs, e.ref)
		}
		byRef[e.ref] = append(byRef[e.ref], e)
	}
	for _, ref := range refs {
		log := byRef[ref]
		for i := len(log) - 1; i >= 0; i-- { // oldest first
			e := log[i]
			old := ""
			if i+1 < len(log) {
				old = log[i+1].sha
			}
			action, _, _ := strings.Cut(e.subject, ": ")
			m := moved{e.sha, old, e.t}
			switch {
			case reflogCreatedHere(e.subject):
				written[e.sha] = struct{}{}
			case action == "update by push":
				if e.t >= horizon {
					pushes = append(pushes, m)
				}
			case reflogArrived(ref+"@{0}", e.subject):
				arrivalTips = append(arrivalTips, m)
				if e.t >= horizon {
					fetches = append(fetches, m)
				}
			}
		}
	}
	if len(fetches) == 0 {
		return arrived
	}

	// walk lists the candidates reachable from tip but not from any of stops.
	walk := func(tip string, stops []string) (map[string]struct{}, bool) {
		var in strings.Builder
		in.WriteString(tip + "\n")
		for _, x := range stops {
			if x != "" {
				in.WriteString("^" + x + "\n")
			}
		}
		for _, x := range outside {
			in.WriteString("^" + x + "\n")
		}
		// #nosec G204 -- constant argv; SHAs on stdin come from this repo's reflog and rev-list. Read-only.
		cmd := exec.Command("git", "-C", root, "rev-list", "--ignore-missing", "--stdin")
		cmd.Stdin = strings.NewReader(in.String())
		raw, err := cmd.Output()
		if err != nil {
			return nil, false
		}
		hit := map[string]struct{}{}
		for _, sha := range parseRevListShas(raw) {
			if _, w := want[sha]; w {
				hit[sha] = struct{}{}
			}
		}
		return hit, true
	}
	newestFirst := func(ms []moved) []moved {
		slices.SortFunc(ms, func(a, b moved) int { return cmp.Compare(b.t, a.t) })
		return ms[:min(len(ms), maxReflogRanges)]
	}

	for _, f := range newestFirst(fetches) {
		hit, ok := walk(f.newSha, []string{f.oldSha})
		if !ok {
			continue // unproven: claim
		}
		for sha := range hit {
			arrived[sha] = struct{}{}
		}
	}
	if len(arrived) == 0 {
		return arrived
	}
	// What this device pushed is its own, wherever it was seen again later. A
	// push's range stops at whatever had already ARRIVED before it, so a pushed
	// branch does not vouch for the fetched base it was built on.
	for _, p := range newestFirst(pushes) {
		stops := []string{p.oldSha}
		for _, a := range arrivalTips {
			// Reflog times are whole seconds, and a pull-then-push lands in
			// one second all the time; a tie counts as "already arrived". A
			// fetch that truly followed a push within the second would need
			// to bring the pushed commits back on another ref, and even then
			// a surviving commit entry still claims them.
			if a.t <= p.t {
				stops = append(stops, a.newSha)
			}
		}
		hit, ok := walk(p.newSha, stops)
		if !ok {
			// Could not tell what this push covered: in doubt, trust it fully.
			hit, ok = walk(p.newSha, nil)
			if !ok {
				continue
			}
		}
		for sha := range hit {
			delete(arrived, sha)
		}
	}
	for sha := range written {
		delete(arrived, sha)
	}
	return arrived
}

// gitDeviceClaimableCommits is shas minus gitArrivedCommits, or nil for no gate.
func gitDeviceClaimableCommits(root string, shas []string) map[string]struct{} {
	arrived := gitArrivedCommits(root, shas)
	if arrived == nil {
		return nil
	}
	claimable := make(map[string]struct{}, len(shas))
	for _, s := range shas {
		if _, gone := arrived[s]; !gone {
			claimable[s] = struct{}{}
		}
	}
	return claimable
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
// one of this repository's identities (gitOwnCommits) AND not arrived from
// another machine (gitDeviceClaimableCommits). nil only when neither gate can
// judge.
func gitClaimableCommits(root string, shas []string) map[string]struct{} {
	if len(shas) == 0 {
		return gitOwnCommits(root, shas)
	}
	return intersectGates(gitOwnCommits(root, shas), gitDeviceClaimableCommits(root, shas))
}
