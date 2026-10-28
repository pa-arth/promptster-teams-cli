package capture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Off-HEAD commit detection.
//
// The HEAD pass (pollGitWatch) only ever sees `cursor..HEAD` of each polled
// checkout. An agent that branches, commits, pushes and then switches back or
// deletes its worktree inside one 60s poll leaves commits that never sat at a
// polled HEAD, so they were never emitted. Prod 2026-10-21 (ops.ai, 30d): of
// 792 merged PRs, 340 had no commit SHA seen at all, and for 175 of those the
// PR's merge commit WAS emitted — the repo was watched, the branch commits were
// not.
//
// A pushed branch leaves refs/remotes/origin/<branch> in the SHARED ref store,
// which outlives the worktree, and local branches are shared by every worktree
// of the repo. So once per REPOSITORY per poll this pass reads every branch and
// remote-tracking tip (one rev-parse, which also yields the common dir that
// groups worktrees) and, only when that tip set changed since the last poll,
// lists `<tips> ^<previous tips> ^<every polled checkout's HEAD>` in one
// rev-list. Steady state is one spawn per root and no rev-list.
//
// AUTHOR GATE. A fetch moves remote tips over other people's commits. The HEAD
// pass has no notion of authorship (it emits whatever reaches HEAD, as
// `unattributed:` when no AI path matches), and the AI-edit ledger is
// path-level, so it cannot serve as the gate: a colleague's fetched commit that
// touches a file an agent edited here would match it and be attributed to this
// device's session. The only signal that separates "mine" from "theirs" is the
// commit identity, so the rev-list is limited to commits whose AUTHOR email is
// this repo's `user.email`. No identity configured means nothing is emitted.
//
// HEAD-REACHABLE COMMITS ARE THE HEAD PASS'S. Excluding every polled checkout's
// HEAD keeps rework folding (which only the HEAD pass does) from being skipped
// by an earlier off-HEAD emit, and the attributed-commits ledger dedupes the
// rest. Commits emitted here are attributed from the group's first root and
// never folded into rework.
//
// First sight of a repo baselines its tips without emitting, matching the HEAD
// pass's cold start.

func gitWatchRefTipsPath() string {
	return filepath.Join(state.StateDir(), "git-watch-ref-tips.json")
}

// repoKey (gitWatchRootKey of the common dir) -> sorted branch+remote tip SHAs.
func loadRefTips() map[string][]string {
	tips := map[string][]string{}
	_ = sign.WithBufferLock(gitWatchRefTipsPath()+".lock", func() error {
		if data, err := os.ReadFile(gitWatchRefTipsPath()); err == nil {
			_ = json.Unmarshal(data, &tips)
		}
		return nil
	})
	return tips
}

func saveRefTips(next map[string][]string) {
	if len(next) == 0 {
		return
	}
	_ = sign.WithBufferLock(gitWatchRefTipsPath()+".lock", func() error {
		merged := map[string][]string{}
		if data, err := os.ReadFile(gitWatchRefTipsPath()); err == nil {
			_ = json.Unmarshal(data, &merged)
		}
		for k, v := range next {
			merged[k] = v
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		tmp := gitWatchRefTipsPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, gitWatchRefTipsPath())
	})
}

// gitRefTips reads, in one spawn, root's common git dir, its HEAD, and the
// sorted, deduped tips of every local branch and remote-tracking ref.
func gitRefTips(root string) (commonDir, head string, tips []string, ok bool) {
	// #nosec G204 -- constant argv; root is a discovered checkout. Read-only.
	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-common-dir", "HEAD", "--branches", "--remotes").Output()
	if err != nil {
		return "", "", nil, false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return "", "", nil, false
	}
	commonDir = lines[0]
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	tips = lines[2:]
	slices.Sort(tips)
	return commonDir, lines[1], slices.Compact(tips), true
}

