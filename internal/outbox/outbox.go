// Package outbox is the durable send queue between the watchers' parse loops
// and the ingest endpoint.
//
// WHY IT EXISTS: the parse loop used to POST inline and advance the transcript
// offset regardless of the result, so any 429/5xx/timeout permanently dropped
// the event — there was no retry anywhere. Splitting parse from send lets the
// offset advance safely (the queue, not the network, is now the thing that
// remembers) and gives retries somewhere to live.
//
// WHY NOT buffer.jsonl: the ledger is a signed tamper-evident artifact that
// rotates (sign/rotate.go) and is written by presence/census too. See
// state.OutboxPath for the full argument. Short version: the ledger's value is
// that nothing mutates it; this queue's value is that the drain can.
//
// ORDERING: callers append AFTER sign.AppendEventToLocalBuffer, which mutates
// the event with Sig/PrevSig and has already run redaction/source-exclusion.
// The queued bytes are therefore exactly what should go on the wire, and the
// drain POSTs them verbatim — it never re-projects or re-signs.
package outbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const (
	// drainIdleInterval is how long the drain sleeps when the queue is empty.
	// Short enough that capture feels live, long enough to be free at idle.
	drainIdleInterval = 1 * time.Second

	// backoffCap bounds the exponential retry for 5xx and network failures.
	// 429s do NOT use it — they honor the server's Retry-After.
	backoffCap = 30 * time.Second

	// OutboxMaxBytes caps the queue so an indefinitely-offline laptop cannot
	// fill the disk. At ~400B/event this is ~160k events, i.e. weeks offline.
	// Reaching it DROPS new events (loudly) — see Append.
	OutboxMaxBytes = 64 << 20

	// stuckAttemptThreshold is how many consecutive failures on ONE event flip
	// logging from debug-gated to a loud warning. Sized so an ordinary transient
	// blip (a deploy, a dropped packet) clears silently, while anything that
	// survives a full backoff ramp gets surfaced.
	stuckAttemptThreshold = 5

	// stuckRepeatInterval bounds how often the stuck warning repeats, so a
	// wedged queue keeps saying so without flooding stderr.
	stuckRepeatInterval = 5 * time.Minute

	// batchProbeCooldown is how long the drain stops attempting batch delivery
	// after a backend answers "no such route" while the policy still advertises
	// one. Long enough not to hammer a rolled-back backend on every pass; short
	// enough that a redeploy is picked up without restarting the daemon.
	batchProbeCooldown = 30 * time.Minute
)

// batchMaxBodyBytes caps one batch request. The backend's own bodyLimit is 10 MB
// and its member ceiling is 500 (~400 B/event ≈ 200 KB), so this is slack for
// unusually large events rather than the binding constraint — it exists so one
// pathological event cannot build a request the backend must refuse in full.
//
// A var rather than a const so a test can shrink it: the path where it binds is
// the one that has to consume a line it cannot use, and that path shipped a
// silent event-loss bug the first time precisely because nothing exercised it.
var batchMaxBodyBytes = 4 << 20

// backoffBase is the first retry delay; it doubles from there to backoffCap.
// A var rather than a const purely so tests can shrink the ramp.
var backoffBase = 500 * time.Millisecond

// LiveHorizon is how old an event may be and still count as live.
//
// STRICTLY GREATER than the backend's 20-minute active window, and that
// inequality is the whole design. The backend calls a device idle when nothing
// has arrived inside its window; if our horizon matched it exactly, an event
// classified backfill at the boundary would be deferred past the moment it was
// needed to prove the device alive. The gap is slack in the safe direction: we
// call something live for ten minutes longer than the backend needs it.
//
// A var so tests can compress it.
var LiveHorizon = 30 * time.Minute

// Lane is one of the two queues. Two files, two cursors, two locks, two
// independent heads — the last of those is what stops a wedged replay from
// blocking a prompt typed a second ago.
type Lane struct {
	// Name appears in warnings and in the batch request's `lane` field.
	Name string
	// path/cursorPath/lockPath are functions, not strings, because the state dir
	// is resolved from the environment at call time (tests relocate it).
	path       func() string
	cursorPath func() string
	lockPath   func() string
}

// LaneLive carries work happening now. Its file name is unchanged from the
// single-queue era on purpose: a device upgrading mid-backlog keeps draining
// what it already queued instead of stranding it under a new name.
func LaneLive() Lane {
	return Lane{
		Name:       "live",
		path:       state.OutboxPath,
		cursorPath: state.OutboxCursorPath,
		lockPath:   state.OutboxLockPath,
	}
}

// LaneBackfill carries replayed history — the one producer that can genuinely
// wait, because its source bytes are already durable on disk.
func LaneBackfill() Lane {
	return Lane{
		Name:       "backfill",
		path:       state.OutboxBackfillPath,
		cursorPath: func() string { return state.OutboxCursorPathFor(state.OutboxBackfillPath()) },
		lockPath:   func() string { return state.OutboxLockPathFor(state.OutboxBackfillPath()) },
	}
}

// PressureHighWater is the queue size at which a producer that can DEFER its
// work should stop producing. A var, not a const, so a test can lower it.
//
// Half of OutboxMaxBytes leaves the other half as headroom for producers that
// cannot defer (a live tool hook has nowhere to put the event but here).
var PressureHighWater int64 = OutboxMaxBytes / 2

