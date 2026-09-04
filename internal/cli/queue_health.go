package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// The send queue (internal/outbox) is durable and drains in the background, so
// its failures are invisible: a revoked key just means every POST 401s while
// events pile up locally and the only trace is a stderr line in daemon.log that
// nobody tails. Doctor is where an engineer looks when capture feels off, so the
// queue reports its health here.
//
// A raw pending count is NOT a health signal — a machine that captured events
// and then stopped watching legitimately holds a backlog forever. Warning on
// depth alone would cry wolf on the single most common idle state and train
// everyone to ignore doctor. What matters is whether the queue is DRAINING when
// something is supposed to be draining it.

// queueStuckAfter bounds how long the queue may hold events with no delivery
// progress before doctor calls it stuck. The drain retries on a backoff capped
// at 30s, so a queue that is merely slow or retrying still advances well inside
// this window; two minutes leaves room for a brief outage without a false alarm.
const queueStuckAfter = 2 * time.Minute

// queueNearFullPercent is the fill level at which doctor warns. Append DROPS
// events outright once the outbox reaches OutboxMaxBytes, so the warning has to
// land before the cap to be worth anything.
const queueNearFullPercent = 75

type queueLevel int

const (
	queueOK queueLevel = iota
	queueWarn
	queueErr
)

// queueLine is one doctor line: a severity and the text after the glyph.
type queueLine struct {
	level queueLevel
	text  string
}

func (l queueLine) glyph() string {
	switch l.level {
	case queueErr:
		return errGlyph
	case queueWarn:
		return warnGlyph
	default:
		return okGlyph
	}
}

// queueInputs is everything checkQueueHealth needs from the outside world.
// Injecting it keeps the decision pure: the tests that matter here are about
// judgement (is a backlog a problem?), and they should not need a live watcher,
// a real clock, or a real filesystem to ask that question.
type queueInputs struct {
	pending    int
	size       int64 // outbox bytes on disk
	haveOutbox bool  // false on a machine that has never captured

	// lane names which queue the size/unreadable/progress fields describe, empty
	// when it is the live one.
	//
	// The queue is two lanes now (live and backfill), and every scalar here is
	// the WORST of them rather than live's. Reading live alone is the failure
	// this field exists to prevent: a backfill lane at the cap is dropping
	// replayed events right now, and a doctor that measured only live would print
	// "delivery queue draining" over the top of it. Which lane is in trouble
	// changes what the engineer does about it, so it is named in the message.
	lane string

	// unreadable is set when a lane exists but cannot be opened. It has to be
	// tracked separately because PendingCount reports 0 for an unreadable queue
	// exactly as it does for an empty one, and the two must not read the same.
	unreadable bool

	// lastProgress is when delivery last made progress: the cursor's mtime, or
	// the newest live watcher's start time when no cursor exists at all.
	//
	// Across lanes this is the OLDEST such time among lanes that actually have
	// pending work — a lane with an empty queue has an old cursor for the honest
	// reason that there was nothing to advance it, and letting that decide the
	// verdict would report every idle machine as stuck. Conversely, taking the
	// NEWEST would let a healthy live lane paper over a backfill lane that has
	// not moved in hours, which is the whole point of measuring both.
	lastProgress time.Time
	haveProgress bool

	// draining reports that a live watcher is up, and with it the drain loop.
	// StartDrain is a process-wide singleton that both watchers call, so either
	// one being alive means the queue is being worked.
	draining bool

	// dropsLive / dropsBackfill are the DURABLE discard counts, cumulative for
	// this machine's lifetime (outbox.DropCounts).
	//
	// Reported per lane and not summed, because the two mean different things to
	// the engineer reading this: a LIVE drop is work that was happening at that
	// moment with no second copy anywhere, while a BACKFILL drop is replayed
	// history whose source transcript is probably still on disk. The first is
	// gone; the second may be recoverable.
	//
	// Separate from `size` on purpose. Size is the LEADING indicator — how close
	// this queue is to discarding — and it goes back down when the queue drains.
	// These only ever rise, and a non-zero one is a statement about the past that
	// stays true after the queue empties, which is exactly why it is reported
	// even on a healthy-looking machine.
	dropsLive     int64
	dropsBackfill int64

	now time.Time
}

