package state

import (
	"fmt"
	"os"
	"path/filepath"
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