// UnderPressure reports whether the BACKFILL lane has grown past
// PressureHighWater.
//
// It reads the backfill lane and not the live one, which is the §1.4 change and
// is not cosmetic. Its only caller is the transcript watchers' history replay —
// the one producer whose input is ALREADY durable, so declining to read defers
// the work perfectly rather than losing it. Measuring the LIVE lane would have
// asked the wrong queue: live pressure is caused by producers that cannot defer
// at all (a tool hook has nowhere to put its event but here), so throttling
// replay on it both fails to relieve the real pressure and stalls the one thing
// that was safe to keep going. Each lane now has its own OutboxMaxBytes and its
// own loud drop, so neither can fill the other's ceiling.
//
// Cheap by construction (one Stat, no lock): it is advisory backpressure, not
// an invariant, so a stale read only costs one poll.
func UnderPressure() bool {
	fi, err := os.Stat(state.OutboxBackfillPath())
	if err != nil {
		return false
	}
	return fi.Size() >= PressureHighWater
}

// ErrQueueFull reports that an append was DISCARDED because its lane had
// already reached OutboxMaxBytes. Match it with errors.Is; AppendTo wraps it
// with the lane and the size it measured.
//
// It exists because the drop used to return nil — "success" — so no caller
// could tell a queued event from a discarded one. That silence was invisible in
// exactly the case it mattered: the transcript watchers commit their read offset
// from the bytes they consumed, so a drop marked the transcript READ while
// discarding the event derived from it. A three-day ingest wedge on one customer
// device dropped ~54,000 events that way, every one of them still sitting in a
// transcript on disk that the watcher had already recorded as fully read — a
// loss no restart, redeploy, or backend fix could recover without hand-rewinding
// the cursor.
//
// The contract this establishes is asymmetric, deliberately:
//
//   - A producer with a DURABLE SOURCE (the Claude and Codex transcript
//     watchers) must treat it as "these bytes were NOT consumed" and leave its
//     read offset short of the failing record. Re-reading the transcript next
//     poll costs nothing and normalizers derive STABLE event ids, so a re-queued
//     record collapses to one row on the backend.
//   - Every other producer (tool hooks, presence, census, durability, commit
//     attribution, window usage — see Append) has no second copy of the event.
//     For them a full queue is still a real loss and there is nothing better to
//     do than log and carry on. The ones that gate durable state on the queue
//     (commit attribution's ledger, the durability inventory throttle) simply
//     decline to mark the work done and retry it later, which is the correct
//     reading of a failed enqueue.
var ErrQueueFull = errors.New("outbox queue is full: event dropped")

// Append enqueues an already-signed, already-redacted event on the LIVE lane.
//
// This is the right call for every producer whose input is NOT durable on disk:
// tool hooks, presence, census, durability, commit attribution, window usage.
// They have nowhere else to put the event, so deferring it would lose it.
//
// It takes the lane's lock, so it is safe across the concurrent emitters and
// across processes. It never blocks on the network — that is the entire point
// of the split.
func Append(ev event.Event) error {
	return AppendTo(LaneLive(), ev)
}

// AppendFromDurableSource enqueues an event parsed from bytes that are ALREADY
// on disk, letting it be classified onto the backfill lane.
//
// Both halves of the §1.2 rule are required and each rules out a real mistake:
//
//   - DURABLE SOURCE is the caller's static property, not something inferred
//     here. Only the Claude and Codex transcript watchers can say yes — their
//     input is a file whose read offset has not advanced, so a deferred event is
//     never a lost one. Cursor never backfills. Inferring "old ⇒ deferrable"
//     instead would strand events with no second copy anywhere.
//   - OLDER THAN LiveHorizon, measured on the event's OWN timestamp. A watcher
//     tailing the live tip of a transcript is a durable source too, and its
//     events are happening now; sending those down the slow lane would defer
//     exactly the traffic that proves the device is alive.
//
// An unparseable or absent timestamp resolves to LIVE. That is the fail-safe
// direction: the cost of a wrong live is a little queue jumping, while the cost
// of a wrong backfill is work deferred behind a replay that may run for hours.
func AppendFromDurableSource(ev event.Event) error {
	return AppendTo(laneFor(ev), ev)
}

// laneFor applies the §1.2 classification to an event from a durable source.
func laneFor(ev event.Event) Lane {
	ts, err := time.Parse(time.RFC3339Nano, ev.Ts)
	if err != nil {
		return LaneLive() // unreadable age — fail toward live, see AppendFromDurableSource
	}
	if time.Since(ts) > LiveHorizon {
		return LaneBackfill()
	}
	return LaneLive()
}

// AppendTo enqueues onto a named lane. Exported so a caller that has already
// decided (a migration replay, which knows its own horizon) can say so directly.
func AppendTo(lane Lane, ev event.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return sign.WithBufferLock(lane.lockPath(), func() error {
		p := lane.path()
		// PER LANE, deliberately. Each lane gets the full OutboxMaxBytes rather
		// than sharing one, so a replay that fills its own queue cannot consume
		// the headroom live capture needs — the drop below is the failure this
		// whole package exists to avoid, and letting one lane cause it in the
		// other would undo the split.
		if fi, err := os.Stat(p); err == nil && fi.Size() >= OutboxMaxBytes {
			// Dropping is a real loss of telemetry, never a normal condition, so
			// it is reported unconditionally rather than through the debug-gated
			// logger.
			//
			// This warning used to reassure the operator that "the event still
			// exists in the signed ledger — the audit trail stays complete; only
			// the upload is lost." That was FALSE exactly when it printed. The
			// ledger is bounded too, on the same order and over the same window:
			// sign.rotateLedgerIfLarge rotates at 16 MiB and keeps
			// state.LedgerRetainedSegments (3) beside the live one — ~64 MiB
			// across 4 segments, the same 64 MiB this lane caps at. The two
			// bounds evict from OPPOSITE ends: this queue drops the NEWEST event,
			// the ledger drops the OLDEST segment. A stall long enough to fill
			// 64 MiB of queue has been filling the ledger over that same period,
			// so the event may already have rotated out of it. MAY, not has: the
			// two files do not fill at the same rate (the ledger also carries
			// presence and census; the projector strips fields on the way in), so
			// the text below claims possibility, not loss.
			warnf("%s outbox is full (%d bytes) — DROPPING event (%s). Delivery has been failing long enough to fill the queue; check connectivity and the ingest endpoint. The signed ledger is bounded too (~64 MiB across 4 segments, oldest dropped first), so a stall this long may already have rotated this event out of it.",
				lane.Name, fi.Size(), ev.Kind)
			return fmt.Errorf("%s lane at %d bytes: %w", lane.Name, fi.Size(), ErrQueueFull)
		}
		// #nosec G304 -- p is lane.path(), derived from state.StateDir(), not user input.
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(b, '\n'))
		return err
	})
}

