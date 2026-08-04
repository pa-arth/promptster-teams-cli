package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Capture roots are the directories this install captures from, beyond the
// running daemon's own watch dir.
//
// The problem they solve: the single-instance lock lives at
// StateDir()/watch.lock — ONE per user account, not one per directory. So a
// second `promptster-teams start` from an unrelated tree used to see the lock
// held, bow out, and change nothing. The engineer got a "already running"
// message that read like success while every session in that second tree was
// silently classified as a cwd mismatch and dropped. One lock is correct (two
// watchers would double-count presence and corrupt seat utilization); what was
// wrong was that the second `start` had no way to say "also capture here".
//
// Now it registers the directory instead of declining, and the running daemon
// picks it up on its next poll — the list is re-read every poll, never cached
// in the process. Widening capture is consent-shaped, so `start` prints every
// watched directory rather than widening silently.
type captureRootsFile struct {
	Roots []string `json:"roots"`
}

// maxCaptureRoots bounds the list. A registration that would exceed it drops
// the oldest entry: an unbounded file would grow forever for anyone who runs
// `start` from throwaway directories, and every root costs a `git worktree
// list` on every poll.
const maxCaptureRoots = 32

func captureRootsPath() string {
	return filepath.Join(state.StateDir(), "capture-roots.json")
}

// RegisteredCaptureRoots returns the registered directories, oldest first.
// A missing or unreadable file means none — capture then behaves exactly as it
// did before this file existed.
func RegisteredCaptureRoots() []string {
	data, err := os.ReadFile(captureRootsPath())
	if err != nil {
		return nil
	}
	var f captureRootsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	out := make([]string, 0, len(f.Roots))
	for _, r := range f.Roots {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// RegisterCaptureRoot adds dir to the captured set and returns whether the set
// actually changed plus the full list after the write.
//
// Containment is folded, not just exact duplicates: registering a child of an
// existing root is a no-op (already covered), and registering a parent
// REPLACES the children it now subsumes. Without that, `start` in ~ after
// `start` in ~/work would report two watched directories when one describes
// the truth, and the printed confirmation has to be honest to be worth
// printing.
// The whole read-modify-write runs under a cross-process lock, and the caller
// MUST report a returned error rather than printing that the directory is
// covered: a registration that failed and read as success is the exact failure
// this feature exists to remove.
func RegisterCaptureRoot(dir string) (added bool, all []string, err error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false, RegisteredCaptureRoots(), nil
	}
	dir = resolvePath(dir)

	all = RegisteredCaptureRoots()
	// Registration is read-modify-write on a shared set, so two concurrent
	// `start`/`login`/`watch` invocations in different trees would otherwise
	// both compute a replacement from the same prior state and the loser's root
	// would vanish — silently uncaptured, which is the bug this file fixes. The
	// same lock the ledgers use serializes them.
	lockErr := sign.WithBufferLock(captureRootsPath()+".lock", func() error {
		existing := RegisteredCaptureRoots()
		all = existing
		kept := make([]string, 0, len(existing)+1)
		for _, r := range existing {
			if pathWithin(dir, r) {
				// Already covered by a root at or above dir: nothing to do, and the
				// file must not be rewritten (a no-op write would churn the
				// fingerprint below and pointlessly invalidate match caches).
				return nil
			}
			if pathWithin(r, dir) {
				continue // subsumed by the new, broader root
			}
			kept = append(kept, r)
		}
		kept = append(kept, dir)
		if len(kept) > maxCaptureRoots {
			kept = kept[len(kept)-maxCaptureRoots:]
		}

		if err := os.MkdirAll(filepath.Dir(captureRootsPath()), 0o700); err != nil {
			return err
		}
		data, err := json.Marshal(captureRootsFile{Roots: kept})
		if err != nil {
			return err
		}
		// Atomic rename through a PID-unique temp name. The daemon reads this
		// file mid-poll and must never see a half-written list (which unmarshals
		// to zero roots and would NARROW capture for a poll) — and a shared temp
		// name would let two writers interleave bytes into one file before either
		// rename, producing exactly that.
		tmp := fmt.Sprintf("%s.tmp.%d", captureRootsPath(), os.Getpid())
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, captureRootsPath()); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		all, added = kept, true
		return nil
	})
	if lockErr != nil {
		return false, all, lockErr
	}
	return added, all, nil
}

// captureRootsFingerprint is a stable digest of an effective root SET (order
// and duplicates do not matter).
//
// Both watchers cache per-file classification decisions. Storing this digest
// alongside the cache lets a poll invalidate those decisions whenever the
// effective root set changes. That includes cached "yes" entries: narrowing
// the roots can make a previously accepted transcript fall outside the current
// capture scope. Byte offsets live in a separate map and remain untouched, so
// reclassification never re-uploads content that was already consumed.
func captureRootsFingerprint(roots []string) string {
	uniq := make([]string, 0, len(roots))
	seen := map[string]bool{}
	for _, r := range roots {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		uniq = append(uniq, r)
	}
	sort.Strings(uniq)
	sum := sha256.Sum256([]byte(strings.Join(uniq, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// syncMatchCacheToRoots reconciles a watcher's classification cache with the
// root set it is about to classify against. When the set changed since those
// decisions were made, every cached decision is dropped in place. Widening can
// turn "no" into "yes"; narrowing can turn "yes" into "no". The callers retain
// their independent offset maps, so an accepted transcript that remains in
// scope resumes from its durable offset after reclassification.
//
// Returns the fingerprint to store, how many cached decisions were dropped, and
// whether anything changed. It is a function rather than inline poll code so
// the decision is observable: the poll re-caches an unchanged "no" within the
// same pass, which makes "invalidate always" and "invalidate on change" look
// identical from the outside while differing by a full re-parse of every
// mismatched file on every 3-second poll.
func syncMatchCacheToRoots(match map[string]string, storedFP string, roots []string) (fp string, dropped int, changed bool) {
	fp = captureRootsFingerprint(roots)
	if fp == storedFP {
		return fp, 0, false
	}
	for k := range match {
		delete(match, k)
		dropped++
	}
	return fp, dropped, true
}
