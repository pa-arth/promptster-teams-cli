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
// building hundreds of commits.
//
// ⚠️This cap is PER ROOT and a poll walks EVERY discovered root, so on its own it
// does not bound a poll's TOTAL emission: 63 discovered roots × 100 = 6,300, and
// teams prod recorded a single burst of 7,482 commit_attribution POSTs 0.1s
// apart. gitWatchMaxCommitsPerPollTotal below is the shared budget that bounds
// the poll-wide total; this per-root cap still limits any ONE root's `git show`
// fan-out and is the gc'd-cursor recovery-window size (see gitNewCommits).
var gitWatchMaxCommitsPerPoll = 100

// gitWatchMaxCommitsPerPollTotal bounds how many commits a SINGLE poll surfaces
// across ALL roots combined — the global companion to the per-root cap above
// (§0.2c). pollGitWatch spends this budget root-by-root and takes only the OLDEST
// commits that fit: a root whose batch overflows the remaining budget has its
// cursor advanced only to the newest commit actually taken, and a root reached
// after the budget is spent is left with its cursor untouched. Both drain on
// subsequent polls, in commit order — deferred, never dropped. This is what makes
// a genuine large burst (a first import, a multi-root gc storm) land as a bounded
// trickle instead of a 7,482-POST thundering herd against the ingest endpoint.
//
// 200 = 2× the per-root cap: ordinary multi-repo activity (a handful of commits
// across a few repos per 60s) never touches it, while a pathological burst drains
// at 200/poll. A package var, not a const, so a test can lower it. Deferring by
// NOT advancing the cursor (rather than refusing to emit downstream, which WOULD
// drop — the cursor would already be past the un-emitted commits) is the whole
// point; keep the budget decision co-located with the cursor advance in
// pollGitWatch. NB: an already-attributed commit re-surfaced by the gc'd-recovery
// path is skipped cheaply by the attributed-commits ledger in
// pollGitWatchWorkspace but still counts against this budget here (pollGitWatch
// cannot see the ledger without a cost this loop is built to avoid); the effect
// is a transient extra poll or two to clear a mass re-emission, never data loss.
var gitWatchMaxCommitsPerPollTotal = 200

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
	aiKey   string // the root key the ai-paths / bash-windows ledgers are stored under
	prefix  string // POSIX rel(taskRoot, root) when root is UNDER taskRoot ("" == taskRoot)
	absRoot string // resolved root, set when root is OUTSIDE taskRoot (evidence stored absolute)
}

// resolveLedgerScope computes the scope for reconciling `root`'s committed paths
// against the ledgers, which capture ALWAYS anchors to the workspace
// (session.TaskRoot) — every event is stamped with gitWatchRootKey(taskRoot) —
// never to each polled repo. So the key is always the workspace key; only the
// PATH form differs, matching exactly what RelativizeEventPaths stored:
//
//   - root UNDER taskRoot (the common daemon case: TaskRoot=home, repo under it):
//     the path was rewritten workspace-relative, so look it up with the
//     rel(taskRoot, root) prefix. rel is taken in resolvePath-canonical space so a
//     symlinked home (macOS /var vs /private/var) cancels on both sides. When
//     root == taskRoot the prefix is "" — byte-for-byte the pre-discovery behavior.
//   - root OUTSIDE taskRoot (a discovered repo/worktree not under home — rare):
//     RelativizeEventPaths leaves an out-of-workspace path UNREWRITTEN (absolute),
//     so match by the absolute path under the same workspace key. Reading under
//     the repo key here would miss the evidence entirely (it was never stored
//     there) and silently attribute unknown.
//
// taskRoot == "" (no workspace, e.g. a malformed session) falls back to the
// per-root key.
func resolveLedgerScope(root, taskRoot string) ledgerScope {
	if taskRoot == "" {
		return ledgerScope{aiKey: gitWatchRootKey(root)}
	}
	rRoot := resolvePath(root)
	rel, err := filepath.Rel(resolvePath(taskRoot), rRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ledgerScope{aiKey: gitWatchRootKey(taskRoot), absRoot: rRoot} // outside workspace
	}
	if rel == "." {
		rel = ""
	}
	return ledgerScope{aiKey: gitWatchRootKey(taskRoot), prefix: filepath.ToSlash(rel)}
}