// warnOut is where warnf writes. A var so tests can capture it; nothing else
// should reassign it.
//
// warnMu guards it because the two lane goroutines both warn. os.Stderr would
// survive unsynchronized (one write syscall), but the capture buffer tests
// substitute would not — an interleaved write there is a data race and a
// corrupted assertion, and the point of these warnings is that an operator can
// read them.
var (
	warnMu  sync.Mutex
	warnOut io.Writer = os.Stderr
)

// warnf reports a delivery problem. Deliberately NOT debug-gated: a queue that
// silently stops draining is indistinguishable from an idle one, which is the
// exact failure this package exists to prevent.
func warnf(format string, args ...interface{}) {
	warnMu.Lock()
	defer warnMu.Unlock()
	fmt.Fprintf(warnOut, "promptster-teams: outbox: "+format+"\n", args...)
}

// --- cursor ------------------------------------------------------------------

// readCursor returns the byte offset of the next undelivered event.
//
// A missing cursor means a FRESH INSTALL and MUST read 0, because the outbox
// itself is also fresh (it is a new file introduced with this queue). This is
// the load-bearing difference from draining buffer.jsonl: that ledger holds
// thousands of already-delivered events, and starting a drain over it at offset
// 0 would replay the entire backlog and cause the very 429 storm this change
// removes. The outbox has no history to replay, so 0 is trivially safe.
func readCursor(lane Lane) int64 {
	data, err := os.ReadFile(lane.cursorPath())
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeCursor commits the offset. A var so tests can inject IO faults; nothing
// else should reassign it. Reassigned only in tests, but read from BOTH lane
// goroutines — a substitute must be safe for concurrent use.
var writeCursor = writeCursorFile

// writeCursorFile commits the offset via temp+rename so a crash mid-write cannot
// leave a half-written number that parses as a smaller offset (which would
// re-send) or a larger one (which would skip).
//
// The temp file is per-lane too (it is derived from the lane's cursor path), so
// the two lanes' commits cannot rename over each other.
func writeCursorFile(lane Lane, n int64) error {
	p := lane.cursorPath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n, 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// PendingCount returns how many queued events are still undelivered (lines
// past the cursor). This is the honest answer to "events pending upload".
//
// The status UI previously derived that number from the LEDGER, which nothing
// drains — so it counted every event ever captured and reported them all as
// perpetually "pending", while "all events shipped" could only appear on a
// device that had captured nothing. Counting the queue makes both states mean
// what they say.
//
// BOTH LANES, summed. "Events pending upload" is a device-level question and the
// engineer does not have two outboxes — reporting only live would show a caught-
// up device with 60,000 replayed events still queued behind it, which is the
// misreport this whole change exists to remove.
func PendingCount() int {
	return pendingCountIn(LaneLive()) + pendingCountIn(LaneBackfill())
}

// PendingIn reports one lane's undelivered depth. Exported for doctor, whose
// stuck verdict is PER LANE — a live lane delivering every second must not paper
// over a backfill lane that has not advanced in an hour. PendingCount answers
// the device-level question and sums them; this answers "which lane".
func PendingIn(lane Lane) int { return pendingCountIn(lane) }

func pendingCountIn(lane Lane) int {
	cursor := readCursor(lane)
	// #nosec G304 -- lane.path() is StateDir()-derived, not user input.
	f, err := os.Open(lane.path())
	if err != nil {
		return 0
	}
	defer f.Close()
	if _, err := f.Seek(cursor, 0); err != nil {
		return 0
	}
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) > 0 {
			n++
		}
	}
	return n
}

// PendingState is what the liveness beat reports about this device's backlog.
//
// Count alone is not enough and that is the whole point. 62,000 events queued
// from the last five minutes is a busy engineer; 62,000 queued whose oldest is
// dated three weeks ago is an outage. On 2026-08-04 the backend could see
// neither, so it reported an actively-working engineer as having zero active
// sessions for over an hour while ingest ran at 100% 2xx.
type PendingState struct {
	// Undelivered lines past the cursor. 0 is a MEASUREMENT ("caught up"), and
	// the backend stores it as one — distinct from never having been told.
	Count int
	// Event timestamp of the OLDEST undelivered event, zero when the queue is
	// empty. This is the lag, and the lag is what makes the count interpretable.
	Oldest time.Time
}

