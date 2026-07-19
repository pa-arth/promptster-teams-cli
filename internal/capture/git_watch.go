package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Out-of-band git watcher (PR2: detection only).
//
// A periodic timer, deliberately OFF any latency-sensitive path, notices when a
// root's HEAD advances and surfaces the new commit SHAs. It computes NO diffs
// and NO attribution and emits NOTHING to the outbox — that is a later PR's job,
// which will consume readAiTouchedPaths() plus the SHAs surfaced here.
//
// Spawn budget is strict and constant-time per root per poll: at most ONE
// `git rev-parse HEAD` (always) and ONE `git rev-list <lastSeen>..HEAD` (only
// when HEAD moved). Never a spawn per commit, file, or line. The root key is
// computed WITHOUT spawning git so the two above stay the whole budget.
const gitWatchInterval = 60 * time.Second

// gitWatchMaxCommitsPerPoll bounds how many commits a single poll of ONE root
// surfaces — and therefore how many `git show` spawns attribution makes. A HEAD
// that jumps far from the cursor (a `git checkout other-branch`, a large rebase)
// would otherwise return the whole `lastSeen..head` range in one burst; we clamp
// to the OLDEST N and advance the cursor only to the newest returned SHA, so the
// remainder drains on subsequent polls in order rather than being dropped. It
// doubles as the recovery-window size when the cursor is gc'd (see
// gitNewCommits). A package var, not a const, so a test can lower it without
// building hundreds of commits. Re-seeing a SHA on a clamp/overlap is idempotent
// (the backend upserts by SHA and attribution re-derives from the current
// ledger), so the bounded batching is never wrong.
var gitWatchMaxCommitsPerPoll = 100

func gitWatchCursorsPath() string {
	return filepath.Join(state.StateDir(), "git-watch-cursors.json")
}

// gitWatchCursors persists the last-seen HEAD per root, keyed by an opaque root
// identifier (see gitWatchRootKey). The value set is bounded by the number of
// worktrees a workspace has, so it needs no TTL.
type gitWatchCursors struct {
	V       int               `json:"v"`
	Cursors map[string]string `json:"cursors"` // rootKey -> last-seen HEAD sha
}

const gitWatchCursorsVersion = 1

// gitWatchRootKey derives a stable, privacy-safe identifier for a root that is
// unique PER WORKTREE (two worktrees of the same repo share a remote slug but
// have distinct absolute paths, so a slug alone would collide their cursors).
//
// It is a one-way sha256 of the absolute path truncated to 16 hex chars — the
// exact primitive workspaceKey() already persists for a non-remote workspace,
// so this leaks no filesystem path and stays consistent with existing local
// state. Crucially it spawns NO git, keeping the per-poll budget at rev-parse +
// rev-list. (workspaceKey/gitRemoteSlug would add a `git config` spawn per root
// per poll for a human-readable slug we don't need in a local-only cursor file.)
//
// The path is canonicalized via resolvePath (symlink-resolved, with a cleaned
// fallback) so the key is caller-independent: a writer and reader referring to
// the same dir through different spellings (e.g. /tmp vs /private/tmp on macOS,
// or a symlinked checkout) agree on the key.
func gitWatchRootKey(root string) string {
	return ingest.Sha256Hex(resolvePath(root))[:16]
}

// ledgerScope maps a polled repo root onto capture's AI ledgers, which are
// anchored to the daemon's single workspace (session.TaskRoot) rather than to
// each repo. In the autostart daemon TaskRoot is the HOME dir and `root` is a
// repo discovered under it, so a path that capture recorded workspace-relative as
// "<rel(home,root)>/<p>" under gitWatchRootKey(home) must be looked up WITH that
// prefix. When root == taskRoot (the explicit-repo / dev / `git-watch`
// subcommand case) the prefix is "" and the key is gitWatchRootKey(root), so
// reconciliation is byte-for-byte what it was before repo discovery existed.
type ledgerScope struct {
	aiKey  string // the root key the ai-paths / bash-windows ledgers are stored under
	prefix string // POSIX rel(taskRoot, root); "" when root == taskRoot (or fallback)
}

