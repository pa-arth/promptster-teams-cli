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

	// lastProgress is when delivery last made progress: the cursor's mtime, or
	// the newest live watcher's start time when no cursor exists at all.
	lastProgress time.Time
	haveProgress bool

	// draining reports that a live watcher is up, and with it the drain loop.
	// StartDrain is a process-wide singleton that both watchers call, so either
	// one being alive means the queue is being worked.
	draining bool

	now time.Time
}

// checkQueueHealth turns a queue snapshot into doctor lines. Normally that is a
// single line describing the backlog and whether it is moving; a queue nearing
// the cap adds a size line above it, and a queue at the cap replaces it. Nothing
// is reported as a problem unless it actually is one.
func checkQueueHealth(in queueInputs) []queueLine {
	var lines []queueLine

	// A full outbox dominates and is reported alone. Append drops on FILE SIZE,
	// not on backlog depth, so a queue that has drained but not yet compacted is
	// still at the cap and still dropping every new event — meaning "FULL" and a
	// depth of zero are both true at once. Printed side by side they read as a
	// contradiction ("queue FULL" / "queue empty"), so the depth line is dropped
	// here: when events are being lost right now, that is the only message.
	if in.haveOutbox && in.size >= outbox.OutboxMaxBytes {
		return []queueLine{{queueErr, fmt.Sprintf(
			"delivery queue FULL (%s) — new events are being DROPPED. Delivery has failed long enough to fill the queue; see %s",
			humanizeBytes(in.size), capture.DaemonLogPath())}}
	}

	// Approaching the cap: warn while there is still lead time to act. Nothing is
	// being dropped yet, so this pairs cleanly with the depth line below.
	if in.haveOutbox && in.size*100 >= outbox.OutboxMaxBytes*queueNearFullPercent {
		lines = append(lines, queueLine{queueWarn, fmt.Sprintf(
			"delivery queue %d%% full (%s of %s) — events get DROPPED at the cap; see %s",
			in.size*100/outbox.OutboxMaxBytes, humanizeBytes(in.size),
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
			"delivery queue stuck — %s pending, no delivery progress in %s. Likely a revoked key (ingest 401), an unreachable ingest endpoint, or a full or unwritable state dir; see %s",
			eventCount(in.pending), humanizeDuration(in.now.Sub(in.lastProgress)), capture.DaemonLogPath())})

	default:
		lines = append(lines, queueLine{queueOK, fmt.Sprintf(
			"delivery queue draining — %s pending", eventCount(in.pending))})
	}

	return lines
}

// gatherQueueInputs reads the queue's state. Strictly read-only: doctor is a
// diagnostic and must never advance the cursor, compact the outbox, or POST.
func gatherQueueInputs(now time.Time, snap capture.CaptureSnapshot) queueInputs {
	in := queueInputs{
		pending:  countBufferedEvents(),
		draining: watcherDraining(snap),
		now:      now,
	}

	// A missing outbox is the fresh-install state, not a fault.
	if fi, err := os.Stat(state.OutboxPath()); err == nil {
		in.haveOutbox = true
		in.size = fi.Size()
	}

	// The cursor's mtime is the progress probe: it is rewritten (temp+rename)
	// every time delivery advances. Doctor is one-shot, so a rate is not
	// observable without sleeping and sampling twice — which would make doctor
	// slow for no real gain.
	if fi, err := os.Stat(state.OutboxCursorPath()); err == nil {
		in.lastProgress = fi.ModTime()
		in.haveProgress = true
		return in
	}

	// No cursor at all means delivery has NEVER succeeded — which is exactly what
	// a revoked key looks like on a machine that has only ever 401'd. Fall back to
	// the watcher's start time: delivery has had that long to write a cursor and
	// has not.
	if t := latestWatcherStart(snap); !t.IsZero() {
		in.lastProgress = t
		in.haveProgress = true
	}
	return in
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

func eventCount(n int) string {
	if n == 1 {
		return "1 event"
	}
	return fmt.Sprintf("%d events", n)
}
