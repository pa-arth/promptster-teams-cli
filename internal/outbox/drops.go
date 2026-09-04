package outbox

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// The DISCARD counter — how many events this queue has thrown away.
//
// WHY IT EXISTS. AppendTo drops an event when its lane has reached
// OutboxMaxBytes, and until this file the only report was a warnf to the
// device's stderr. On a daemon that is a stderr nobody reads, so the loss was
// device-local knowledge and nothing else: between 2026-08-31 and 2026-09-02 a
// customer lost roughly 15.6k events (measured ~5,900/day baseline against 19
// and 36 on the two worst days) and told US about it. Every surface we had said
// the same thing throughout — `pendingEvents: 63,965`, which at ~1KB/event is
// exactly the 64 MiB cap. A queue PINNED at its cap looks identical to a queue
// that is merely busy.
//
// This is the lagging half of the fix: non-zero means telemetry was lost, with
// no interpretation under which that is benign. OutboxBytes is the leading half.
//
// WHY IT PERSISTS. A counter that resets on restart would be close to useless
// here: the conditions that fill a queue — a wedged lane, a long outage — are
// precisely the conditions under which somebody restarts the daemon. The number
// has to outlive the process that lost the events.
//
// WHY ITS OWN FILE AND ITS OWN LOCK, and why the caller records OUTSIDE the
// lane lock. sign.WithBufferLock is a blocking flock with no timeout. Recording
// through a second one nested inside the lane's lock would hold the lane lock
// across an unrelated file write while every producer queues behind it, and
// would establish a lock ordering that nothing else is checked against. The same
// argument cursor_hook_overruns.go makes for its own separate file, one level
// down. See AppendTo: the drop is FLAGGED under the lane lock and RECORDED after
// it is released.
//
// THE CURSOR IS NEVER TOUCHED. Nothing here reads or writes lane.path() or
// lane.cursorPath(). The drop path advances no cursor — that invariant predates
// this counter and this counter must not be the thing that breaks it — so a
// failure to record a drop loses a count and never an offset.

func dropsPath() string {
	return filepath.Join(state.StateDir(), "outbox-drops.json")
}

// outboxDrops is the on-disk shape. PER LANE, matching the cap it counts
// against: OutboxMaxBytes is enforced per lane on purpose (a replay filling the
// backfill queue must not consume the headroom live capture needs), so "which
// lane lost work" is a real question with different answers — a live drop is
// work happening now with no second copy anywhere, a backfill drop is history
// whose source bytes may still be on disk.
//
// Only the SUM goes on the wire. The split stays here for `doctor`, because the
// backend's decision ("are we losing telemetry") does not branch on the lane and
// two columns to sharpen a number nobody branches on is not worth a beat field.
type outboxDrops struct {
	V        int   `json:"v"`
	Live     int64 `json:"live"`
	Backfill int64 `json:"backfill"`
}

const outboxDropsVersion = 1

func loadOutboxDrops() outboxDrops {
	c := outboxDrops{V: outboxDropsVersion}
	data, err := os.ReadFile(dropsPath()) // #nosec G304 -- state dir path.
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

// recordDrop bumps the named lane's counter, best-effort.
//
// MUST be called with the lane's append lock RELEASED — see the package comment
// above. Errors never propagate: the caller has already lost an event, and
// failing its Append because we could not also count the loss trades one dropped
// event for another. A failed RENAME is warned about rather than swallowed
// silently, because an under-reporting drop counter is the same class of bug as
// the drop it is counting.
func recordDrop(lane Lane) {
	_ = sign.WithBufferLock(dropsPath()+".lock", func() error {
		c := loadOutboxDrops()
		c.V = outboxDropsVersion
		if lane.Name == LaneBackfill().Name {
			c.Backfill++
		} else {
			c.Live++
		}
		data, err := json.Marshal(c)
		if err != nil {
			return nil
		}
		dir := filepath.Dir(dropsPath())
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil
		}
		// A UNIQUE temp in the same dir, then a CHECKED rename — the pattern
		// policy.writeDiskCache already uses, and for a reason that bites here
		// specifically. This lock serialises WRITERS, but DropCount/DropCounts
		// read outbox-drops.json without taking it, and on Windows a rename onto
		// a path another process currently has open fails with a sharing
		// violation. presence beats call DropCount on a timer while AppendTo can
		// be recording a drop, so those two do overlap.
		//
		// The old `_ = os.Rename(tmp, ...)` swallowed that: the increment was
		// lost, the temp file was left behind, and the counter under-reported the
		// very loss it exists to make visible. Best-effort still means the append
		// never fails for it — but a lost count is now SAID, not hidden, which is
		// the whole thesis of this file.
		tmp, err := os.CreateTemp(dir, "outbox-drops-*.json.tmp")
		if err != nil {
			return nil
		}
		name := tmp.Name()
		if _, err := tmp.Write(data); err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
			return nil
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(name)
			return nil
		}
		// os.CreateTemp already creates at 0600, so no chmod is needed.
		if err := os.Rename(name, dropsPath()); err != nil {
			_ = os.Remove(name)
			warnf("dropped an event from the %s lane but could not persist the drop count: %v. The counter under-reports; the loss itself is real.",
				lane.Name, err)
		}
		return nil
	})
}

// DropCount is what the liveness beat reports: both lanes, summed, cumulative
// for this MACHINE's lifetime.
//
// Cumulative and lifetime-scoped to match cursorHookRepairs / cursorStopSeen,
// which the backend already stores last-write-wins on a per-KEY row. Two
// machines sharing a key alternate and the stored value is one machine's total —
// a real limitation, documented on the column rather than papered over here.
func DropCount() int64 {
	c := loadOutboxDrops()
	return c.Live + c.Backfill
}

// DropCounts is the per-lane split, for `doctor`. The engineer whose machine is
// throwing events away is entitled to read that locally and to know which queue.
func DropCounts() (live, backfill int64) {
	c := loadOutboxDrops()
	return c.Live, c.Backfill
}

// OutboxBytes is the LEADING indicator: how full the fullest lane is right now.
//
// A MAX, DELIBERATELY NOT A SUM, and the distinction is the whole point. The cap
// is per lane, so the drop condition is "any lane at OutboxMaxBytes". A sum
// cannot express it: 32 MiB live + 32 MiB backfill and 64 MiB live + 0 backfill
// are the same total and opposite situations — the first has half its headroom,
// the second is dropping every event it is handed. Reporting the max makes
// OutboxBytes/OutboxMaxBytes a true fill ratio for the lane nearest the cliff.
//
// Cheap by construction (two Stats, no lock), exactly like UnderPressure: this
// is advisory pressure rather than an invariant, so a torn read costs one beat.
func OutboxBytes() int64 {
	var max int64
	for _, lane := range []Lane{LaneLive(), LaneBackfill()} {
		fi, err := os.Stat(lane.path())
		if err != nil {
			continue // missing file is an empty lane, not an error
		}
		if fi.Size() > max {
			max = fi.Size()
		}
	}
	return max
}