// resolveLedgerScope computes the scope for reconciling `root`'s committed paths
// against ledgers anchored to `taskRoot`. rel is taken in resolvePath-canonical
// space so a symlinked home (macOS /var vs /private/var, or a symlinked checkout)
// still yields the correct workspace-relative prefix — the symlinked absolute
// prefix cancels on both sides, leaving exactly the sub-path capture stored. A
// root that is NOT under taskRoot (a discovered repo outside home — rare) falls
// back to the per-root key with no prefix: conservative, and identical to the
// pre-discovery behavior.
func resolveLedgerScope(root, taskRoot string) ledgerScope {
	if taskRoot == "" {
		return ledgerScope{aiKey: gitWatchRootKey(root)}
	}
	rel, err := filepath.Rel(resolvePath(taskRoot), resolvePath(root))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ledgerScope{aiKey: gitWatchRootKey(root)} // root not under taskRoot
	}
	if rel == "." {
		rel = ""
	}
	return ledgerScope{aiKey: gitWatchRootKey(taskRoot), prefix: filepath.ToSlash(rel)}
}

// ledgerPath translates a repo-relative committed path into the workspace-relative
// key the ai-paths ledger stored it under.
func (s ledgerScope) ledgerPath(committedRel string) string {
	if s.prefix == "" {
		return committedRel
	}
	return s.prefix + "/" + committedRel
}

// gitRootOf walks up from dir to the nearest ancestor that is a git repo root
// (its .git exists — reusing isGitRepoRoot), or ok=false at the filesystem root.
// Stat-only per level, bounded by path depth: NO git spawn, so it stays off the
// constant-time budget the poll loop guards.
func gitRootOf(dir string) (string, bool) {
	for {
		if isGitRepoRoot(dir) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached the filesystem root without finding .git
			return "", false
		}
		dir = parent
	}
}

