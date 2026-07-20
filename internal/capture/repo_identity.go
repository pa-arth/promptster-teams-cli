package capture

import "os"

// sessionRepoRoot resolves the canonical per-session repository identity from the
// session's cwd — the value the CLI stamps as `repoRoot` on prompt events so the
// backend can de-fragment a repo across subdirs/worktrees and join it exactly to
// GitHub PRs (outcome_events.repo). It is resolved ONCE per transcript/session in
// this package (never per prompt line) and threaded into the normalize processors
// as session state, because the resolution needs `git config` (fs/exec) that the
// pure internal/normalize package deliberately avoids.
//
// It composes the existing package-private helpers rather than adding a second
// identity scheme (the exact "two competing identities" trap this change kills):
//   - gitRootOf(cwd): a stat-only walk up to the nearest .git ancestor (no git
//     spawn), so a subdir or a linked worktree both canonicalize to a repo root.
//   - workspaceKey(root): slug-preferred (git remote owner/name) with an opaque
//     sha256 fallback — the SAME identity config_census emits, so a session and
//     its device census share one key.
//
// The fallback ladder (via workspaceKey):
//  1. git repo with an origin remote → owner/name slug (GitHub-joinable).
//  2. git repo, no remote → sha256 of the repo common-dir (collapses worktrees).
//  3. non-git dir → sha256 of the abspath.
//
// Returns "" (so the prompt omits repoRoot rather than guessing) when the cwd is
// empty or no longer on disk — a resumed/backfilled transcript — mirroring
// state.HomeRelativeStrict's empty-on-failure treatment of workdir.
//
// NOTE (accepted limitation, deferred to the repoident/census Part-B extraction):
// case 2's collapse relies on workspaceKey's common-dir handling; this function
// does not add its own worktree reconciliation.
func sessionRepoRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	root := resolvePath(cwd)
	if _, err := os.Stat(root); err != nil {
		// cwd no longer on disk — omit rather than hash a stale path.
		return ""
	}
	if r, ok := gitRootOf(root); ok {
		root = r
	}
	return workspaceKey(root)
}
