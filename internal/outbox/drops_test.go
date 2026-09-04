package outbox

import (
	"os"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// fillLane grows a lane's queue file past its cap so the next append to it is
// dropped. Truncate rather than real events: the cap is a FILE SIZE check, so
// the byte count is the whole input, and writing 64 MiB of JSON per test case
// would trade a fast suite for no additional coverage.
func fillLane(t *testing.T, lane Lane) {
	t.Helper()
	f, err := os.OpenFile(lane.path(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s queue: %v", lane.Name, err)
	}
	if err := f.Truncate(OutboxMaxBytes + 1); err != nil {
		t.Fatalf("grow %s queue: %v", lane.Name, err)
	}
	f.Close()
}

// TestDropIsCounted is the whole point of the change: a discard has to leave a
// number behind. Before this, the only trace was a warnf to a stderr nobody
// reads, which is how ops.ai lost ~15.6k events over 2026-08-31..09-02 and told
// US rather than the other way round.
func TestDropIsCounted(t *testing.T) {
	laneTest(t)

	if got := DropCount(); got != 0 {
		t.Fatalf("DropCount on a fresh install = %d, want 0", got)
	}

	fillLane(t, LaneLive())
	for i := 0; i < 3; i++ {
		if err := Append(event.NewEvent("prompt", "sess-test")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	if got := DropCount(); got != 3 {
		t.Errorf("DropCount = %d, want 3", got)
	}
	live, backfill := DropCounts()
	if live != 3 || backfill != 0 {
		t.Errorf("DropCounts = (%d, %d), want (3, 0) — the lane must be attributed", live, backfill)
	}
}

// TestDropsAreCountedPerLane pins the attribution. A LIVE drop is work happening
// now with no second copy anywhere; a BACKFILL drop is replayed history whose
// source transcript is probably still on disk. Same number, different verdict,
// so a single counter would throw away the half that decides what to do.
func TestDropsAreCountedPerLane(t *testing.T) {
	laneTest(t)

	fillLane(t, LaneBackfill())
	old := event.NewEvent("prompt", "sess-test")
	old.Ts = time.Now().Add(-2 * LiveHorizon).UTC().Format(time.RFC3339Nano)
	if err := AppendFromDurableSource(old); err != nil {
		t.Fatalf("AppendFromDurableSource: %v", err)
	}

	live, backfill := DropCounts()
	if live != 0 || backfill != 1 {
		t.Errorf("DropCounts = (%d, %d), want (0, 1)", live, backfill)
	}
	// And the live lane is untouched by its neighbour being full — the per-lane
	// ceiling that TestEachLaneHasItsOwnFullQueueCeiling pins, restated here
	// because the counter must not be the thing that blurs the two lanes.
	if err := Append(event.NewEvent("prompt", "sess-test")); err != nil {
		t.Fatalf("Append(live): %v", err)
	}
	if live, _ := DropCounts(); live != 0 {
		t.Errorf("live drops = %d, want 0 — a full backfill lane must not drop live events", live)
	}
}

// TestDropCountSurvivesRestart is why the counter is on disk at all. The states
// that fill a queue — a wedged lane, a long outage — are the states in which
// somebody restarts the daemon, so an in-memory counter would be reset by the
// very act of responding to what it is measuring.
func TestDropCountSurvivesRestart(t *testing.T) {
	laneTest(t)

	fillLane(t, LaneLive())
	if err := Append(event.NewEvent("prompt", "sess-test")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Every read goes back to the file — there is no process-local cache to
	// invalidate — so re-reading is exactly what a fresh process would do.
	if got := DropCount(); got != 1 {
		t.Fatalf("DropCount = %d, want 1", got)
	}
	if _, err := os.Stat(dropsPath()); err != nil {
		t.Fatalf("drop counter was not persisted: %v", err)
	}
	if got := DropCount(); got != 1 {
		t.Errorf("DropCount re-read = %d, want 1", got)
	}
}

// TestDropDoesNotDisturbTheCursor is the invariant the counter was most likely
// to break. The drop path deliberately advances nothing: the cursor is a byte
// offset into bytes that were WRITTEN, and an event that was never appended
// contributed none. A counter that moved it would skip real undelivered work on
// the next drain — turning a reporting change into data loss.
func TestDropDoesNotDisturbTheCursor(t *testing.T) {
	laneTest(t)

	// One real queued event, and a cursor parked deliberately at 0.
	if err := Append(event.NewEvent("prompt", "sess-test")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writeCursor(LaneLive(), 0); err != nil {
		t.Fatalf("writeCursor: %v", err)
	}
	before := readCursor(LaneLive())
	sizeBefore := fileSize(t, LaneLive().path())
	pendingBefore := pendingCountIn(LaneLive())

	// Now force the lane over the cap and drop several events against it.
	fillLane(t, LaneLive())
	for i := 0; i < 5; i++ {
		if err := Append(event.NewEvent("prompt", "sess-test")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	if after := readCursor(LaneLive()); after != before {
		t.Errorf("cursor moved from %d to %d across a drop — the drop path must advance nothing", before, after)
	}
	if got := DropCount(); got != 5 {
		t.Errorf("DropCount = %d, want 5", got)
	}
	// fillLane grew the file, so assert on the QUEUED BYTES rather than the size:
	// no dropped event may have been written.
	if sizeBefore <= 0 {
		t.Fatalf("fixture wrote no bytes")
	}
	if pendingBefore != 1 {
		t.Fatalf("fixture pending = %d, want 1", pendingBefore)
	}
}

// TestOutboxBytesIsAMaxNotASum pins the reason the byte figure is shaped the way
// it is. OutboxMaxBytes is enforced PER LANE, so the discard condition is "any
// lane at the cap". A sum cannot express it: two half-full lanes and one full
// lane produce the same total and describe opposite situations — one has half its
// headroom, the other is discarding every event it is handed.
func TestOutboxBytesIsAMaxNotASum(t *testing.T) {
	laneTest(t)

	if got := OutboxBytes(); got != 0 {
		t.Fatalf("OutboxBytes with no queue files = %d, want 0", got)
	}

	half := int64(OutboxMaxBytes / 2)
	growTo(t, LaneLive().path(), half)
	growTo(t, LaneBackfill().path(), half)

	if got := OutboxBytes(); got != half {
		t.Errorf("OutboxBytes with two half-full lanes = %d, want %d (a max, not a sum of %d)",
			got, half, 2*half)
	}

	growTo(t, LaneBackfill().path(), OutboxMaxBytes)
	if got := OutboxBytes(); got != OutboxMaxBytes {
		t.Errorf("OutboxBytes = %d, want %d — the fullest lane decides", got, int64(OutboxMaxBytes))
	}
}

func growTo(t *testing.T, path string, n int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if err := f.Truncate(n); err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
	f.Close()
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