// discoverAiRepoRoots derives the repo roots to poll from capture's ai-paths
// ledger, so the single autostart daemon (TaskRoot == HOME, not a repo) actually
// polls the engineer's real repos instead of the un-pollable home dir. Every
// AI-touched path is recorded workspace-relative under gitWatchRootKey(taskRoot),
// so the repo that owns it is the nearest ancestor dir containing a .git — found
// by a stat-only walk (gitRootOf), deduped. Bounded by the number of distinct
// AI-touched files and each walk by path depth; runs on the 60s timer, never the
// critical path, and spawns NO git.
func discoverAiRepoRoots(taskRoot string) []string {
	base := resolvePath(taskRoot)
	seen := map[string]bool{}
	dirRoot := map[string]string{} // memoize dir -> resolved repo root ("" = none)
	var roots []string
	for rel := range readAiTouchedPaths(gitWatchRootKey(taskRoot)) {
		abs := rel
		if !filepath.IsAbs(rel) {
			abs = filepath.Join(base, rel)
		}
		dir := filepath.Dir(abs)
		root, memoized := dirRoot[dir]
		if !memoized {
			if r, ok := gitRootOf(dir); ok {
				root = r
			}
			dirRoot[dir] = root
		}
		if root != "" && !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots
}

// dedupRootsByKey collapses roots that resolve to the same repo (workspace root
// and a discovered root can be different spellings of one dir), keeping the first
// spelling so each repo is polled — and cursored — exactly once per cycle.
func dedupRootsByKey(roots []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range roots {
		k := resolvePath(r)
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// gitHead returns the root's current HEAD commit, or ok=false when there is no
// commit yet (unborn branch) or the dir is not a git repo. A detached HEAD
// still resolves — it is a real commit — so it is tracked normally.
func gitHead(root string) (string, bool) {
	// #nosec G204 -- constant argv; root is a discovered workspace/worktree dir, not user input. Read-only.
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", false
	}
	return sha, true
}

// gitNewCommits lists commits reachable from head but not from lastSeen, newest
// first, bounded to gitWatchMaxCommitsPerPoll. The bool distinguishes a
// definitive answer from an inconclusive one, and it closes the robustness holes
// a bare `rev-list lastSeen..head` leaves open:
//
//   - Normal range (lastSeen resolves): the new commits, ok=true. This covers a
//     plain fast-forward AND any surviving-object rewrite — amend, rebase,
//     cherry-pick, squash — because the rewritten tip is reachable from head but
//     not from lastSeen, so it re-enters through the same range. A burst larger
//     than the cap keeps the OLDEST cap (rev-list is newest-first, so the tail);
//     the caller advances the cursor only to the newest returned SHA, so the
//     remainder drains on later polls in commit order rather than being dropped.
//     An empty slice means genuinely nothing new (e.g. a backward reset, where
//     the range is empty). Re-seeing a SHA after a branch switch or overlap is
//     idempotent — the backend upserts by SHA and attribution re-derives from
//     the current ledger — so we deliberately do NOT guard against it (a
//     merge-base ancestor check would silently drop amend/rebase detection).
//   - gc'd cursor (lastSeen is unresolvable — pruned after an aggressive
//     rewrite): `rev-list lastSeen..head` ERRORS. We fall back to a bounded
//     recovery window, `rev-list -n <cap> head`, so the tip region is still
//     attributed instead of the commits being skipped forever, ok=true.
//   - Failure (even the recovery rev-list errors — not a repo / bad head): ok=false,
//     so the caller KEEPS the old cursor and retries next poll rather than
//     advancing past an undetermined window.
//
// Spawn budget stays sane: the normal path is one `rev-list`; the fallback adds
// at most one more, only in the rare gc'd case. No merge-base spawn.
func gitNewCommits(root, lastSeen, head string) (commits []string, ok bool) {
	// #nosec G204 -- constant argv; root is a discovered workspace/worktree dir and both SHAs come from git rev-parse output, not user input. Read-only.
	out, err := exec.Command("git", "-C", root, "rev-list", lastSeen+".."+head).Output()
	if err == nil {
		return clampCommitBurst(parseRevListShas(out), root), true
	}
	// lastSeen is unreachable (gc'd/pruned, or a corrupt cursor): recover the tip
	// region rather than skip it. If even that errors, keep the cursor and retry.
	// #nosec G204 -- see above; read-only.
	out, rerr := exec.Command("git", "-C", root, "rev-list",
		"-n", strconv.Itoa(gitWatchMaxCommitsPerPoll), head).Output()
	if rerr != nil {
		return nil, false
	}
	shas := parseRevListShas(out)
	state.HookDebugf("git-watch: cursor %s unreachable on %s; recovered newest %d commit(s) from head",
		lastSeen, gitWatchRootKey(root), len(shas))
	return shas, true
}

// clampCommitBurst bounds a fast-forward range to gitWatchMaxCommitsPerPoll,
// keeping the OLDEST cap commits (rev-list is newest-first, so the tail). The
// caller advances the cursor only to the newest returned SHA, so the remainder
// drains on subsequent polls in commit order rather than being permanently
// dropped.
func clampCommitBurst(shas []string, root string) []string {
	if len(shas) > gitWatchMaxCommitsPerPoll {
		total := len(shas)
		shas = shas[len(shas)-gitWatchMaxCommitsPerPoll:]
		state.HookDebugf("git-watch: %d new commit(s) on %s exceed per-poll cap %d; processing oldest %d this poll, draining the remaining %d on subsequent polls",
			total, gitWatchRootKey(root), gitWatchMaxCommitsPerPoll, gitWatchMaxCommitsPerPoll, total-gitWatchMaxCommitsPerPoll)
	}
	return shas
}

// parseRevListShas splits newest-first rev-list stdout into trimmed, non-empty
// SHAs, preserving order.
func parseRevListShas(out []byte) []string {
	var shas []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			shas = append(shas, line)
		}
	}
	return shas
}

func loadGitWatchCursors() map[string]string {
	cursors := map[string]string{}
	_ = sign.WithBufferLock(gitWatchCursorsPath()+".lock", func() error {
		data, err := os.ReadFile(gitWatchCursorsPath())
		if err != nil {
			return nil
		}
		var onDisk gitWatchCursors
		if json.Unmarshal(data, &onDisk) == nil && onDisk.V == gitWatchCursorsVersion && onDisk.Cursors != nil {
			cursors = onDisk.Cursors
		}
		return nil
	})
	return cursors
}