// checkQueueHealth turns a queue snapshot into doctor lines. Normally that is a
// single line describing the backlog and whether it is moving; a queue nearing
// the cap adds a size line above it, and a queue at the cap replaces it. Nothing
// is reported as a problem unless it actually is one.
func checkQueueHealth(in queueInputs) []queueLine {
	var lines []queueLine

	// EVENTS ALREADY LOST, first and unconditionally. This is history, not a
	// current-state reading: it survives the queue draining, it never goes back
	// down, and there is no interpretation of a non-zero value under which
	// nothing is wrong. It leads because a machine that has thrown telemetry away
	// and since recovered otherwise prints an all-clear, which is precisely the
	// silence that let a customer lose ~15.6k events over 2026-08-31..09-02 and
	// tell us about it before our own telemetry did.
	if in.dropsLive > 0 || in.dropsBackfill > 0 {
		lines = append(lines, queueLine{queueErr, fmt.Sprintf(
			"delivery queue has DROPPED %s since install (live %d, backfill %d) — those events were never uploaded and cannot be recovered from here; they remain in the signed local ledger. See %s",
			eventCount64(in.dropsLive+in.dropsBackfill), in.dropsLive, in.dropsBackfill,
			capture.DaemonLogPath())})
	}

	// A full outbox dominates and is reported alone. Append drops on FILE SIZE,
	// not on backlog depth, so a queue that has drained but not yet compacted is
	// still at the cap and still dropping every new event — meaning "FULL" and a
	// depth of zero are both true at once. Printed side by side they read as a
	// contradiction ("queue FULL" / "queue empty"), so the depth line is dropped
	// here: when events are being lost right now, that is the only message.
	if in.haveOutbox && in.size >= outbox.OutboxMaxBytes {
		return append(lines, queueLine{queueErr, fmt.Sprintf(
			"delivery queue%s FULL (%s) — new events are being DROPPED. Delivery has failed long enough to fill the queue; see %s",
			laneSuffix(in.lane), humanizeBytes(in.size), capture.DaemonLogPath())})
	}

	// An outbox we cannot read cannot be counted. PendingCount returns 0 on any
	// read error, so without this an unreadable queue renders as "empty — every
	// captured event has shipped": a confident all-clear about a file whose
	// contents are invisible, and one where Append is almost certainly failing to
	// write too. Reported alone, because the depth is genuinely unknown and any
	// number next to it would be a guess.
	if in.haveOutbox && in.unreadable {
		return append(lines, queueLine{queueWarn, fmt.Sprintf(
			"delivery queue%s unreadable (%s at %s) — cannot tell whether events are pending, and capture is likely failing to write; check permissions on the state dir, then see %s",
			laneSuffix(in.lane), humanizeBytes(in.size), in.queuePath(), capture.DaemonLogPath())})
	}

	// Approaching the cap: warn while there is still lead time to act. Nothing is
	// being dropped yet, so this pairs cleanly with the depth line below.
	if in.haveOutbox && in.size*100 >= outbox.OutboxMaxBytes*queueNearFullPercent {
		lines = append(lines, queueLine{queueWarn, fmt.Sprintf(
			"delivery queue%s %d%% full (%s of %s) — events get DROPPED at the cap; see %s",
			laneSuffix(in.lane), in.size*100/outbox.OutboxMaxBytes, humanizeBytes(in.size),
			humanizeBytes(outbox.OutboxMaxBytes), capture.DaemonLogPath())})
	}

	switch {
	case in.pending == 0:
		lines = append(lines, queueLine{queueOK, "delivery queue empty — every captured event has shipped"})

	// Nothing is draining because nothing is watching. This is the normal idle
	// state of any machine that has captured and stopped, and it is emphatically
	// not a problem — the events ship on the next watch.
	case !in.draining:
		lines = append(lines, queueLine{queueOK, fmt.Sprintf(
			"delivery queue holds %s — nothing is draining because capture is not running; they ship on the next `promptster-teams watch`",
			eventCount(in.pending))})

	// Draining, but there is no timestamp to judge progress against. Say what we
	// know rather than guess at a verdict.
	case !in.haveProgress:
		lines = append(lines, queueLine{queueOK, fmt.Sprintf(
			"delivery queue holds %s — delivery is running", eventCount(in.pending))})

	case in.now.Sub(in.lastProgress) > queueStuckAfter:
		lines = append(lines, queueLine{queueWarn, fmt.Sprintf(
			"delivery queue%s stuck — %s pending, no delivery progress in %s. Likely a revoked key (ingest 401), an unreachable ingest endpoint, or a full or unwritable state dir; see %s",
			laneSuffix(in.lane), eventCount(in.pending), humanizeDuration(in.now.Sub(in.lastProgress)),
			capture.DaemonLogPath())})

	default:
		lines = append(lines, queueLine{queueOK, fmt.Sprintf(
			"delivery queue draining — %s pending", eventCount(in.pending))})
	}

	return lines
}