// PendingStateNow scans the undelivered tail once and reports both numbers.
//
// The MINIMUM `ts` across pending lines, deliberately NOT the first line's.
// Append order stopped being chronological the moment history replay started
// running newest-first (cmd_codex_watch.go / cmd_claude_watch.go): during a
// backfill the head of the queue is recent work and the three-week-old events
// are behind it. Taking the head's timestamp would report the lag as minutes
// during exactly the episode the field exists to describe.
//
// A malformed or unparseable line is COUNTED but contributes no timestamp: it
// is still undelivered work, and dropping it from the count would understate a
// real backlog. Cost is one pass over the tail, run once per beat (5 min).
//
// CONCURRENCY. This runs on the presence goroutine while the drain advances the
// cursor, compacts, and the watchers append — none of it under a lock. Two
// windows exist and they are not equally acceptable:
//
//   - The cursor advances between our read and our scan, so we count a handful
//     of lines the drain has since delivered. Bounded by the drain rate over a
//     sub-second scan (~1 event) and it OVERSTATES, which at worst shows a beat
//     of backlog that has already cleared. Left unsynchronized deliberately.
//   - The queue COMPACTS between our read and our seek: compact() truncates to
//     zero and resets the cursor, the watchers append fresh events, and our
//     stale cursor now points past EOF. Seeking there succeeds and reads
//     nothing, so we would report a MEASURED ZERO — "this device is caught up" —
//     while real work sat queued. That is the exact lie this whole field exists
//     to prevent, so it is handled: a cursor past EOF means the queue was
//     compacted out from under us and the only correct reading is from the
//     start, the same conclusion drainOnce reaches at the same fork. Unlike
//     drainOnce we do NOT write the rewound cursor back — a reader must never
//     mutate delivery state; the drain will correct it on its own next pass.
//
// BOTH LANES. Count sums; Oldest is the minimum across them, which is almost
// always the backfill lane's head — and that is precisely the number the beat
// exists to carry. Reporting live alone would answer "is this engineer working"
// correctly and "is this device caught up" backwards.
func PendingStateNow() PendingState {
	live := pendingStateIn(LaneLive())
	back := pendingStateIn(LaneBackfill())
	out := PendingState{Count: live.Count + back.Count, Oldest: live.Oldest}
	if out.Oldest.IsZero() || (!back.Oldest.IsZero() && back.Oldest.Before(out.Oldest)) {
		out.Oldest = back.Oldest
	}
	return out
}

func pendingStateIn(lane Lane) PendingState {
	cursor := readCursor(lane)
	// #nosec G304 -- lane.path() is StateDir()-derived, not user input.
	f, err := os.Open(lane.path())
	if err != nil {
		return PendingState{}
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && cursor > fi.Size() {
		cursor = 0
	}
	if _, err := f.Seek(cursor, 0); err != nil {
		return PendingState{}
	}
	out := PendingState{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if len(line) == 0 {
			continue
		}
		out.Count++
		var probe struct {
			TS string `json:"ts"`
		}
		if json.Unmarshal([]byte(line), &probe) != nil || probe.TS == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, probe.TS)
		if err != nil {
			continue
		}
		if out.Oldest.IsZero() || ts.Before(out.Oldest) {
			out.Oldest = ts
		}
	}
	return out
}

// --- drain -------------------------------------------------------------------

var startOnce sync.Once

// BatchCapability answers whether the backend can take a batch right now, and
// on what terms. Its shape is exactly policy.Resolver.BatchIngest's, so a caller
// passes that method value directly and this package stays free of a dependency
// on policy.
//
// Consulted once per chunk rather than once per process: the resolver refreshes
// in the background, so a backend that gains or loses the route is picked up
// without restarting the daemon. A nil BatchCapability means per-event delivery,
// which is what every caller that has not been taught about batching gets.
type BatchCapability func() (endpoint string, maxSize int, ok bool)

// batchSuppressedUntil latches batch delivery OFF after a backend answered "no
// such route" to one. Guarded by its own mutex because the drain is one
// goroutine but tests reach in.
//
// This exists because the two sources of truth can disagree: the policy says the
// route is there (fetched up to 10 minutes ago, or adopted from disk at startup)
// while the backend in front of us has rolled back and 404s. Believing the
// policy on every pass would mean one wasted round-trip per chunk, forever. The
// latch is a COOLDOWN and not a permanent kill so a redeploy recovers on its own
// — an unrecoverable-until-restart optimisation is how a fleet quietly stays on
// the slow path for weeks.
var (
	batchSuppressMu    sync.Mutex
	batchSuppressUntil time.Time
)

func batchSuppressed(now time.Time) bool {
	batchSuppressMu.Lock()
	defer batchSuppressMu.Unlock()
	return now.Before(batchSuppressUntil)
}

func suppressBatch(now time.Time) {
	batchSuppressMu.Lock()
	defer batchSuppressMu.Unlock()
	batchSuppressUntil = now.Add(batchProbeCooldown)
}

// resolveBatch collapses the capability and the latch into the one question
// drainOnce asks: batch this chunk, or send it an event at a time?
func resolveBatch(caps BatchCapability) (endpoint string, maxSize int, ok bool) {
	if caps == nil || batchSuppressed(time.Now()) {
		return "", 0, false
	}
	return caps()
}

// StartDrain launches the drain in the background, AT MOST ONCE PER PROCESS.
//
// The singleton is load-bearing, not defensive. The supervisor runs the claude
// and codex watchers as goroutines in ONE process (capture/teams.go), and both
// feed this one device-wide queue. If each started its own drain, the two would
// read the same cursor, POST the same event, and both advance — re-creating the
// exact duplicate-emission bug this change exists to remove, but on the send
// side. One queue, one drain.
//
// It is deliberately tied to process lifetime rather than to a caller's
// context: whichever watcher happens to start first must not own delivery for
// the others, and a watcher exiting must not silently stop the queue. Tests
// call Drain directly for a cancellable, blocking drain.
// caps may be nil, which means per-event delivery.
func StartDrain(client *http.Client, apiKey string, caps BatchCapability) {
	startOnce.Do(func() {
		go Drain(context.Background(), client, apiKey, caps)
	})
}

