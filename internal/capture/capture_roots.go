package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
func RegisterCaptureRoot(dir string) (added bool, all []string, err error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false, RegisteredCaptureRoots(), nil
	}
	dir = resolvePath(dir)

	existing := RegisteredCaptureRoots()
	kept := make([]string, 0, len(existing)+1)
	for _, r := range existing {
		if pathWithin(dir, r) {
			// Already covered by a root at or above dir: nothing to do, and the
			// file must not be rewritten (a no-op write would churn the
			// fingerprint below and pointlessly invalidate match caches).
			return false, existing, nil
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
		return false, existing, err
	}
	data, err := json.Marshal(captureRootsFile{Roots: kept})
	if err != nil {
		return false, existing, err
	}
	// Atomic rename: the daemon reads this file mid-poll and must never see a
	// half-written list (which unmarshals to zero roots and would narrow
	// capture for one poll).
	tmp := captureRootsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, existing, err
	}
	if err := os.Rename(tmp, captureRootsPath()); err != nil {
		_ = os.Remove(tmp)
		return false, existing, err
	}
	return true, kept, nil
}

// captureRootsFingerprint is a stable digest of an effective root SET (order
// and duplicates do not matter).
//
// Both watchers cache a per-file "no" classification forever, so widening the
// roots is invisible to any file already judged a mismatch — a newly
// registered directory's existing sessions would stay dropped for the life of
// the progress file. Storing this digest alongside the cache lets a poll drop
// exactly the "no" entries when the set changed. "yes" entries and byte
// offsets are untouched: widening never un-matches anything, so re-tailing a
// matched file from zero would duplicate events.
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
// decisions were made, every cached "no" is dropped in place so the widened set
// is applied to files already judged mismatches; "yes" entries survive, because
// widening never un-matches anything and clearing one would reset its byte
// offset and re-upload a whole transcript.
//
// Returns the fingerprint to store, how many mismatches were dropped, and
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
	for k, v := range match {
		if v == "no" {
			delete(match, k)
			dropped++
		}
	}
	return fp, dropped, true
}