// laneSuffix renders a lane name for a doctor line, empty for the live lane.
// Live is unqualified because it is what an engineer means by "the queue"; the
// backfill lane is named because knowing WHICH one is stuck is most of the
// diagnosis.
func laneSuffix(lane string) string {
	if lane == "" || lane == outbox.LaneLive().Name {
		return ""
	}
	return " (" + lane + ")"
}

// queuePath is the file the reported lane lives in, for the unreadable message.
func (in queueInputs) queuePath() string {
	if in.lane == outbox.LaneBackfill().Name {
		return state.OutboxBackfillPath()
	}
	return state.OutboxPath()
}

// queueLaneProbe is one lane's on-disk state, before the lanes are reduced to
// the single worst reading checkQueueHealth judges.
type queueLaneProbe struct {
	lane         outbox.Lane
	name         string
	path         string
	cursor       string
	pending      int
	size         int64
	exists       bool
	unreadable   bool
	lastProgress time.Time
	haveProgress bool
}

// gatherQueueInputs reads the queue's state. Strictly read-only: doctor is a
// diagnostic and must never advance the cursor, compact the outbox, or POST.
//
// BOTH LANES, reduced to the worst reading. Measuring only live is a real blind
// spot and not a theoretical one: the backfill lane has its own file, its own
// OutboxMaxBytes ceiling and its own cursor, so it can be at the cap and
// dropping replayed events, or wedged for hours, entirely underneath a healthy
// live lane — and doctor would have printed "delivery queue draining" over the
// top of it.
func gatherQueueInputs(now time.Time, snap capture.CaptureSnapshot) queueInputs {
	in := queueInputs{
		// Already both lanes: outbox.PendingCount sums them.
		pending:  countBufferedEvents(),
		draining: watcherDraining(snap),
		now:      now,
	}
	in.dropsLive, in.dropsBackfill = outbox.DropCounts()

	probes := []queueLaneProbe{
		{
			lane:   outbox.LaneLive(),
			name:   outbox.LaneLive().Name,
			path:   state.OutboxPath(),
			cursor: state.OutboxCursorPath(),
		},
		{
			lane:   outbox.LaneBackfill(),
			name:   outbox.LaneBackfill().Name,
			path:   state.OutboxBackfillPath(),
			cursor: state.OutboxCursorPathFor(state.OutboxBackfillPath()),
		},
	}
	for i := range probes {
		probeLane(&probes[i])
	}

	// SIZE and UNREADABLE take the worse lane. Both drive "events are being lost
	// right now" verdicts, and a problem on either lane is a problem.
	for _, pr := range probes {
		if !pr.exists {
			continue
		}
		in.haveOutbox = true
		if pr.unreadable && !in.unreadable {
			// Unreadable outranks size: a lane whose contents are invisible makes
			// every other number about it a guess.
			in.unreadable = true
			in.size = pr.size
			in.lane = pr.name
			continue
		}
		if !in.unreadable && pr.size > in.size {
			in.size = pr.size
			in.lane = pr.name
		}
	}

	// PROGRESS takes the OLDEST among lanes that actually have pending work. A
	// lane with an empty queue has an old cursor for the honest reason that there
	// was nothing to advance it, and letting that decide would report every idle
	// machine as stuck.
	for _, pr := range probes {
		if !pr.haveProgress || pr.pending == 0 {
			continue
		}
		if !in.haveProgress || pr.lastProgress.Before(in.lastProgress) {
			in.lastProgress = pr.lastProgress
			in.haveProgress = true
			if !in.unreadable {
				in.lane = pr.name
			}
		}
	}
	if in.haveProgress {
		return in
	}

	// No usable cursor on any lane holding work means delivery has NEVER
	// succeeded — which is exactly what a revoked key looks like on a machine
	// that has only ever 401'd. Fall back to the watcher's start time: delivery
	// has had that long to write a cursor and has not.
	if t := latestWatcherStart(snap); !t.IsZero() {
		in.lastProgress = t
		in.haveProgress = true
	}
	return in
}