// Drain delivers queued events in order until ctx is cancelled. Prefer
// StartDrain outside tests — a second concurrent Drain would double-send.
//
// Delivery rules, per event:
//
//	2xx                  -> advance cursor
//	400/422 (rejection)  -> advance cursor (permanently unsendable; debug-log)
//	429                  -> honor Retry-After, retry the SAME event
//	5xx / network / t.o. -> exponential backoff + jitter, retry the SAME event
//
// Only 2xx and 400/422 advance. Everything else retries in place forever, which
// is correct: the queue is bounded by OutboxMaxBytes, not by giving up.
//
// When caps reports batch support the same rules apply PER MEMBER, read off the
// backend's 207 — see deliverChunk. The rules do not change with the transport;
// only how many events one round-trip carries.
// The two lanes drain CONCURRENTLY, one goroutine each, and that is the §1.3
// decision. The obvious alternative — one goroutine round-robinning "live to
// empty, then a bounded backfill slice" — cannot actually meet the acceptance
// this change is for ("backfill head wedged on a permanent 5xx, live throughput
// unaffected"). A single goroutine sitting in backfill's retry backoff is a
// goroutine not delivering live, so the best it can do is bound the damage to
// one slice's worth of latency; and bounding the retry means abandoning the
// never-give-up rule that keeps the queue honest. Two goroutines make live
// genuinely independent, and the backend already budgets the lanes separately
// (live 100/min, backfill 60/min) — a budget per lane only makes sense if the
// lanes can be in flight at once.
//
// The singleton in StartDrain still holds. What it protects against is TWO
// DRAINS ON ONE QUEUE — same cursor, same events, both advancing. These two
// share no queue, no cursor and no lock; they share the HTTP client (safe) and
// the batch-suppression latch (mutex-guarded, and a property of the backend
// rather than of either lane).
func Drain(ctx context.Context, client *http.Client, apiKey string, caps BatchCapability) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		drainLane(ctx, client, apiKey, caps, LaneBackfill())
	}()
	drainLane(ctx, client, apiKey, caps, LaneLive())
	wg.Wait()
}

// drainLane runs one lane's delivery loop until ctx is cancelled.
func drainLane(ctx context.Context, client *http.Client, apiKey string, caps BatchCapability, lane Lane) {
	failures := 0
	var lastWarn time.Time
	for {
		n, err := drainOnce(ctx, client, apiKey, caps, lane)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// A drainOnce error is a LOCAL fault — in practice the cursor write
			// failing (disk full, read-only FS). It MUST back off.
			//
			// The durable cursor still names the last event we managed to
			// record, so the next pass re-reads and re-POSTs everything
			// delivered since — events the backend already accepted. Looping
			// straight back therefore re-sends accepted events indefinitely:
			// the duplicate-send bug this whole change removes, reintroduced on
			// a local-IO fault. Backing off bounds that to one duplicate per
			// (capped, jittered) cycle instead of one per second, and never
			// takes the `n > 0` fast path below, which would skip the sleep and
			// spin. Deliberately no give-up: the cursor may yet become
			// writable, and dropping capture over a transient disk blip is
			// worse than a bounded duplicate the backend already dedupes.
			failures++
			if failures == 1 || time.Since(lastWarn) >= stuckRepeatInterval {
				warnf("cannot record %s delivery progress (%d consecutive failure(s)): %v — "+
					"events are captured and queued, but %s cannot be written, so already-delivered "+
					"events may be re-sent until this clears. Check free disk space and that the "+
					"state directory is writable.",
					lane.Name, failures, err, lane.cursorPath())
				lastWarn = time.Now()
			}
			if !sleepCtx(ctx, backoffFor(failures-1)) {
				return
			}
			continue
		}
		failures = 0
		// Only idle when the queue is empty; a non-empty queue loops straight
		// back so a backlog drains at line rate rather than one event/second.
		if n == 0 {
			if !sleepCtx(ctx, drainIdleInterval) {
				return
			}
		}
	}
}

