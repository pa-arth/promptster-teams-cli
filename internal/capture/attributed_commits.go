package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// attributedCommitsPath persists which commit SHAs this device has ALREADY
// emitted a commit_attribution event for, making attribution idempotent on the
// CLIENT rather than relying on the server to swallow repeats.
//
// Why this exists (measured on teams prod 2026-07-21..23):
//   - 125,877 `hook.commit_attribution` POSTs stored 4,173 useful rows — ~30x
//     redundant. Ingest is 3 sequential single-row DB round-trips per event, so
//     the waste is ~375k round-trips for ~12k of real work.
//   - The redundancy arrives in BURSTS, not a steady trickle: a single burst of
//     7,482 events with a median inter-arrival gap of 0.1s. That is far above
//     gitWatchMaxCommitsPerPoll, because that cap is PER ROOT and a poll walks
//     every discovered root (63 on the measured device).
//
// The cursor in git-watch-cursors.json is NOT sufficient to prevent this. It
// answers "has HEAD moved since I last looked at this root", which is a
// different question from "have I already attributed this commit". Every path
// that loses or invalidates a cursor re-detects commits that were already sent:
//   - gitNewCommits' recovery path — when `rev-list lastSeen..head` errors
//     because lastSeen was made unreachable (a rebase, a deleted worktree), it
//     falls back to `rev-list -n <cap> head` and re-surfaces the newest commits
//     wholesale, every one of which has typically already been attributed;
//   - root churn — roots are keyed by absolute path, and 41 of the 63 discovered
//     roots on the measured device were `.claude/worktrees/<slug>` directories
//     that no longer exist. Worktrees are created and deleted continuously.
//
// Keying on the SHA alone (not root+SHA) is deliberate: a commit is
// content-addressed, so the same SHA seen through a second worktree of the same
// repo is the same commit and must be attributed once. This is also exactly the
// idempotence git_watch.go's clamp comment always assumed existed.
func attributedCommitsPath() string {
	return filepath.Join(state.StateDir(), "attributed-commits.json")
}

// attributedCommitTTLMs keeps a SHA remembered long enough to outlive the
// windows that cause re-detection. gitNewCommits' recovery window re-surfaces
// the newest commits of a repo regardless of age, so the horizon must be
// generous; it is matched to the discovery horizon so a repo cannot still be
// polled while its attributed SHAs have already been forgotten.
var attributedCommitTTLMs = discoveredRepoTTLMs

// attributedCommitsMax hard-bounds the file so a pathological repo (or a machine
// with many active repos) cannot grow it without limit. When exceeded, the
// OLDEST entries are dropped first — the newest SHAs are the ones a recovery
// window is most likely to re-surface, so they are the ones worth keeping.
// A package var, not a const, so a test can lower it without writing 20k rows
// (same reason as gitWatchMaxCommitsPerPoll).
var attributedCommitsMax = 20000

const attributedCommitsVersion = 1

// attributedCommits maps a commit SHA to the ms timestamp at which this device
// emitted attribution for it. LOCAL-only state, never transmitted.
type attributedCommits struct {
	V    int              `json:"v"`
	Shas map[string]int64 `json:"shas"`
}

// loadAttributedCommits returns the set of SHAs already attributed and still
// within the TTL. Expired rows are ignored and compacted on the next write.
// Best-effort: an unreadable or corrupt file yields an empty set, which degrades
// to today's behaviour (a possible repeat) rather than dropping attribution.
func loadAttributedCommits(nowMs int64) map[string]struct{} {
	seen := map[string]struct{}{}
	_ = sign.WithBufferLock(attributedCommitsPath()+".lock", func() error {
		data, err := os.ReadFile(attributedCommitsPath())
		if err != nil {
			return nil
		}
		var onDisk attributedCommits
		if json.Unmarshal(data, &onDisk) != nil || onDisk.V != attributedCommitsVersion {
			return nil
		}
		for sha, ts := range onDisk.Shas {
			if sha != "" && nowMs-ts <= attributedCommitTTLMs {
				seen[sha] = struct{}{}
			}
		}
		return nil
	})
	return seen
}

// recordAttributedCommits stamps nowMs onto every SHA just emitted, prunes past
// the TTL, and enforces attributedCommitsMax by dropping the oldest entries.
// Best-effort: I/O failure never blocks a poll. A failed write costs at most a
// repeat on a later poll, which is the behaviour this file replaces.
func recordAttributedCommits(shas []string, nowMs int64) {
	if len(shas) == 0 {
		return
	}
	_ = sign.WithBufferLock(attributedCommitsPath()+".lock", func() error {
		merged := attributedCommits{V: attributedCommitsVersion, Shas: map[string]int64{}}
		if data, err := os.ReadFile(attributedCommitsPath()); err == nil {
			var onDisk attributedCommits
			if json.Unmarshal(data, &onDisk) == nil && onDisk.Shas != nil {
				merged.Shas = onDisk.Shas
			}
		}
		for _, sha := range shas {
			if sha != "" {
				merged.Shas[sha] = nowMs
			}
		}
		for sha, ts := range merged.Shas {
			if nowMs-ts > attributedCommitTTLMs {
				delete(merged.Shas, sha)
			}
		}
		if len(merged.Shas) > attributedCommitsMax {
			type row struct {
				sha string
				ts  int64
			}
			rows := make([]row, 0, len(merged.Shas))
			for sha, ts := range merged.Shas {
				rows = append(rows, row{sha, ts})
			}
			// Oldest first, SHA as a deterministic tiebreak so the eviction set is
			// stable when many commits share a timestamp (a burst drains in one ms).
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].ts != rows[j].ts {
					return rows[i].ts < rows[j].ts
				}
				return rows[i].sha < rows[j].sha
			})
			for _, r := range rows[:len(rows)-attributedCommitsMax] {
				delete(merged.Shas, r.sha)
			}
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		tmp := attributedCommitsPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, attributedCommitsPath())
	})
}