// ledgerPath translates a repo-relative committed path into the key the ai-paths
// ledger stored it under: workspace-relative when the repo is under the workspace,
// absolute when it is outside (see resolveLedgerScope).
func (s ledgerScope) ledgerPath(committedRel string) string {
	if s.absRoot != "" {
		return filepath.ToSlash(filepath.Join(s.absRoot, committedRel))
	}
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

// discoveredReposPath persists the repos git-watch has discovered so polling
// SURVIVES the ai-paths ledger's 7-day expiry. Discovery from ai-paths alone
// would stop polling a repo the moment its AI edits age out — but a durability
// span matures at 30 days and is harvested only while its repo is still polled,
// so a repo left AI-idle for a week would silently drop its pending durability /
// rework verdicts. This file re-anchors polling to a durability-scale horizon.
func discoveredReposPath() string {
	return filepath.Join(state.StateDir(), "git-watch-repos.json")
}

// discoveredRepoTTL keeps a repo polled long enough after its last AI touch (or
// last commit) to still observe a durability span maturing there even if the
// engineer stops using AI in it. Comfortably above durabilityWindowMs (30d) so a
// verdict is never missed to discovery expiry; the set stays bounded by the
// number of repos the engineer actually works in regardless.
var discoveredRepoTTLMs = durabilityWindowMs + 15*24*60*60*1000 // 30d + 15d slack

const discoveredReposVersion = 1

// discoveredRepos maps the opaque root key to the repo's on-disk path (a
// LOCAL-only state file, never transmitted — the daemon plainly knows its own
// repo paths) and the last time it had a reason to stay polled.
type discoveredRepos struct {
	V     int                          `json:"v"`
	Repos map[string]discoveredRepoRow `json:"repos"`
}

type discoveredRepoRow struct {
	Path string `json:"path"`
	TsMs int64  `json:"tsMs"`
}

// loadDiscoveredRepos returns the paths of every persisted repo still within the
// TTL. Expired rows are ignored (and compacted on the next refresh write).
func loadDiscoveredRepos(nowMs int64) []string {
	var paths []string
	_ = sign.WithBufferLock(discoveredReposPath()+".lock", func() error {
		data, err := os.ReadFile(discoveredReposPath())
		if err != nil {
			return nil
		}
		var onDisk discoveredRepos
		if json.Unmarshal(data, &onDisk) != nil || onDisk.V != discoveredReposVersion {
			return nil
		}
		for _, row := range onDisk.Repos {
			if row.Path != "" && nowMs-row.TsMs <= discoveredRepoTTLMs {
				paths = append(paths, row.Path)
			}
		}
		return nil
	})
	return paths
}

// refreshDiscoveredRepos stamps nowMs onto every root that has a REASON to keep
// being polled — fresh AI activity or a commit detected this poll — and prunes
// rows past the TTL. A root that is merely idle (no AI, no commits) is NOT
// refreshed, so it ages out after the durability horizon rather than being polled
// forever. Best-effort: I/O failure never blocks a poll.
func refreshDiscoveredRepos(roots []string, nowMs int64) {
	if len(roots) == 0 {
		return
	}
	_ = sign.WithBufferLock(discoveredReposPath()+".lock", func() error {
		merged := discoveredRepos{V: discoveredReposVersion, Repos: map[string]discoveredRepoRow{}}
		if data, err := os.ReadFile(discoveredReposPath()); err == nil {
			var onDisk discoveredRepos
			if json.Unmarshal(data, &onDisk) == nil && onDisk.Repos != nil {
				merged.Repos = onDisk.Repos
			}
		}
		for _, root := range roots {
			merged.Repos[gitWatchRootKey(root)] = discoveredRepoRow{Path: root, TsMs: nowMs}
		}
		for key, row := range merged.Repos {
			if nowMs-row.TsMs > discoveredRepoTTLMs {
				delete(merged.Repos, key)
			}
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		tmp := discoveredReposPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, discoveredReposPath())
	})
}

// concatRoots flattens root slices into one, allocating fresh so no input slice
// is aliased/mutated by a later append (the classic append-to-shared-backing bug).
func concatRoots(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
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

// gitBranchCommitsSinceDefault lists the commits a root's checked-out branch
// holds that the default branch does not — the branch's own pre-merge range,
// newest-first. It is the replay range for a cold-started root (see
// replayReworkForColdStartBranch); nothing else calls it, and it never advances
// a cursor.
//
// head is the SHA pollGitWatch BASELINED THE CURSOR TO, and the range is built
// against that SHA rather than against a second read of `HEAD`. The two reads are
// separated by a whole poll's worth of git spawns (pollDurability over every
// root, then loadAttributedCommits), so a commit landing in that window would be
// folded here while the cursor still sits behind it — and the next poll, seeing
// it as new, would fold its hunks a SECOND time and slide every recovered span
// by an offset already applied. Resolving a moving reference twice is the defect;
// there is nothing to re-check.
//
// --first-parent and --topo-order are what make the returned list foldable, and
// neither is cosmetic:
//
//   - --first-parent, because gitCommitRawDiff reads every commit with
//     `-m --first-parent`. A merge's diff therefore ALREADY carries everything
//     the second-parent side brought in, so folding that side's own commits too
//     applies the same hunks twice, and applies them in the merged-away
//     coordinate space of a sibling branch. The first-parent chain is the one
//     sequence of diffs that composes exactly to head's tree.
//   - --topo-order, because rev-list's default is reverse COMMIT-DATE order,
//     which git does not promise is topological. A commit whose committer clock
//     runs ahead sorts past its own descendants, and reversed that folds a parent
//     after its child — spans addressed in a line space no checkout ever had.
//     TestReworkColdStartSkewedClockFoldsTheBranchInHeadsLineSpace builds that
//     shape from real committer dates.
//
// It returns NOTHING when that range exceeds gitWatchMaxCommitsPerPoll, and the
// refusal — rather than a clamp — is the point.
//
// The range is replayed ONCE, from the empty ledger adoption just left, because
// a cold-start cursor is already at head and no later poll re-surfaces any of
// it. So a partial replay is not "the remainder drains next poll", which is what
// clampCommitBurst's clamp means; it is a replay that STARTS MID-BRANCH, from a
// state live tracking would never have been in. Both ends go wrong there, and
// both go wrong toward FABRICATION rather than loss:
//
//   - keeping the OLDEST slice (clampCommitBurst's direction) leaves the rebuilt
//     spans positioned as of the middle of the branch while the working tree
//     stands at its head, so the next commit to rewrite those coordinates emits a
//     verdict about lines nobody wrote — the shape
//     TestReworkAdoptionClampedBurstDoesNotStrandSpansMidBranch pins;
//   - keeping the NEWEST slice ends at head, but makes the first replayed commit
//     look like a FIRST TOUCH of every path it edits. The path-presence inference
//     that is correct at a branch's real base then seeds that commit's added
//     lines on an AI-touched path as AI — including a human's. Measured, not
//     reasoned: a two-commit branch replayed under a cap of one seeded the ten
//     lines a human prepended.
//
// So a branch more than gitWatchMaxCommitsPerPoll commits past its default
// branch keeps the pre-fix behaviour and that copy under-reports it — the
// direction these ledgers always resolve toward, and the reason this returns a
// whole range or none of it.
//
// One read-only spawn (the default ref itself is cached).
func gitBranchCommitsSinceDefault(root, head string) []string {
	if head == "" {
		return nil
	}
	defRef := durabilityDefaultRef(root)
	if defRef == "" {
		return nil // no resolvable default branch — no pre-merge range to speak of
	}
	// #nosec G204 -- constant argv; root is a discovered workspace/worktree dir, defRef is a ref name resolved by git itself and head came from git rev-parse. Read-only.
	out, err := exec.Command("git", "-C", root, "rev-list",
		"--topo-order", "--first-parent", defRef+".."+head).Output()
	if err != nil {
		return nil
	}
	shas := parseRevListShas(out)
	if len(shas) > gitWatchMaxCommitsPerPoll {
		state.HookDebugf("git-watch: cold-start branch on %s holds %d commit(s) past the default branch, over the per-root cap %d; replaying none of it rather than starting mid-branch",
			gitWatchRootKey(root), len(shas), gitWatchMaxCommitsPerPoll)
		return nil
	}
	return shas
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
//
// The second return says, per root, whether this poll left that root's cursor AT
// ITS HEAD — i.e. whether the root DRAINED. A root is not drained when the
// shared budget deferred it whole, when a burst was clamped to a partial batch,
// or when the comparison was inconclusive; in each of those cases commits this
// poll already knows about are still owed to a later poll. Only the caller can
// act on that, and it must: state it drops on the strength of "this root's
// commits are being processed now" (branch adoption) stays owed for exactly as
// long as this flag is false.
//
// The third return names the roots that COLD-STARTED here — baselined with no
// prior cursor at all — and maps each to THE EXACT SHA ITS CURSOR WAS BASELINED
// TO. It is reported rather than acted on, because the cursor discipline must not
// change: a cold-start root still reports nothing and still emits nothing. What
// it buys the caller is the ability to tell "this root has never been seen" apart
// from "this root has nothing new", which are identical from here (both baseline
// to head and drain) and mean opposite things to branch adoption — see
// replayReworkForColdStartBranch.
//
// The SHA, not just the fact, is returned because the caller runs much later in
// the same poll and must resolve its replay range against the same commit this
// cursor now claims. Re-reading `HEAD` there would let a commit made in between
// be folded by the replay AND detected as new by the next poll, which folds its
// hunks twice.
func pollGitWatch(roots []string) (map[string][]string, map[string]bool, map[string]string) {
	prior := loadGitWatchCursors()
	newHeads := map[string]string{}
	detected := map[string][]string{}
	drained := map[string]bool{}
	coldStart := map[string]string{}

	// Global per-poll budget shared across ALL roots (§0.2c). gitNewCommits already
	// clamps each root to gitWatchMaxCommitsPerPoll, but a poll walks every root, so
	// without a shared cap N roots surface up to N×cap in one burst. We spend the
	// budget root-by-root and take only the oldest commits that fit; the remainder
	// (a partial root's newer tail, or a whole root reached after the budget is
	// spent) keeps its old cursor and drains on the next poll — deferred, not lost.
	budget := gitWatchMaxCommitsPerPollTotal
	deferred := 0

	for _, root := range roots {
		head, ok := gitHead(root)
		if !ok {
			continue // no commits / detached-before-first-commit / not a repo
		}
		key := gitWatchRootKey(root)

		lastSeen, hadCursor := prior[key]
		if !hadCursor {
			// Cold start: baseline WITHOUT reporting, unchanged. Only the flag is new,
			// and it is the whole reason this case is no longer folded in with
			// "nothing moved" below — the two are indistinguishable from here and a
			// root that has never been seen is precisely the one whose branch may hold
			// AI history this device recorded through another copy of the repo.
			newHeads[key] = head
			drained[key] = true
			coldStart[key] = head
			continue
		}
		if lastSeen == head {
			newHeads[key] = head // nothing moved
			drained[key] = true
			continue
		}
		commits, ok := gitNewCommits(root, lastSeen, head)
		if !ok {
			continue // comparison inconclusive — keep the old cursor, retry next poll
		}
		if len(commits) == 0 {
			newHeads[key] = head // nothing new (e.g. a backward reset): just move to head
			drained[key] = true
			continue
		}
		if budget <= 0 {
			// Poll-wide budget already spent: defer this whole root untouched (cursor
			// stays at lastSeen so every commit re-surfaces next poll). NOT a drop.
			deferred += len(commits)
			continue
		}
		if len(commits) > budget {
			// This root overflows the remaining budget. commits is newest-first and we
			// drain oldest-first, so keep the OLDEST `budget` (the tail) and defer the
			// newer remainder — the same shape as clampCommitBurst's per-root drain,
			// just bounded by the shared budget instead of the per-root cap.
			deferred += len(commits) - budget
			commits = commits[len(commits)-budget:]
		}
		detected[key] = commits
		// Advance only to the newest commit we actually returned. commits[0] is
		// newest-first: it equals head on a normal or gc'd-recovery poll that fit the
		// budget, but on a clamped burst (per-root OR global) it is the newest of the
		// OLDEST batch, so the next poll enumerates commits[0]..head and drains the
		// remainder in order.
		newHeads[key] = commits[0]
		drained[key] = commits[0] == head
		budget -= len(commits)
	}

	if deferred > 0 {
		state.HookDebugf("git-watch: per-poll global cap %d reached; deferred %d commit(s) to subsequent polls",
			gitWatchMaxCommitsPerPollTotal, deferred)
	}

	saveGitWatchCursors(newHeads)
	return detected, drained, coldStart
}

// pollGitWatchWorkspace enumerates the workspace's roots (workspace + its git
// worktrees), polls them, and computes+emits AI attribution for every newly
// detected commit. pollGitWatch returns commits keyed by the opaque root key, so
// we re-derive that key per root to recover the ROOT PATH the attribution engine
// needs to run its one `git show` per commit.
func pollGitWatchWorkspace(session Session) {
	nowMs := time.Now().UnixMilli() // one clock read, threaded to every pass

	// The poll set is three sources unioned and deduped:
	//   1. workspace roots (workspace + its worktrees) — the explicit case;
	//   2. repos discovered from the ai-paths ledger — what makes the autostart
	//      daemon work at all (its TaskRoot is the un-pollable HOME dir, so without
	//      discovery the set is just [home] and nothing is ever detected);
	//   3. repos persisted from earlier polls — so a repo keeps being polled across
	//      the 30-day durability horizon even after its AI edits age out of the
	//      7-day ai-paths ledger (else its maturing verdicts would be silently lost).
	aiRoots := discoverAiRepoRoots(session.TaskRoot)
	roots := dedupRootsByKey(concatRoots(
		workspaceMatchRoots(resolvePath(session.TaskRoot)),
		aiRoots,
		loadDiscoveredRepos(nowMs),
	))
	detected, drained, coldStart := pollGitWatch(roots)

	// Persist the repos with a REASON to stay polled: fresh AI activity or a commit
	// detected this poll. A merely-idle repo is left to age out after the horizon.
	keep := append([]string{}, aiRoots...)
	for _, root := range roots {
		if len(detected[gitWatchRootKey(root)]) > 0 {
			keep = append(keep, root)
		}
	}
	refreshDiscoveredRepos(keep, nowMs)

	// Durability advances on the DEFAULT branch only (its own cursor), so it is
	// driven separately from the working-HEAD attribution loop. It MUST run BEFORE
	// attributeCommit: attributeCommit records this cycle's AI fingerprints, and a
	// squash landing on the default branch is BOTH the new working-HEAD commit and
	// the new default-branch commit. Seeding first means the squash is matched only
	// against fingerprints from EARLIER cycles (the real feature-branch lines) —
	// never its own just-recorded ones, which path-level attribution would mark for
	// the whole AI-touched file, wrongly transferring human lines in the squash as AI.
	pollDurability(session, roots, nowMs)

	// Loaded ONCE per poll, not per root: the ledger is keyed by SHA alone, so a
	// commit reachable from two roots (a repo and its worktree) is attributed once.
	attributed := loadAttributedCommits(nowMs)
	var emitted []string
	reattempted := 0

	for _, root := range roots {
		rootKey := gitWatchRootKey(root)
		// Rework scope, resolved ONCE per root (never per commit). Resolved BEFORE the
		// no-new-commits guard so that returning to the default branch clears stale
		// rework tracking even on a poll that surfaces no new commits (e.g. a plain
		// `git checkout main` after a feature branch merged).
		scope, branch := reworkScope(root)
		// Set while this root still OWES the replay that binding it to a new branch
		// made necessary. Adoption empties the root's rework state, and the skip
		// below then drops the very commits that would rebuild it, so the adopted
		// branch's AI spans are lost — see replayReworkForAdoptedCommit.
		//
		// It is read from the LEDGER, not from "did this poll adopt", because the
		// commit range the replay covers is not bounded by one poll: a root the
		// shared budget deferred whole surfaces no commits at all on the adopting
		// poll, and a burst clamped to the per-root cap leaves its newer tail for
		// later polls. A per-poll flag left both ranges skipped with nothing to
		// replay them — the first silently, the second while holding spans
		// positioned at the middle of the branch, which the next live commit
		// remaps into lines the AI never wrote. The marker is therefore cleared
		// only once the root has actually drained (drained[rootKey]), and dropping
		// it is what restores the steady-state guard: gitNewCommits' cursor
		// recovery re-surfaces skipped commits with no branch change, and folding
		// one of those into live state re-applies its hunks.
		adopting := false
		// Set only where adoptReworkBranch actually FIRED this poll — i.e. where the
		// root's rework state was emptied a few lines above. `adopting` cannot stand
		// in for it: it is read from the ledger on purpose, so it stays true across
		// later polls that adopt nothing.
		justAdopted := false
		switch scope {
		case scopeDefault:
			// On (or merged back to) the default branch: surviving AI lines are now the
			// durability engine's and reworked ones already emitted, so drop the root's
			// rework tracking before a future branch could remap against stale ranges.
			// Guarded on presence to avoid a needless ledger write on every poll. The
			// guard must consider the seed tombstones and the recorded branch too: both
			// outlive the tracked map, so a Roots-only check would strand them.
			if reworkLedgerHasRoot(rootKey) {
				clearReworkLedger(rootKey)
			}
		case scopePreMerge:
			// Rework state belongs to ONE branch. Binding it to the checked-out branch
			// here is what expires it: `git switch -c next-thing` straight off a feature
			// branch, and a per-branch worktree that never visits the default branch,
			// both skip the scopeDefault clear above entirely, and the previous branch's
			// ranges and tombstones would otherwise carry over silently.
			recorded, pending := reworkLedgerBranchState(rootKey)
			if branch != "" && recorded != branch {
				adoptReworkBranch(rootKey, branch)
				pending = true
				justAdopted = true
			}
			adopting = pending
		}

		// A root that has never been polled before — the ordinary `git worktree add`
		// — is a NEW rootKey, so pollGitWatch cold-started it: the cursor went
		// straight to head and none of the branch's existing commits were surfaced.
		// The adoption above still fires (a new key has no recorded branch), so
		// without this the root declares its replay owed and finishes it below having
		// replayed nothing, leaving that copy of the branch with no AI spans at all.
		// The replay is one-shot by construction and takes its own range from git,
		// because there is no cursor for a later poll to drain against.
		//
		// THE SAME COMMIT'S HUNKS ARE NEVER FOLDED TWICE INTO THE SAME LEDGER, which
		// is why the gate is justAdopted and not `adopting`. `adopting` is an
		// obligation read from the ledger and deliberately outlives the poll that
		// created it, so a root whose CURSOR is lost while its LEDGER survives —
		// an unreadable or version-bumped cursors file, or a best-effort save that
		// never landed — cold-starts on a branch it has already recorded, leaves the
		// tracked spans in place, and would replay the whole range over them.
		// justAdopted is true only where adoptReworkBranch just emptied this root's
		// state, so the range is always folded into the empty ledger it assumes. A
		// genuinely new root always takes that path: its recorded branch is "".
		if justAdopted && coldStart[rootKey] != "" {
			replayReworkForColdStartBranch(session, root, coldStart[rootKey], attributed, nowMs)
		}

		commits := detected[rootKey]
		if len(commits) == 0 {
			// A root with no commits this poll either has nothing left to replay
			// (drained) or was deferred with its whole range still owed. Only the
			// first ends the adoption.
			if adopting && drained[rootKey] {
				finishReworkAdoption(rootKey)
			}
			continue
		}
		preMerge := scope == scopePreMerge
		state.HookDebugf("git-watch: %d new commit(s) on %s (preMerge=%v)", len(commits), rootKey, preMerge)
		// Oldest-first: rework is STATEFUL (seed then churn across commits), so it
		// must see commits in commit order. Attribution is per-commit independent, so
		// the reversed order is equally correct for it. commits is newest-first.
		//
		// A SHA this device already attributed is skipped outright. "Detected" only
		// means HEAD moved relative to a cursor, which is NOT the same as "not yet
		// attributed": the recovery path in gitNewCommits re-surfaces the newest
		// commits wholesale whenever a cursor becomes unreachable, and roots are
		// keyed by path so worktree churn re-detects too. Skipping covers rework as
		// well as attribution — re-running rework over a commit already accounted for
		// would double-count its churn.
		for i := len(commits) - 1; i >= 0; i-- {
			sha := commits[i]
			if _, done := attributed[sha]; done {
				reattempted++
				// Attribution stays skipped — this commit really was accounted for,
				// and nothing here emits. But on a poll that just adopted the branch,
				// this SHA is also the only thing that can rebuild the rework state
				// adoption emptied, and skipping it outright is what left an adopted
				// branch looking like it held no AI work.
				if preMerge && adopting {
					replayReworkForAdoptedCommit(session, root, sha, nowMs)
				}
				continue
			}
			if !attributeAndReworkCommit(session, root, sha, preMerge, nowMs) {
				// Enqueue failed — leave the SHA out of the ledger so the next poll
				// retries it rather than suppressing it for the ledger's whole TTL.
				continue
			}
			// Mark it in the IN-MEMORY set too, not just the batch written at the end
			// of the poll: the same commit is reachable from a repo AND from each of
			// its worktrees, which are separate roots later in THIS loop. Without
			// this, one poll emits it once per root.
			attributed[sha] = struct{}{}
			emitted = append(emitted, sha)
		}
		// The replay is owed until the root reaches its head. A clamped burst
		// processed only the oldest slice of the range, so the branch's remaining
		// commits — every one of them already attributed — still have to be
		// replayed on later polls.
		if adopting && drained[rootKey] {
			finishReworkAdoption(rootKey)
		}
	}

	if reattempted > 0 {
		state.HookDebugf("git-watch: skipped %d already-attributed commit(s) this poll", reattempted)
	}
	recordAttributedCommits(emitted, nowMs)
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
//
// It also RETURNS the checked-out branch name, which the rework ledger records
// as the identity of the state it holds. Returning it from here rather than
// re-resolving it at the call site keeps the spawn budget where the comment above
// says it is — the name has already been read to classify the scope.
func reworkScope(root string) (branchScope, string) {
	defRef := durabilityDefaultRef(root)
	if defRef == "" {
		return scopeUnknown, ""
	}
	head := gitSymbolicRef(root, "HEAD")
	if head == "" {
		return scopeUnknown, "" // detached or unborn — no branch name to compare
	}
	if shortBranchName(head) == shortBranchName(defRef) {
		return scopeDefault, shortBranchName(head)
	}
	return scopePreMerge, shortBranchName(head)
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