// drainOnce delivers every event currently queued past the cursor, then
// compacts. Returns how many events it delivered (or permanently skipped).
func drainOnce(ctx context.Context, client *http.Client, apiKey string, caps BatchCapability, lane Lane) (int, error) {
	cursor := readCursor(lane)
	p := lane.path()

	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // nothing queued yet
		}
		return 0, err
	}
	// A cursor past EOF means the queue was compacted or truncated out from
	// under us (e.g. a second drain, or a user clearing state). Rewind rather
	// than stall forever — re-reading a compacted queue is empty by definition.
	if cursor > fi.Size() {
		cursor = 0
		if err := writeCursor(lane, 0); err != nil {
			return 0, err
		}
	}
	if cursor == fi.Size() {
		compact(lane, cursor)
		return 0, nil
	}

	// #nosec G304 -- p is lane.path(), derived from state.StateDir(), not user input.
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(cursor, 0); err != nil {
		return 0, err
	}

	reader := bufio.NewReader(f)
	delivered := 0

	// Batch path. One round-trip carries up to maxSize events; the cursor still
	// advances only over members the backend has answered for.
	if endpoint, maxSize, ok := resolveBatch(caps); ok {
		for {
			if ctx.Err() != nil {
				return delivered, nil
			}
			// Re-asked every chunk, because deliverChunk's fallback latches batch
			// OFF and the rest of this pass must then run on the per-event loop
			// below. Without this the pass would spend one wasted round-trip per
			// chunk re-discovering a route the backend has already said is absent.
			if _, _, stillBatching := resolveBatch(caps); !stillBatching {
				break
			}
			chunk, desynced := readChunk(reader, maxSize)
			if len(chunk) == 0 {
				break // queue drained, or a partial trailing line — next pass
			}
			advance, count, fellBack := deliverChunk(ctx, client, apiKey, endpoint, lane, chunk)
			if fellBack {
				// The backend cannot answer a batch. Deliver THIS chunk one event
				// at a time rather than dropping it or re-reading it: the bytes are
				// already in hand and the per-event path works against every
				// backend. This is the boundary the acceptance names, and nothing
				// crosses it unsent.
				for _, ln := range chunk {
					if ctx.Err() != nil {
						return delivered, nil
					}
					if ln.body != nil && !deliver(ctx, client, apiKey, ln.body) {
						return delivered, nil // ctx cancelled mid-retry
					}
					cursor += ln.size
					if err := writeCursor(lane, cursor); err != nil {
						return delivered, fmt.Errorf("persist cursor: %w", err)
					}
					delivered++
				}
				continue
			}
			delivered += count
			if advance > 0 {
				cursor += advance
				if err := writeCursor(lane, cursor); err != nil {
					// Same reasoning as the per-event path below: without a durable
					// cursor every further send is one we cannot prove.
					return delivered, fmt.Errorf("persist cursor: %w", err)
				}
			}
			if advance < chunkBytes(chunk) {
				// Some member was neither accepted nor rejected (403, 500, or an
				// index the backend did not answer for), or ctx was cancelled. The
				// cursor now points AT it. End the pass so the next one re-reads
				// from there with that member at the head, where deliverChunk's
				// backoff applies to it directly.
				return delivered, nil
			}
			if desynced {
				// readChunk consumed a line it did not hand us, so this reader can
				// no longer be trusted to sit where the cursor does. The cursor
				// itself is correct and durable; ending the pass reopens the file
				// there. See readChunk.
				return delivered, nil
			}
		}
		// Falls through to the per-event loop, which shares this reader and this
		// cursor. Normally it finds the queue exhausted and goes straight to
		// compact; after a fallback it delivers whatever is left of the pass.
	}

	for {
		if ctx.Err() != nil {
			return delivered, nil
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break // partial trailing line — an append is mid-flight; next pass
		}
		if !deliver(ctx, client, apiKey, line) {
			return delivered, nil // ctx cancelled mid-retry; cursor stays put
		}
		cursor += int64(len(line))
		if err := writeCursor(lane, cursor); err != nil {
			// Stop this pass immediately: without a durable cursor, every event
			// we send from here on is one we cannot prove we sent, and a restart
			// would re-send the lot. Drain treats this as a local fault and backs
			// off (it must — see the comment there); returning is not enough on
			// its own, because the caller would otherwise loop straight back and
			// re-POST this same, already-accepted event at line rate.
			return delivered, fmt.Errorf("persist cursor: %w", err)
		}
		delivered++
	}
	compact(lane, cursor)
	return delivered, nil
}

// --- batch delivery ----------------------------------------------------------

// chunkLine is one line read from the queue, kept alongside the exact number of
// bytes it occupies there.
//
// size is tracked separately from len(body) and that is load-bearing: body is
// TRIMMED (it is the signed payload, and trailing whitespace was never signed)
// while the cursor must advance by the untrimmed length including the newline.
// Deriving one from the other would drift the cursor by a byte per line and
// re-send the tail of the queue forever.
type chunkLine struct {
	size int64
	// body is the trimmed bytes to send, or nil for a line that must never be
	// sent — blank, or unparseable. Those still occupy queue bytes and are still
	// advanced past; they are simply not the backend's problem.
	body []byte
}

// chunkBytes is how far the cursor moves if every line in the chunk clears.
func chunkBytes(chunk []chunkLine) int64 {
	var n int64
	for _, ln := range chunk {
		n += ln.size
	}
	return n
}

// readChunk reads up to maxSize SENDABLE lines, plus any unsendable ones
// interleaved among them.
//
// It stops early on a partial trailing line (an append is mid-flight — same rule
// as the per-event loop) and on batchMaxBodyBytes. One event larger than that
// cap is still admitted when it would otherwise be the only member, because a
// chunk of zero sendable members makes no progress and the queue would wedge on
// it permanently — the same reasoning that makes the backend's budget admit an
// oversized batch against an empty window.
// It returns `desynced` when it consumed a line it did not include — the byte
// cap is the only way that happens. bufio cannot put a whole line back, so the
// reader is then positioned PAST a line the cursor has not accounted for, and
// the caller MUST end the pass rather than keep reading: the next chunk would
// otherwise start after the dropped line, so its bytes would be added to a
// cursor that never covered the dropped one, leaving the cursor misaligned in
// the middle of a later line. That loses the skipped event outright and then
// truncates its neighbour. Ending the pass reopens the file at the cursor, where
// the skipped line is the head of a fresh chunk and `sendable == 0` admits it
// alone. Caught in review on #152; the first version returned silently and the
// comment claimed a re-read that could not happen.
func readChunk(reader *bufio.Reader, maxSize int) (chunk []chunkLine, desynced bool) {
	chunk = make([]chunkLine, 0, maxSize)
	sendable := 0
	var bodyBytes int

	for sendable < maxSize {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break
		}
		ln := chunkLine{size: int64(len(line))}
		body := []byte(strings.TrimSpace(string(line)))

		switch {
		case len(body) == 0:
			// Blank line: nothing to send, advance past it.
		case !json.Valid(body):
			// An unparseable queue line can never become sendable. Skipping it is
			// the only way to avoid wedging the queue head forever — and in a
			// batch it would poison every member alongside it, since the backend
			// rejects a malformed ENVELOPE wholesale.
			warnf("skipping unparseable queued line")
		default:
			if sendable > 0 && bodyBytes+len(body) > batchMaxBodyBytes {
				return chunk, true
			}
			ln.body = body
			bodyBytes += len(body)
			sendable++
		}
		chunk = append(chunk, ln)
	}
	return chunk, false
}