// saveGitWatchCursors merges the freshly observed heads into the on-disk cursor
// set (re-read under the lock so a transiently-unreadable root keeps its old
// cursor rather than re-baselining). Best-effort: I/O failure never blocks.
func saveGitWatchCursors(heads map[string]string) {
	if len(heads) == 0 {
		return
	}
	_ = sign.WithBufferLock(gitWatchCursorsPath()+".lock", func() error {
		merged := gitWatchCursors{V: gitWatchCursorsVersion, Cursors: map[string]string{}}
		if data, err := os.ReadFile(gitWatchCursorsPath()); err == nil {
			var onDisk gitWatchCursors
			if json.Unmarshal(data, &onDisk) == nil && onDisk.Cursors != nil {
				merged.Cursors = onDisk.Cursors
			}
		}
		for k, v := range heads {
			merged.Cursors[k] = v
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		tmp := gitWatchCursorsPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, gitWatchCursorsPath())
	})
}

// pollGitWatch polls each root once and advances the persisted cursors. It
// returns the newly-detected commit SHAs keyed by root (the seam the later
// attribution PR consumes); a cold-start root (no prior cursor) is baselined
// WITHOUT reporting, matching git-ai's cold-start discipline.
func pollGitWatch(roots []string) map[string][]string {
	prior := loadGitWatchCursors()
	newHeads := map[string]string{}
	detected := map[string][]string{}

	for _, root := range roots {
		head, ok := gitHead(root)
		if !ok {
			continue // no commits / detached-before-first-commit / not a repo
		}
		key := gitWatchRootKey(root)

		lastSeen, hadCursor := prior[key]
		if !hadCursor || lastSeen == head {
			newHeads[key] = head // cold start (baseline only) or nothing moved
			continue
		}
		commits, ok := gitNewCommits(root, lastSeen, head)
		if !ok {
			continue // comparison inconclusive — keep the old cursor, retry next poll
		}
		if len(commits) == 0 {
			newHeads[key] = head // nothing new (e.g. a backward reset): just move to head
			continue
		}
		detected[key] = commits
		// Advance only to the newest commit we actually returned. commits[0] is
		// newest-first: it equals head on a normal or gc'd-recovery poll, but on a
		// clamped burst it is the newest of the OLDEST batch, so the next poll
		// enumerates commits[0]..head and drains the remainder in order.
		newHeads[key] = commits[0]
	}

	saveGitWatchCursors(newHeads)
	return detected
}

// pollGitWatchWorkspace enumerates the workspace's roots (workspace + its git
// worktrees), polls them, and computes+emits AI attribution for every newly
// detected commit. pollGitWatch returns commits keyed by the opaque root key, so
// we re-derive that key per root to recover the ROOT PATH the attribution engine
// needs to run its one `git show` per commit.
func pollGitWatchWorkspace(session Session) {
	// The workspace roots (workspace + its worktrees) PLUS the repos discovered
	// from the ai-paths ledger. The latter is what makes the autostart daemon work
	// at all: its TaskRoot is the un-pollable HOME dir, so without discovery the
	// root set is just [home] and nothing is ever detected. Deduped so a repo that
	// appears in both sources is polled once.
	roots := dedupRootsByKey(append(
		workspaceMatchRoots(resolvePath(session.TaskRoot)),
		discoverAiRepoRoots(session.TaskRoot)...,
	))
	detected := pollGitWatch(roots)
	nowMs := time.Now().UnixMilli() // one clock read, threaded to both passes

	// Durability advances on the DEFAULT branch only (its own cursor), so it is
	// driven separately from the working-HEAD attribution loop. It MUST run BEFORE
	// attributeCommit: attributeCommit records this cycle's AI fingerprints, and a
	// squash landing on the default branch is BOTH the new working-HEAD commit and
	// the new default-branch commit. Seeding first means the squash is matched only
	// against fingerprints from EARLIER cycles (the real feature-branch lines) —
	// never its own just-recorded ones, which path-level attribution would mark for
	// the whole AI-touched file, wrongly transferring human lines in the squash as AI.
	pollDurability(session, roots, nowMs)

	for _, root := range roots {
		rootKey := gitWatchRootKey(root)
		// Rework scope, resolved ONCE per root (never per commit). Resolved BEFORE the
		// no-new-commits guard so that returning to the default branch clears stale
		// rework tracking even on a poll that surfaces no new commits (e.g. a plain
		// `git checkout main` after a feature branch merged).
		scope := reworkScope(root)
		if scope == scopeDefault {
			// On (or merged back to) the default branch: surviving AI lines are now the
			// durability engine's and reworked ones already emitted, so drop the root's
			// rework tracking before a future branch could remap against stale ranges.
			// Guarded on presence to avoid a needless ledger write on every poll.
			if _, tracked := loadReworkLedger().Roots[rootKey]; tracked {
				clearReworkLedger(rootKey)
			}
		}

		commits := detected[rootKey]
		if len(commits) == 0 {
			continue
		}
		preMerge := scope == scopePreMerge
		state.HookDebugf("git-watch: %d new commit(s) on %s (preMerge=%v)", len(commits), rootKey, preMerge)
		// Oldest-first: rework is STATEFUL (seed then churn across commits), so it
		// must see commits in commit order. Attribution is per-commit independent, so
		// the reversed order is equally correct for it. commits is newest-first.
		for i := len(commits) - 1; i >= 0; i-- {
			attributeAndReworkCommit(session, root, commits[i], preMerge, nowMs)
		}
	}
}

