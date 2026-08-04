package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func hookDebugEnabled() bool {
	if os.Getenv("PROMPTSTER_DEBUG") == "1" {
		return true
	}
	_, err := os.Stat(filepath.Join(StateDir(), "debug-hooks"))
	return err == nil
}

func HookDebugf(format string, args ...interface{}) {
	if !hookDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "promptster-teams: "+format+"\n", args...)
}

func HookBufferPath() string {
	if p := os.Getenv("PROMPTSTER_BUFFER_PATH"); p != "" {
		return p
	}
	return filepath.Join(StateDir(), "buffer.jsonl")
}

// ChainStatePath is the per-session signature-chain index that pairs with
// HookBufferPath. It is deliberately derived from the buffer path rather than
// from StateDir so the two can never be mismatched: redirect
// PROMPTSTER_BUFFER_PATH (as the tests do) and the index follows it.
func ChainStatePath() string {
	return HookBufferPath() + ".chain.json"
}

// BufferLockPath is the sentinel the ledger append path locks.
//
// It is deliberately NOT the buffer itself. flock is held on an inode, so
// rotating the buffer aside would carry the lock with it while a concurrent
// opener created a fresh inode and locked *that* — two writers, both believing
// they hold the lock. Locking a sentinel that never rotates keeps mutual
// exclusion intact across rotation. Same idiom the dedup ledgers use.
func BufferLockPath() string {
	return HookBufferPath() + ".lock"
}

// --- outbox -----------------------------------------------------------------
//
// The outbox is the durable SEND QUEUE, deliberately separate from the ledger
// (HookBufferPath). They answer different questions and have incompatible
// lifecycles:
//
//   - buffer.jsonl is a signed, tamper-evident TRUST ARTIFACT. It is append-only
//     and ROTATES (sign/rotate.go renames it aside at 16MiB and drops the oldest
//     segment). A byte cursor into a rotating file is the same identity-vs-
//     position bug as path-keyed transcript offsets: after a rename the file
//     restarts at 0 and a stale cursor silently skips or replays. It is also
//     device-wide — presence and census append to it and POST themselves — so
//     draining it would re-send their events.
//   - outbox.jsonl is an unsigned QUEUE owned solely by the drain. Nothing
//     audits it, so it may be freely truncated once delivered.
//
// Do not merge them. The ledger's value is that nothing mutates it; the
// outbox's value is that the drain can.

// THE QUEUE IS SPLIT IN TWO, and the split is the point.
//
// `outbox.jsonl` carries LIVE events — work happening now. `outbox-backfill.jsonl`
// carries replayed history, which is the only producer that can genuinely wait:
// its source bytes are already durable on disk and its read offset has not
// advanced, so deferring it loses nothing.
//
// One FIFO could not express that. A schema bump enqueues tens of thousands of
// replayed events ahead of the next live one, so a prompt typed now is delivered
// after all of them — measured at 20,761 deep with the head 15 days old, while
// the dashboard reported the engineer idle. Worse, a permanently-failing event
// at the head blocked everything behind it. Two files, two cursors, two
// independent heads: neither of those can happen across lanes.
//
// The LIVE path keeps `outbox.jsonl` unchanged so a device upgrading mid-backlog
// keeps draining what it already queued, rather than stranding it under a new
// name.

func OutboxPath() string {
	if p := os.Getenv("PROMPTSTER_OUTBOX_PATH"); p != "" {
		return p
	}
	return filepath.Join(StateDir(), "outbox.jsonl")
}

// OutboxBackfillPath is the replay lane. Derived from OutboxPath so the two
// lanes always live in the same directory and one env override relocates both —
// a test or a sandbox that moved only one would silently write the other into
// the developer's real state dir.
func OutboxBackfillPath() string {
	p := OutboxPath()
	return strings.TrimSuffix(p, ".jsonl") + "-backfill.jsonl"
}

// OutboxCursorPath holds the byte offset of the next undelivered event. Derived
// from OutboxPath so the two can never be mismatched (same idiom as
// ChainStatePath).
func OutboxCursorPath() string {
	return OutboxPath() + ".cursor"
}

// OutboxLockPath is the sentinel guarding append/drain/compact. Like
// BufferLockPath it is NOT the queue file itself: compaction truncates the
// queue, and holding the lock on a sentinel keeps mutual exclusion intact
// regardless of what happens to the queue's inode.
func OutboxLockPath() string {
	return OutboxPath() + ".lock"
}

// OutboxCursorPathFor and OutboxLockPathFor derive a lane's cursor and lock from
// its queue path, so a new lane cannot be given a queue without also getting the
// matching pair. Same identity-not-position reasoning as OutboxCursorPath.
func OutboxCursorPathFor(queuePath string) string { return queuePath + ".cursor" }
func OutboxLockPathFor(queuePath string) string   { return queuePath + ".lock" }

// LedgerRetainedSegments is how many rotated segments are kept alongside the
// live buffer, bounding the ledger to (1+N) * the rotation threshold.
const LedgerRetainedSegments = 3

// LedgerSegmentPath returns the Nth ledger segment: 0 is the live buffer, 1 is
// the most recently rotated, N the oldest retained.
func LedgerSegmentPath(n int) string {
	if n <= 0 {
		return HookBufferPath()
	}
	return fmt.Sprintf("%s.%d", HookBufferPath(), n)
}