// gitNewOffHeadCommits lists, newest-first, commits by `email` reachable from
// tips but from none of `exclude`. --ignore-missing because a previous tip may
// have been gc'd after its branch was deleted.
func gitNewOffHeadCommits(root, email string, tips, exclude []string) ([]string, bool) {
	var in strings.Builder
	for _, t := range tips {
		in.WriteString(t + "\n")
	}
	for _, x := range exclude {
		in.WriteString("^" + x + "\n")
	}
	// #nosec G204 -- constant argv plus the repo's own configured email as a fixed-string pattern. Read-only.
	cmd := exec.Command("git", "-C", root, "rev-list", "--ignore-missing", "--stdin",
		"--fixed-strings", "--regexp-ignore-case", "--author=<"+email+">")
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return parseRevListShas(out), true
}

// pollOffHeadCommits runs the off-HEAD pass over the poll's roots and returns
// the SHAs it emitted (already added to `attributed`) for the caller to record.
func pollOffHeadCommits(session Session, roots []string, attributed map[string]struct{}, nowMs int64) []string {
	type repo struct {
		root  string
		heads []string
		tips  []string
	}
	var order []string
	repos := map[string]*repo{}
	for _, root := range roots {
		commonDir, head, tips, ok := gitRefTips(root)
		if !ok {
			continue
		}
		key := gitWatchRootKey(commonDir)
		if r, seen := repos[key]; seen {
			r.heads = append(r.heads, head)
			continue
		}
		repos[key] = &repo{root: root, heads: []string{head}, tips: tips}
		order = append(order, key)
	}

	prev := loadRefTips()
	next := map[string][]string{}
	var emitted []string
	budget := gitWatchMaxCommitsPerPollTotal
	for _, key := range order {
		r := repos[key]
		old, had := prev[key]
		if !had || slices.Equal(old, r.tips) {
			next[key] = r.tips // first sight baselines; unchanged needs nothing
			continue
		}
		// #nosec G204 -- constant argv; read-only.
		out, _ := exec.Command("git", "-C", r.root, "config", "--get", "user.email").Output()
		email := strings.TrimSpace(string(out))
		if email == "" {
			next[key] = r.tips // no identity: nothing here can be ours
			continue
		}
		shas, ok := gitNewOffHeadCommits(r.root, email, r.tips, append(old, r.heads...))
		if !ok {
			continue // keep the old tips; retry next poll
		}
		// A fetch also brings this engineer's OWN commits from their other
		// devices — same author, so the --author filter passes them. Commits that
		// provably arrived from a remote are not claimed (git_watch_origin.go);
		// anything in doubt still is.
		local := gitDeviceClaimableCommits(r.root, shas)
		var fresh []string
		for _, sha := range shas {
			if _, done := attributed[sha]; done {
				continue
			}
			if local != nil {
				if _, mine := local[sha]; !mine {
					continue
				}
			}
			fresh = append(fresh, sha)
		}
		// Oldest `limit` only; when anything is left over the tips stay put, and the
		// next poll re-lists the range minus what the ledger now holds.
		limit := min(budget, gitWatchMaxCommitsPerPoll)
		drained := len(fresh) <= limit
		if !drained {
			fresh = fresh[len(fresh)-limit:]
		}
		if len(fresh) > 0 && !requestLineOrigins(session, r.root, fresh) {
			continue
		}
		budget -= len(fresh)
		lin := newSiblingLineage(r.root, fresh)
		for i := len(fresh) - 1; i >= 0; i-- {
			// foreign=false: --author and the device-origin gate already kept only ours.
			if !attributeAndReworkCommit(session, r.root, fresh[i], false, false, nowMs, lin) {
				drained = false // enqueue failed: keep the tips so it is retried
				continue
			}
			attributed[fresh[i]] = struct{}{}
			emitted = append(emitted, fresh[i])
		}
		if drained {
			next[key] = r.tips
		}
	}
	if len(emitted) > 0 {
		state.HookDebugf("git-watch: %d off-HEAD commit(s) surfaced from branch/remote refs", len(emitted))
	}
	saveRefTips(next)
	return emitted
}