// memberAdvanceable reports whether a per-member status proves the member is
// done with, in either direction.
//
// Exactly the per-event rule from Drain's doc, applied per member: 2xx stored or
// already-known, 400/422 permanently unsendable. A 403 (device pubkey refused)
// and a 5xx are NOT advanceable — they are the same "retry in place forever"
// case single delivery gives them, and treating either as done would silently
// drop events on a key rotation or a backend fault.
func memberAdvanceable(status int) bool {
	if status >= 200 && status < 300 {
		return true
	}
	return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
}

// advanceOver walks the chunk in order against the backend's per-member results
// and returns how far the cursor may move, plus how many lines that covers.
//
// It stops at the FIRST member it cannot prove is done, and the prefix rule is
// forced by the medium: the cursor is a single byte offset, so there is no way
// to record "members 4-10 landed but 3 did not". Members after a stuck one are
// therefore re-sent on the next pass, which per-member idempotency on the
// backend makes free.
//
// A member with no result row is treated as unproven rather than as accepted.
// A truncated or reordered results array is a backend we do not understand, and
// the only safe reading of silence is that the event did not land.
func advanceOver(chunk []chunkLine, results []ingest.BatchMemberResult) (advance int64, lines int) {
	byIndex := make(map[int]ingest.BatchMemberResult, len(results))
	for _, r := range results {
		byIndex[r.Index] = r
	}

	member := 0
	for _, ln := range chunk {
		if ln.body == nil {
			advance += ln.size
			lines++
			continue
		}
		r, ok := byIndex[member]
		if !ok || !memberAdvanceable(r.Status) {
			return advance, lines
		}
		member++
		advance += ln.size
		lines++
	}
	return advance, lines
}

// deliverChunk delivers one chunk as a single batch request.
//
// Returns how many bytes the cursor may advance, how many lines that covers, and
// whether the caller must fall back to per-event delivery for this same chunk.
//
// It returns the MOMENT it has any provable progress rather than retrying until
// the chunk fully clears. That is deliberate: progress the caller has not been
// told about is progress the cursor has not recorded, and a kill in that window
// re-sends every member of the chunk. Returning early bounds the duplicate
// window to the members genuinely in flight.
//
// With zero progress it retries in place — the head member is a 403 or a 5xx, or
// the whole request failed — under the same backoff and the same never-give-up
// rule as single delivery.
func deliverChunk(
	ctx context.Context,
	client *http.Client,
	apiKey string,
	endpoint string,
	lane Lane,
	chunk []chunkLine,
) (advance int64, lines int, fellBack bool) {
	bodies := make([][]byte, 0, len(chunk))
	for _, ln := range chunk {
		if ln.body != nil {
			bodies = append(bodies, ln.body)
		}
	}
	if len(bodies) == 0 {
		// Every line was blank or unparseable. There is nothing to ask the
		// backend, and advancing is the whole answer.
		return chunkBytes(chunk), len(chunk), false
	}

	attempt := 0
	started := time.Now()
	var lastWarn time.Time
	for {
		results, err := ingest.IngestBatchWithClient(client, endpoint, bodies, apiKey, lane.Name)

		switch {
		case err == nil:
			adv, n := advanceOver(chunk, results)
			if adv > 0 {
				if attempt >= stuckAttemptThreshold {
					warnf("recovered — delivered %d event(s) after %d attempt(s) over %s",
						n, attempt+1, time.Since(started).Round(time.Second))
				}
				return adv, n, false
			}
			// The head member was answered for, and the answer was neither
			// acceptance nor rejection (403, 500, or no row at all). Nothing can
			// advance past it, so this is the retry-in-place case.

		case ingest.IsBatchUnsupported(err):
			// The policy advertises a route this backend does not serve — a
			// rollback, a proxy, or a stale cached capability. Stop asking for a
			// while and deliver the chunk one event at a time. NOT a failure:
			// per-event is the contract every backend honours.
			suppressBatch(time.Now())
			state.HookDebugf("outbox: backend does not support batch ingest, "+
				"falling back to per-event delivery for %s: %v", batchProbeCooldown, err)
			return 0, 0, true

		case ingest.IsIngestRejection(err):
			// A 400 on the ENVELOPE, not on a member — the backend could not read
			// the batch at all, so no member has an answer. Retrying the same
			// bytes cannot help, and skipping the chunk would DROP every event in
			// it. Falling back sends each event on the path that gives it its own
			// verdict, so a genuinely bad event is skipped alone and the rest land.
			suppressBatch(time.Now())
			warnf("backend refused the batch envelope (%v) — falling back to per-event "+
				"delivery for %s; no events were dropped.", err, batchProbeCooldown)
			return 0, 0, true
		}

		var wait time.Duration
		if retryAfter, limited := ingest.IsRateLimited(err); limited {
			wait = retryAfter
			if wait <= 0 {
				wait = backoffFor(attempt)
			}
		} else {
			wait = backoffFor(attempt)
		}
		attempt++

		if attempt >= stuckAttemptThreshold && time.Since(lastWarn) >= stuckRepeatInterval {
			warnf("STUCK on a %d-event batch for %s (%d attempts): %v — events are being captured "+
				"and queued but are NOT reaching the backend. Check that this device's engineer key "+
				"is still valid and the API is reachable; %d event(s) are waiting.",
				len(bodies), time.Since(started).Round(time.Second), attempt, err, PendingCount())
			lastWarn = time.Now()
		} else {
			state.HookDebugf("outbox: batch delivery failed (%d events), retrying in %s: %v",
				len(bodies), wait, err)
		}
		if !sleepCtx(ctx, wait) {
			return 0, 0, false
		}
	}
}

