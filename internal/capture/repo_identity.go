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
//  1. git repo with an origin remote → owner/name slug (GitHub-joinable). TRACKED.
//  2. git repo, no remote → sha256 of the repo common-dir (collapses worktrees). TRACKED.
//  3. non-git dir → sha256 of the abspath. NOT tracked — see sessionRepoIdentity.
//
// Cases 2 and 3 produce a structurally identical opaque key, which is why the
// tracked bit exists at all: it is the only thing that tells a real remoteless
// repo (keep its board row) from a home/container directory (no row).
//
// Returns "" (so the prompt omits repoRoot rather than guessing) when the cwd is
// empty or no longer on disk — a resumed/backfilled transcript — mirroring
// state.HomeRelativeStrict's empty-on-failure treatment of workdir.
//
// NOTE (accepted limitation, deferred to the repoident/census Part-B extraction):
// case 2's collapse relies on workspaceKey's common-dir handling; this function
// does not add its own worktree reconciliation.
func sessionRepoRoot(cwd string) string {
	root, _, _ := sessionRepoIdentity(cwd)
	return root
}

// sessionRepoIdentity resolves all three parts of the session's repo identity in
// one pass: the canonical `repoRoot` (as documented on sessionRepoRoot above),
// the remote's host, and whether the resolved directory was inside a git working
// tree at all.
//
// The host exists because the slug alone cannot tell providers apart — a remote
// on gitlab.com and one on github.com both reduce to "acme/api". The backend
// uses the slug to decide whether a repo belongs to the company's connected
// GitHub org; without a host it would have to treat a colliding owner name as a
// match. Emitting the host lets it require a real provider match and abstain
// when it has none, which is the difference between "we can't tell" and a wrong
// answer about where somebody's work went.
//
// host is non-empty ONLY when repoRoot is a remote slug. The two opaque-hash
// cases have no remote by definition, so there is nothing to report and "" is
// the honest value — never a guess, and never a leftover from another repo.
//
// tracked reports whether the cwd had a `.git` ancestor — i.e. whether ladder
// cases 1-2 (a real repository) or case 3 (a home directory, a container folder,
// a scratch dir) produced the key. It is gitRootOf's `ok`, which this function
// used to discard: without it, "git repo with no origin remote" and "not a repo
// at all" emit a structurally identical 16-hex key and nothing downstream can
// tell a repo that deserves a Top-Repos row from a directory that does not.
//
// tracked is meaningful ONLY when root is non-empty. When the cwd is empty or
// gone we did not look, so the caller MUST omit the field entirely rather than
// stamp `false` — an absent value means "this CLI did not look" and is treated
// as tracked downstream, while an explicit `false` is a positive claim that the
// directory is not a working tree. Returning false here is the zero value of an
// unasked question, not an answer.
//
// tracked is a STAT, and deliberately not a validation. gitRootOf only checks
// that a `.git` entry exists, so a stale or malformed marker — a worktree whose
// admin dir was deleted by hand, say — reports tracked. Confirming it with a git
// spawn was considered and rejected: it trades a rare, harmless false positive
// for a plausible, damaging false negative. A false `true` leaves the directory
// exactly where it sits today, a hash row on the board. A false `false` deletes
// a real repo's row and folds its sessions into the untracked aggregate — and
// every git call here is already timeout-bounded precisely because a network
// mount or a corrupt .git can hang (see gitRemote), so gating `tracked` on one
// would mean an NFS hiccup silently un-repos a live company checkout. A stat
// cannot time out. The conservative direction is the one that keeps the row.
//
// One `git config` spawn, not two: the remote is resolved directly here and the
// opaque fallback reached via workspaceHashKey, rather than round-tripping
// through workspaceKey (which would re-run the same lookup).
func sessionRepoIdentity(cwd string) (root, host string, tracked bool) {
	if cwd == "" {
		return "", "", false
	}
	dir := resolvePath(cwd)
	if _, err := os.Stat(dir); err != nil {
		// cwd no longer on disk — omit rather than hash a stale path.
		return "", "", false
	}
	r, ok := gitRootOf(dir)
	if ok {
		dir = r
	}
	if h, slug := gitRemote(dir); slug != "" {
		return slug, h, ok
	}
	return workspaceHashKey(dir), "", ok
}
