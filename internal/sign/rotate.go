package sign

import (
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// bufferSegmentMaxBytes is the size at which the live ledger is rotated aside.
// With state.LedgerRetainedSegments this bounds the ledger to ~64MiB total.
//
// The ledger is an append-only local audit trail — nothing drains it, and events
// are POSTed independently — so before rotation it grew forever (~2.7MB/day on a
// busy device, i.e. ~1GB/year on a laptop).
const bufferSegmentMaxBytes = 16 << 20

// rotateLedgerIfLarge renames the live ledger aside once it exceeds
// bufferSegmentMaxBytes, shifting the retained segments down one and dropping
// the oldest.
//
// Caller MUST already hold WithBufferLock(state.BufferLockPath()). The lock is
// on a sentinel rather than the buffer precisely so this rename is safe: flock
// follows the inode, so locking the file we are about to rename would hand a
// concurrent opener a fresh inode and a second, independent lock.
//
// Rotation does not break the chain. The index is a separate file and survives,
// and a rebuild walks every retained segment oldest-to-newest, so a session
// whose tip landed in an earlier segment still links correctly. A session whose
// tip has aged out of all retained segments is far older than the index TTL, and
// its next event starts a new segment — the documented meaning of prevSig="".
//
// Best-effort throughout: a rotation failure must never fail an append. The cost
// of failing to rotate is a large file, which is the status quo; the cost of
// failing an append is a lost event.
func rotateLedgerIfLarge() {
	live := state.LedgerSegmentPath(0)
	fi, err := os.Stat(live)
	if err != nil || fi.Size() < bufferSegmentMaxBytes {
		return
	}

	// Dropping the oldest is allowed to fail: the shift below renames over it
	// anyway, which is the same outcome we wanted.
	oldest := state.LedgerSegmentPath(state.LedgerRetainedSegments)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		chainWarnf("could not drop oldest ledger segment: %v", err)
	}

	// Shift newest-last. A failure here MUST abort the whole cascade: if .2 -> .3
	// fails while .2 still exists, carrying on to .1 -> .2 renames over the only
	// copy of .2 and destroys it, taking any chain tips that lived only there.
	// rename replaces its destination, so a partially-applied cascade loses data
	// while an abandoned one loses nothing — the ledger simply keeps growing
	// until the next append retries, which is the status quo we are improving on.
	for n := state.LedgerRetainedSegments - 1; n >= 1; n-- {
		if err := os.Rename(state.LedgerSegmentPath(n), state.LedgerSegmentPath(n+1)); err != nil && !os.IsNotExist(err) {
			chainWarnf("could not shift ledger segment %d (%v) — leaving the ledger unrotated rather than overwriting a retained segment", n, err)
			return
		}
	}
	if err := os.Rename(live, state.LedgerSegmentPath(1)); err != nil {
		chainWarnf("could not rotate ledger (%v) — it will keep growing", err)
	}
}

// ledgerSegmentsOldestFirst returns the retained segment paths in chronological
// order, so a caller replaying them sees later events last.
func ledgerSegmentsOldestFirst() []string {
	paths := make([]string, 0, state.LedgerRetainedSegments+1)
	for n := state.LedgerRetainedSegments; n >= 0; n-- {
		paths = append(paths, state.LedgerSegmentPath(n))
	}
	return paths
}
