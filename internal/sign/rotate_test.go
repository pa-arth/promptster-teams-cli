package sign

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// fillLedgerPastRotation pads the live ledger past the rotation threshold with
// blank lines, which the tip scan skips — so the padding cannot affect chaining,
// only size.
func fillLedgerPastRotation(t *testing.T) {
	t.Helper()
	// #nosec G304 -- test-local path from t.TempDir via PROMPTSTER_BUFFER_PATH.
	f, err := os.OpenFile(state.LedgerSegmentPath(0), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer f.Close()
	pad := strings.Repeat("\n", 1<<20)
	for written := 0; written < bufferSegmentMaxBytes; written += len(pad) {
		if _, err := f.WriteString(pad); err != nil {
			t.Fatalf("pad ledger: %v", err)
		}
	}
}

// TestLedgerRotatesAndChainSurvives is the load-bearing rotation test. A tip that
// gets rotated into an older segment must still be found, or every active
// session silently forks at each rotation — which would make rotation a cure
// worse than the disease.
func TestLedgerRotatesAndChainSurvives(t *testing.T) {
	pub := setupChainTest(t)

	appendEvent(t, "prompt", "sess-a")
	tipBeforeRotation := readBuffer(t)[0].Sig

	fillLedgerPastRotation(t)
	// Drop the index so the tip MUST be recovered by scanning segments — this is
	// what proves rotated segments are still walked.
	if err := os.Remove(state.ChainStatePath()); err != nil {
		t.Fatalf("remove index: %v", err)
	}

	appendEvent(t, "command", "sess-a")

	if _, err := os.Stat(state.LedgerSegmentPath(1)); err != nil {
		t.Fatalf("ledger did not rotate: %v", err)
	}
	live, err := os.Stat(state.LedgerSegmentPath(0))
	if err != nil {
		t.Fatalf("stat live ledger: %v", err)
	}
	if live.Size() >= bufferSegmentMaxBytes {
		t.Errorf("live ledger is %d bytes after rotation, want a fresh small one", live.Size())
	}

	evs := readBuffer(t)
	if len(evs) != 1 {
		t.Fatalf("live segment has %d events, want 1 (the rest rotated away)", len(evs))
	}
	// Deliberately NOT verifyChain: the live segment's first event legitimately
	// carries a non-empty prevSig here, because its parent lives in the rotated
	// segment. That is the whole point of the test.
	if evs[0].PrevSig != tipBeforeRotation {
		t.Errorf("prevSig = %q, want %q — the tip in the rotated segment was not found, so the chain forked", evs[0].PrevSig, tipBeforeRotation)
	}
	msg, err := BuildSigningMessage(evs[0], evs[0].PrevSig)
	if err != nil {
		t.Fatalf("build signing message: %v", err)
	}
	sig, err := hex.DecodeString(evs[0].Sig)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		t.Error("post-rotation event signature does not verify")
	}
}

// TestLedgerRotationBoundsDiskUse: segments beyond the retention window are
// dropped, so the ledger stops growing forever.
func TestLedgerRotationBoundsDiskUse(t *testing.T) {
	setupChainTest(t)

	for i := 0; i <= state.LedgerRetainedSegments+2; i++ {
		appendEvent(t, "command", "sess-a")
		fillLedgerPastRotation(t)
		appendEvent(t, "command", "sess-a")
	}

	// The oldest retained segment may exist; anything past it must not.
	beyond := state.LedgerSegmentPath(state.LedgerRetainedSegments + 1)
	if _, err := os.Stat(beyond); !os.IsNotExist(err) {
		t.Errorf("segment %s exists — retention window is not bounding the ledger", beyond)
	}

	var total int64
	for n := 0; n <= state.LedgerRetainedSegments; n++ {
		if fi, err := os.Stat(state.LedgerSegmentPath(n)); err == nil {
			total += fi.Size()
		}
	}
	if maxTotal := int64(bufferSegmentMaxBytes) * int64(state.LedgerRetainedSegments+2); total > maxTotal {
		t.Errorf("ledger totals %d bytes, want <= %d", total, maxTotal)
	}
}

// TestFailedShiftDoesNotOverwriteRetainedSegment pins that a partially-applied
// rotation cascade never destroys a segment. rename replaces its destination, so
// if .2 -> .3 fails while .2 still exists, carrying on to .1 -> .2 would rename
// over the only copy of .2 — losing any chain tips that lived only there.
//
// Blocks the shift by parking a non-empty directory at .3, which both Remove and
// Rename refuse. On Windows this shape is reachable simply by another process
// holding a segment open.
func TestFailedShiftDoesNotOverwriteRetainedSegment(t *testing.T) {
	setupChainTest(t)

	seg1, seg2 := state.LedgerSegmentPath(1), state.LedgerSegmentPath(2)
	if err := os.WriteFile(seg1, []byte("segment-one\n"), 0o600); err != nil {
		t.Fatalf("seed .1: %v", err)
	}
	if err := os.WriteFile(seg2, []byte("segment-two\n"), 0o600); err != nil {
		t.Fatalf("seed .2: %v", err)
	}

	blocker := state.LedgerSegmentPath(state.LedgerRetainedSegments)
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatalf("park blocker dir: %v", err)
	}
	if err := os.WriteFile(blocker+"/occupied", []byte("x"), 0o600); err != nil {
		t.Fatalf("occupy blocker dir: %v", err)
	}

	fillLedgerPastRotation(t)
	appendEvent(t, "command", "sess-a")

	got, err := os.ReadFile(seg2)
	if err != nil {
		t.Fatalf("read .2: %v", err)
	}
	if string(got) != "segment-two\n" {
		t.Errorf(".2 = %q, want %q — a failed shift overwrote a retained segment and destroyed its chain tips", got, "segment-two\n")
	}
	if got, err := os.ReadFile(seg1); err != nil || string(got) != "segment-one\n" {
		t.Errorf(".1 = %q (err %v), want %q — the cascade should have aborted intact", got, err, "segment-one\n")
	}
}

// TestRotationPreservesLockExclusion pins the reason the lock is a sentinel
// rather than the buffer itself: rotation renames the buffer, and flock follows
// the inode, so locking the buffer would let a concurrent opener lock a fresh
// inode and enter the critical section too. If the lock path ever moves back
// onto the buffer, this catches it.
func TestRotationPreservesLockExclusion(t *testing.T) {
	setupChainTest(t)

	if state.BufferLockPath() == state.HookBufferPath() {
		t.Fatal("the append lock is the buffer itself — rotation would move the lock off the inode concurrent openers use")
	}
	for n := 1; n <= state.LedgerRetainedSegments; n++ {
		if state.BufferLockPath() == state.LedgerSegmentPath(n) {
			t.Fatalf("the append lock collides with rotated segment %d", n)
		}
	}
}
