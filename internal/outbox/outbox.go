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
)

// backoffBase is the first retry delay; it doubles from there to backoffCap.
// A var rather than a const purely so tests can shrink the ramp.
var backoffBase = 500 * time.Millisecond

// Append enqueues an already-signed, already-redacted event for delivery.
//
// It takes the outbox lock, so it is safe across the four concurrent emitters
// and across processes. It never blocks on the network — that is the entire
// point of the split.
func Append(ev event.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return sign.WithBufferLock(state.OutboxLockPath(), func() error {
		p := state.OutboxPath()
		if fi, err := os.Stat(p); err == nil && fi.Size() >= OutboxMaxBytes {
			// Dropping is a real loss of telemetry, never a normal condition, so
			// it is reported unconditionally rather than through the debug-gated
			// logger. The event still exists in the signed ledger — the audit
			// trail stays complete; only the upload is lost.
			warnf("outbox is full (%d bytes) — DROPPING event (%s). Delivery has been failing long enough to fill the queue; check connectivity and the ingest endpoint.",
				fi.Size(), ev.Kind)
			return nil
		}
		// #nosec G304 -- p is state.OutboxPath(), derived from state.StateDir(), not user input.
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
var warnOut io.Writer = os.Stderr

// warnf reports a delivery problem. Deliberately NOT debug-gated: a queue that
// silently stops draining is indistinguishable from an idle one, which is the
// exact failure this package exists to prevent.
func warnf(format string, args ...interface{}) {
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
func readCursor() int64 {
	data, err := os.ReadFile(state.OutboxCursorPath())
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeCursor commits the offset via temp+rename so a crash mid-write cannot
// leave a half-written number that parses as a smaller offset (which would
// re-send) or a larger one (which would skip).
func writeCursor(n int64) error {
	p := state.OutboxCursorPath()
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
func PendingCount() int {
	cursor := readCursor()
	// #nosec G304 -- state.OutboxPath() is StateDir()-derived, not user input.
	f, err := os.Open(state.OutboxPath())
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

// --- drain -------------------------------------------------------------------

var startOnce sync.Once

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
func StartDrain(client *http.Client, apiKey string) {
	startOnce.Do(func() {
		go Drain(context.Background(), client, apiKey)
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
func Drain(ctx context.Context, client *http.Client, apiKey string) {
	for {
		n, err := drainOnce(ctx, client, apiKey)
		if err != nil {
			warnf("drain error: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
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
func drainOnce(ctx context.Context, client *http.Client, apiKey string) (int, error) {
	cursor := readCursor()
	p := state.OutboxPath()

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
		if err := writeCursor(0); err != nil {
			return 0, err
		}
	}
	if cursor == fi.Size() {
		compact(cursor)
		return 0, nil
	}

	// #nosec G304 -- p is state.OutboxPath(), derived from state.StateDir(), not user input.
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
		if err := writeCursor(cursor); err != nil {
			// Cannot persist progress: stop before sending more, or a restart
			// would re-send everything after the last durable cursor.
			return delivered, fmt.Errorf("persist cursor: %w", err)
		}
		delivered++
	}
	compact(cursor)
	return delivered, nil
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

// backoffFor returns an exponentially-growing delay with full jitter, capped at
// backoffCap. Jitter matters here: every watcher on every machine in an org
// backs off against the SAME endpoint, and a deterministic schedule would
// re-synchronize them into the retry storm we are trying to break up.
func backoffFor(attempt int) time.Duration {
	d := backoffBase << min(attempt, 16) // cheap guard against shift overflow
	if d > backoffCap || d <= 0 {
		d = backoffCap
	}
	// Full jitter: uniform in (0, d]. Never 0 — a zero wait would busy-loop.
	jittered := time.Duration(rand.Int63n(int64(d))) + 1 // #nosec G404 -- jitter, not security
	return jittered
}

// compact resets a fully-delivered queue to empty so it does not grow forever.
//
// Safe because the outbox is NOT the audit ledger: it carries no signature
// chain, so truncating delivered events destroys nothing auditable (buffer.jsonl
// retains them). Guarded by the same lock appends take, and re-checks the size
// under that lock — an append that landed since the caller's read makes size >
// cursor, and compacting then would discard an undelivered event.
func compact(cursor int64) {
	if cursor <= 0 {
		return
	}
	err := sign.WithBufferLock(state.OutboxLockPath(), func() error {
		fi, err := os.Stat(state.OutboxPath())
		if err != nil {
			return nil //nolint:nilerr // nothing to compact
		}
		if fi.Size() != cursor {
			return nil // raced with an append — leave it for the next pass
		}
		if err := os.Truncate(state.OutboxPath(), 0); err != nil {
			return err
		}
		return writeCursor(0)
	})
	if err != nil {
		warnf("compaction failed (queue will keep growing until it succeeds): %v", err)
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
