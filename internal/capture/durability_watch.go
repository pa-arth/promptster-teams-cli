package capture

import (
	"os/exec"
	"strings"
	"sync"
)

// Durability watcher wiring: fold DEFAULT-BRANCH commits into the durability
// ledger on the same off-critical-path 60s cadence as the attribution watcher.
//
// Durability is stateful and directional, so — unlike the stateless
// commit_attribution watcher, which follows working HEAD — it MUST advance only
// on the default branch. Feeding it feature-branch commits would double-count
// against the eventual squash-merge (the same lines would churn on the branch
// and then re-land under a new SHA on default). So durability keeps its OWN
// cursor over the default-branch tip: feature-branch commits are invisible to it
// until they actually land on the default branch.
//
// Spawn budget stays constant-time per root per poll: the default-branch REF is
// resolved once and cached (symbolic-ref), leaving one `rev-parse` for its tip
// and — only when the tip moved — one `rev-list` for the new commits. Never a
// spawn per commit beyond the single `git show` durability already reuses.

// durDefaultRefCache memoizes the resolved default-branch ref per rootKey so the
// symbolic-ref resolution spawns once per root for the life of the process, not
// every poll. Guarded because StartGitWatch may run concurrent workspaces.
var (
	durDefaultRefMu    sync.Mutex
	durDefaultRefCache = map[string]string{}
)

// gitSymbolicRef resolves a symbolic ref to its full target ref name, or "" when
// it does not exist / is not symbolic. One read-only spawn.
func gitSymbolicRef(root, name string) string {
	// #nosec G204 -- constant argv; root is a discovered workspace dir, name is a constant ref. Read-only.
	out, err := exec.Command("git", "-C", root, "symbolic-ref", "--quiet", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRefExists reports whether a ref resolves. One read-only spawn (fallback
// path only, then cached).
func gitRefExists(root, ref string) bool {
	// #nosec G204 -- constant argv; root discovered, ref is a constant. Read-only.
	err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", ref).Run()
	return err == nil
}

// resolveDefaultRef picks the ref durability tracks as "the default branch":
//  1. the remote's declared default (refs/remotes/origin/HEAD) — authoritative
//     when a remote exists, and correct regardless of which branch is checked
//     out or which local branch has advanced;
//  2. for a remote-less repo, the conventional local default if it exists;
//  3. otherwise the checked-out branch (best effort for a detached/renamed local).
//
// Returns "" only for a repo with no resolvable branch at all (unborn/detached
// with no default), which disables durability scope for that root.
func resolveDefaultRef(root string) string {
	if ref := gitSymbolicRef(root, "refs/remotes/origin/HEAD"); ref != "" {
		return ref
	}
	for _, name := range []string{"refs/heads/main", "refs/heads/master"} {
		if gitRefExists(root, name) {
			return name
		}
	}
	return gitSymbolicRef(root, "HEAD")
}

// durabilityDefaultRef returns the cached default-branch ref for a root,
// resolving (and memoizing) it on first use. Only a NON-EMPTY resolution is
// cached: an unborn/detached repo with no default yet (empty result) is re-probed
// every poll so durability starts as soon as its default branch is created,
// instead of staying disabled for that root until the process restarts. The
// re-probe is a handful of constant-time read-only spawns off the critical path.
func durabilityDefaultRef(root string) string {
	key := gitWatchRootKey(root)
	durDefaultRefMu.Lock()
	if ref, ok := durDefaultRefCache[key]; ok {
		durDefaultRefMu.Unlock()
		return ref
	}
	durDefaultRefMu.Unlock()

	ref := resolveDefaultRef(root)
	if ref == "" {
		return "" // not resolvable yet — re-probe next poll rather than cache it
	}

	durDefaultRefMu.Lock()
	durDefaultRefCache[key] = ref
	durDefaultRefMu.Unlock()
	return ref
}

// durabilityDefaultTip resolves the default branch's current tip SHA (one
// rev-parse; the ref itself is cached). ok=false when there is no default scope
// or the tip is unresolvable.
func durabilityDefaultTip(root string) (ref, tip string, ok bool) {
	ref = durabilityDefaultRef(root)
	if ref == "" {
		return "", "", false
	}
	// #nosec G204 -- constant argv; root discovered, ref is a resolved ref name (not user input). Read-only.
	out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", ref).Output()
	if err != nil {
		return "", "", false
	}
	tip = strings.TrimSpace(string(out))
	if tip == "" {
		return "", "", false
	}
	return ref, tip, true
}

// durabilityCursor returns the last default-branch tip durability processed for
// a root ("" when never seen).
func durabilityCursor(rootKey string) string {
	return loadDurabilityLedger().Cursors[rootKey]
}

// advanceDurabilityCursor persists the default-branch cursor for a root as a
// single locked read-modify-write, preserving any tracked-range update written
// by a concurrent session between this load and save.
func advanceDurabilityCursor(rootKey, tip string) {
	mutateDurabilityLedger(func(led *durabilityLedger) {
		if led.Cursors == nil {
			led.Cursors = map[string]string{}
		}
		led.Cursors[rootKey] = tip
	})
}

// pollDurability advances durability for every root over its DEFAULT branch
// only, then harvests matured spans. nowMs is injected by the caller (top-level
// reads the clock) so tests drive time directly.
//
// Cold start baselines the cursor to the current default tip WITHOUT processing
// pre-existing history — durability tracks AI lines forward from first sight,
// exactly as the attribution watcher baselines HEAD (we cannot know which
// pre-existing lines were AI). From then on, only commits that land on the
// default branch advance it.
func pollDurability(session Session, roots []string, nowMs int64) {
	for _, root := range roots {
		rootKey := gitWatchRootKey(root)
		_, tip, ok := durabilityDefaultTip(root)
		if !ok {
			continue
		}
		lastSeen := durabilityCursor(rootKey)
		switch {
		case lastSeen == "":
			advanceDurabilityCursor(rootKey, tip) // baseline only
		case lastSeen != tip:
			commits, ok := gitNewCommits(root, lastSeen, tip)
			if !ok {
				continue // inconclusive — keep the cursor, retry next poll
			}
			for i := len(commits) - 1; i >= 0; i-- { // oldest-first
				pollDurabilityCommit(root, rootKey, session, commits[i], nowMs)
			}
			if len(commits) > 0 {
				advanceDurabilityCursor(rootKey, commits[0]) // newest returned (clamp-safe)
			} else {
				advanceDurabilityCursor(rootKey, tip) // backward move / nothing new
			}
		}
		harvestDurable(session, root, rootKey, nowMs)
	}
}