// probeLane fills one lane's on-disk readings. A missing queue is the
// fresh-install state, not a fault. Stat succeeds on a file the process cannot
// open, so readability is probed separately.
func probeLane(pr *queueLaneProbe) {
	fi, err := os.Stat(pr.path)
	if err != nil {
		return
	}
	pr.exists = true
	pr.size = fi.Size()
	pr.unreadable = !queueReadable(pr.path)
	if !pr.unreadable {
		pr.pending = outbox.PendingIn(pr.lane)
	}

	// The cursor's mtime is the progress probe: it is rewritten (temp+rename)
	// every time delivery advances. Doctor is one-shot, so a rate is not
	// observable without sleeping and sampling twice — which would make doctor
	// slow for no real gain.
	if cfi, err := os.Stat(pr.cursor); err == nil {
		pr.lastProgress = cfi.ModTime()
		pr.haveProgress = true
	}
}

// queueReadable reports whether a lane can actually be opened for reading.
// Opening and immediately closing is the same access PendingCount performs, so
// this answers "would the count be trustworthy?" without duplicating the scan.
func queueReadable(path string) bool {
	// #nosec G304 -- path comes from state.Outbox*Path(), StateDir()-derived, not user input.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// watcherDraining reports whether a live watcher — and therefore the drain loop
// it starts — is running.
//
// Deliberately NOT snap.Live. Live ORs in DaemonStatus(), which proves only that
// the PID in supervisor.json exists; it checks no heartbeat. A supervisor killed
// without a clean stop (power loss, SIGKILL) leaves that pidfile behind, and
// after a reboot the OS hands the recorded PID to some unrelated process — so
// Live reads true forever, with both watchers dead and nothing draining. Doctor
// would then tell an idle laptop its queue is stuck and blame a revoked key: the
// exact false alarm this check exists to avoid, on the most ordinary state there
// is. WatcherStat.Running requires a recent heartbeat (watcherLive), which a
// recycled PID cannot fake.
//
// The inverse — a live supervisor whose watcher goroutines have not yet written
// their first pidfile — lasts seconds and only costs a "delivery is running"
// line instead of a warning. Erring toward silence is the right bias here.
func watcherDraining(snap capture.CaptureSnapshot) bool {
	return snap.Claude.Running || snap.Codex.Running
}

// latestWatcherStart returns the newest start time among live watchers. Newest,
// not oldest: a watcher that restarted ten seconds ago has not had time to
// deliver anything, and reporting it as stuck would be a false alarm.
//
// Deliberately not CaptureSnapshot.StartedAt(), which answers a different
// question for the uptime display — it takes the earliest start and ignores
// whether the watcher is still running. Both would bias this toward crying wolf.
func latestWatcherStart(snap capture.CaptureSnapshot) time.Time {
	var t time.Time
	for _, w := range []capture.WatcherStat{snap.Claude, snap.Codex} {
		if w.Running && w.StartedAt.After(t) {
			t = w.StartedAt
		}
	}
	return t
}

// eventCount64 is eventCount for the durable drop counters, which are int64
// because they accumulate over a machine's whole life rather than describing a
// queue that has to fit on disk.
func eventCount64(n int64) string {
	if n == 1 {
		return "1 event"
	}
	return fmt.Sprintf("%d events", n)
}

func eventCount(n int) string {
	if n == 1 {
		return "1 event"
	}
	return fmt.Sprintf("%d events", n)
}
