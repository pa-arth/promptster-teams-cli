package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// first, but ONLY on a fast-forward — lastSeen must be an ancestor of head. The
// bool distinguishes a definitive answer from an inconclusive one:
//
//   - Fast-forward (lastSeen is an ancestor of head): the new commits, ok=true.
//     An empty slice means genuinely nothing new.
//   - Divergence (lastSeen is valid but NOT an ancestor of head — a branch
//     switch, rebase, or reset): no commits, ok=true. rev-list lastSeen..head
//     would otherwise surface the new tip's PRE-EXISTING history as if freshly
//     created, so the caller re-baselines instead of replaying it.
//   - Failure (lastSeen unresolvable — pruned object, corrupt cursor — or git
//     errored): ok=false, so the caller KEEPS the old cursor and retries next
//     poll rather than advancing past an undetermined window forever.
func gitNewCommits(root, lastSeen, head string) (commits []string, ok bool) {
	// #nosec G204 -- constant argv; root is a discovered workspace/worktree dir and both SHAs come from git rev-parse output, not user input. Read-only.
	switch err := exec.Command("git", "-C", root, "merge-base", "--is-ancestor", lastSeen, head).Run().(type) {
	case nil:
		// Fast-forward — fall through to rev-list.
	case *exec.ExitError:
		if err.ExitCode() == 1 {
			return nil, true // valid but not an ancestor → divergence: re-baseline
		}
		return nil, false // bad object / other git error → keep cursor, retry
	default:
		return nil, false // spawn failure → keep cursor, retry
	}
	// #nosec G204 -- see above; read-only.
	out, err := exec.Command("git", "-C", root, "rev-list", lastSeen+".."+head).Output()
	if err != nil {
		return nil, false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			commits = append(commits, line)
		}
	}
	return commits, true
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
		if len(commits) > 0 {
			detected[key] = commits
		}
		newHeads[key] = head // advance only after a definitive comparison
	}

	saveGitWatchCursors(newHeads)
	return detected
}

// pollGitWatchWorkspace enumerates the workspace's roots (workspace + its git
// worktrees) and polls them, logging what it detected. Nothing is emitted.
func pollGitWatchWorkspace(workspace string) {
	detected := pollGitWatch(workspaceMatchRoots(workspace))
	for key, commits := range detected {
		state.HookDebugf("git-watch: %d new commit(s) on %s", len(commits), key)
	}
}

// runGitWatch baselines immediately, then re-polls every gitWatchInterval until
// stop is closed. Mirrors runConfigCensus's stop-channel loop.
func runGitWatch(workspace string, stop <-chan struct{}) {
	pollGitWatchWorkspace(workspace)
	ticker := time.NewTicker(gitWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			pollGitWatchWorkspace(workspace)
		}
	}
}

// StartGitWatch launches the watcher goroutine for a workspace and returns a
// stop func the caller defers. Mirrors StartConfigCensus / StartPresenceHeartbeat.
func StartGitWatch(workspace string) (stop func()) {
	done := make(chan struct{})
	go runGitWatch(workspace, done)
	return func() { close(done) }
}

// RunGitWatcher is the foreground `git-watch` subcommand: resolve the session's
// workspace and poll until the process is interrupted.
func RunGitWatcher() error {
	session, err := loadSession()
	if err != nil {
		return fmt.Errorf("no active session: %w", err)
	}
	if session.TaskRoot == "" {
		return fmt.Errorf("session has no task root")
	}
	runGitWatch(resolvePath(session.TaskRoot), make(chan struct{}))
	return nil
}
