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
	roots := workspaceMatchRoots(resolvePath(session.TaskRoot))
	detected := pollGitWatch(roots)
	nowMs := time.Now().UnixMilli() // one clock read, threaded to both passes
	for _, root := range roots {
		commits := detected[gitWatchRootKey(root)]
		if len(commits) == 0 {
			continue
		}
		state.HookDebugf("git-watch: %d new commit(s) on %s", len(commits), gitWatchRootKey(root))
		for _, sha := range commits {
			attributeCommit(session, root, sha, nowMs)
		}
	}
	// Durability advances on the DEFAULT branch only (its own cursor), so it is
	// driven separately from the working-HEAD attribution loop above. Same roots,
	// same cadence.
	pollDurability(session, roots, nowMs)
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