// branchScope classifies a root's current checkout for rework tracking.
type branchScope int

const (
	// scopeUnknown: no resolvable default branch, or a detached/unborn HEAD. Neither
	// seed rework (we cannot tell it is a feature branch) nor clear it (a transient
	// detach mid-rebase must not wipe a real branch's tracking).
	scopeUnknown branchScope = iota
	// scopeDefault: checked out ON the default branch — durability territory. Any
	// rework tracking for this root is stale and gets cleared.
	scopeDefault
	// scopePreMerge: checked out on a NON-default named branch — pre-merge feature
	// work whose AI-line churn is rework.
	scopePreMerge
)

// reworkScope classifies root's checkout by comparing BRANCH NAMES, not tip SHAs.
// A sha comparison (HEAD vs the default tip) misreads a local, not-yet-pushed
// commit on the default branch as pre-merge: the default ref resolves to the
// remote-tracking tip (refs/remotes/origin/HEAD), which lags a local commit, so
// ordinary default-branch work would be wrongly seeded as rework. Comparing the
// checked-out branch name to the default branch name is push-state independent.
// Two constant-time read-only spawns per root per poll (symbolic-ref HEAD + the
// cached default ref).
func reworkScope(root string) branchScope {
	defRef := durabilityDefaultRef(root)
	if defRef == "" {
		return scopeUnknown
	}
	head := gitSymbolicRef(root, "HEAD")
	if head == "" {
		return scopeUnknown // detached or unborn — no branch name to compare
	}
	if shortBranchName(head) == shortBranchName(defRef) {
		return scopeDefault
	}
	return scopePreMerge
}

// shortBranchName reduces a full ref name to its branch short name:
// refs/heads/feat/x → feat/x, refs/remotes/origin/main → main. A name matching
// neither prefix is returned unchanged.
func shortBranchName(ref string) string {
	if s, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return s
	}
	if s, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
		if i := strings.IndexByte(s, '/'); i >= 0 {
			return s[i+1:] // drop the remote name (first path component)
		}
		return s
	}
	return ref
}

// runGitWatch baselines immediately, then re-polls every gitWatchInterval until
// stop is closed. Mirrors runConfigCensus's stop-channel loop.
func runGitWatch(session Session, stop <-chan struct{}) {
	pollGitWatchWorkspace(session)
	ticker := time.NewTicker(gitWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			pollGitWatchWorkspace(session)
		}
	}
}

// StartGitWatch launches the watcher goroutine for a session and returns a stop
// func the caller defers. Mirrors StartConfigCensus / StartPresenceHeartbeat.
func StartGitWatch(session Session) (stop func()) {
	done := make(chan struct{})
	go runGitWatch(session, done)
	return func() { close(done) }
}

// RunGitWatcher is the foreground `git-watch` subcommand: resolve the session
// and poll until the process is interrupted.
func RunGitWatcher() error {
	session, err := loadSession()
	if err != nil {
		return fmt.Errorf("no active session: %w", err)
	}
	if session.TaskRoot == "" {
		return fmt.Errorf("session has no task root")
	}
	runGitWatch(session, make(chan struct{}))
	return nil
}