// deliver POSTs one queued line, retrying per the rules in Drain's doc. Returns
// false only when ctx was cancelled (caller must NOT advance the cursor).
//
// The line is shipped VERBATIM — the bytes are the ones that were signed, and
// re-marshalling them could change the canonical form the backend verifies
// against (see ingest.IngestRawEventWithClient). The event is parsed only to
// name its kind in log lines.
func deliver(ctx context.Context, client *http.Client, apiKey string, line []byte) bool {
	body := []byte(strings.TrimSpace(string(line)))
	if len(body) == 0 {
		return true // blank line: nothing to send, advance past it
	}
	var meta struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		// An unparseable queue line can never become sendable; skipping it is
		// the only way to avoid wedging the queue head forever.
		warnf("skipping unparseable queued line: %v", err)
		return true
	}

	attempt := 0
	started := time.Now()
	var lastWarn time.Time
	for {
		err := ingest.IngestRawEventWithClient(client, body, apiKey)
		if err == nil {
			// Say so if we previously complained, or the operator is left with a
			// scary warning and no idea it cleared.
			if attempt >= stuckAttemptThreshold {
				warnf("recovered — delivered %s after %d attempt(s) over %s",
					meta.Kind, attempt+1, time.Since(started).Round(time.Second))
			}
			return true
		}
		// 400/422: the backend refuses this event's shape or kind (e.g. a new
		// kind an older backend doesn't accept). Retrying can never help, and
		// the channel itself is healthy — skip it.
		if ingest.IsIngestRejection(err) {
			state.HookDebugf("outbox: event rejected by backend (%s): %v", meta.Kind, err)
			return true
		}
		var wait time.Duration
		if retryAfter, limited := ingest.IsRateLimited(err); limited {
			// Honor the server's own number. The absence of any backoff at all
			// is what made the 429 storm self-sustaining.
			wait = retryAfter
			if wait <= 0 {
				wait = backoffFor(attempt)
			}
		} else {
			wait = backoffFor(attempt)
		}
		attempt++

		// Escalate a persistently failing head-of-queue from debug to LOUD.
		//
		// Retrying forever is correct — the queue is bounded by OutboxMaxBytes,
		// not by giving up — but it must never be silent. Anything that is not
		// 2xx/400/422 retries indefinitely, which includes permanent conditions
		// this loop cannot distinguish from transient ones: a revoked engineer
		// key (401/403 — see DELETE /v1/team/engineers/:userId), a wrong API
		// URL, a backend 500ing on one poison event. Debug-gated logging would
		// hide all of them until the queue filled weeks later and started
		// dropping — silent capture loss, the exact failure class this package
		// exists to remove. So: no status special-casing, just "this event has
		// not moved in a while, here is the error".
		if attempt >= stuckAttemptThreshold && time.Since(lastWarn) >= stuckRepeatInterval {
			warnf("STUCK on %s for %s (%d attempts): %v — events are being captured and queued but are NOT reaching the backend. "+
				"Check that this device's engineer key is still valid and the API is reachable; %d event(s) are waiting.",
				meta.Kind, time.Since(started).Round(time.Second), attempt, err, PendingCount())
			lastWarn = time.Now()
		} else {
			state.HookDebugf("outbox: delivery failed (%s), retrying in %s: %v", meta.Kind, wait, err)
		}
		if !sleepCtx(ctx, wait) {
			return false
		}
	}
}

// backoffJitter spreads a computed backoff ceiling over (0, d]. Full jitter, so
// the returned delay is a random DRAW bounded by d, not d itself.
//
// A var rather than an inline call so a test can pin the ramp. That is not
// cosmetic: a test asserting "the delay exceeded X" against a uniform draw is
// flaky by construction, and this one was — it failed ~1 run in 37 by dice
// alone, which is frequent enough to train everyone to wave off a red CI.
// Substitute an identity here and the schedule becomes the exact ramp.
//
// Never 0 — a zero wait would busy-loop.
var backoffJitter = func(d time.Duration) time.Duration {
	return time.Duration(rand.Int63n(int64(d))) + 1 // #nosec G404 -- jitter, not security
}

// backoffFor returns an exponentially-growing delay with full jitter, capped at
// backoffCap. Jitter matters here: every watcher on every machine in an org
// backs off against the SAME endpoint, and a deterministic schedule would
// re-synchronize them into the retry storm we are trying to break up.
func backoffFor(attempt int) time.Duration {
	d := backoffBase << min(attempt, 16) // cheap guard against shift overflow
	if d > backoffCap || d <= 0 {
		d = backoffCap
	}
	return backoffJitter(d)
}

// compact resets a fully-delivered queue to empty so it does not grow forever.
//
// Safe because the outbox is NOT the audit ledger: it carries no signature
// chain, so truncating delivered events destroys nothing auditable (buffer.jsonl
// retains them). Guarded by the same lock appends take, and re-checks the size
// under that lock — an append that landed since the caller's read makes size >
// cursor, and compacting then would discard an undelivered event.
func compact(lane Lane, cursor int64) {
	if cursor <= 0 {
		return
	}
	err := sign.WithBufferLock(lane.lockPath(), func() error {
		fi, err := os.Stat(lane.path())
		if err != nil {
			return nil //nolint:nilerr // nothing to compact
		}
		if fi.Size() != cursor {
			return nil // raced with an append — leave it for the next pass
		}
		if err := os.Truncate(lane.path(), 0); err != nil {
			return err
		}
		return writeCursor(lane, 0)
	})
	if err != nil {
		warnf("%s compaction failed (queue will keep growing until it succeeds): %v", lane.Name, err)
	}
}

// sleepCtx sleeps for d, returning false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
