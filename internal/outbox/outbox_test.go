package outbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// newOutboxTest isolates all outbox state in a temp dir.
func newOutboxTest(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	return tmp
}

// enqueue appends n events straight to the queue file, bypassing the ledger
// (which is not under test here).
func enqueue(t *testing.T, kinds ...string) {
	t.Helper()
	for _, k := range kinds {
		if err := Append(event.NewEvent(k, "sess-test")); err != nil {
			t.Fatalf("Append(%s): %v", k, err)
		}
	}
}

// runDrain runs the drain until want requests have arrived (or it times out),
// then cancels and waits for the goroutine so it cannot race t.TempDir cleanup.
func runDrain(t *testing.T, srv *httptest.Server, done <-chan struct{}, timeout time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST") }()
	defer func() { cancel(); <-finished }()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// TestDrainHonorsRetryAfterOn429 pins bug 3: the backend returns Retry-After on
// 429 and the old code discarded the header and had no backoff at all — the
// event was simply dropped. The drain must sleep the server's stated delay and
// retry THE SAME event, delivering it rather than losing it.
func TestDrainHonorsRetryAfterOn429(t *testing.T) {
	newOutboxTest(t)

	var attempts int32
	var gap time.Duration
	var first time.Time
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			first = time.Now()
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-RateLimit-Limit", "100")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		gap = time.Since(first)
		w.WriteHeader(http.StatusCreated)
		once.Do(func() { close(done) })
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt")
	if !runDrain(t, srv, done, 10*time.Second) {
		t.Fatal("429'd event was never retried — it must not be dropped (bug 2/3)")
	}

	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Errorf("attempts = %d, want >= 2 (the SAME event must be retried)", got)
	}
	// Honoring Retry-After: 1 means waiting ~1s, not retrying instantly.
	if gap < 900*time.Millisecond {
		t.Errorf("retry gap = %v, want >= ~1s (Retry-After was ignored — that is the 429 storm)", gap)
	}
	if n := PendingCount(); n != 0 {
		t.Errorf("queue must be empty after delivery; pending = %d", n)
	}
}

// TestDrainRetryAfterHTTPDate covers the RFC 7231 date form, which an
// intermediary may rewrite the seconds form into.
func TestDrainRetryAfterHTTPDate(t *testing.T) {
	newOutboxTest(t)

	var attempts int32
	done := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", time.Now().Add(1*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
		once.Do(func() { close(done) })
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt")
	if !runDrain(t, srv, done, 10*time.Second) {
		t.Fatal("event with HTTP-date Retry-After was never delivered")
	}
}

// TestDrainAdvancesPastRejections pins the 400/422 rule: the backend refusing
// an event's shape or kind can never be fixed by retrying, so the drain must
// skip it and move on rather than wedging the queue head forever.
func TestDrainAdvancesPastRejections(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprintf("HTTP_%d", status), func(t *testing.T) {
			newOutboxTest(t)

			var seen int32
			done := make(chan struct{})
			var once sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := atomic.AddInt32(&seen, 1)
				if n == 1 {
					w.WriteHeader(status) // reject the first
					return
				}
				w.WriteHeader(http.StatusCreated) // the second must still arrive
				once.Do(func() { close(done) })
			}))
			defer srv.Close()
			t.Setenv("PROMPTSTER_API_URL", srv.URL)

			enqueue(t, "subagent_usage", "prompt")
			if !runDrain(t, srv, done, 10*time.Second) {
				t.Fatal("a rejected event blocked the queue; the drain must skip it and deliver the next")
			}
			if n := atomic.LoadInt32(&seen); n != 2 {
				t.Errorf("requests = %d, want exactly 2 (rejected event must NOT be retried)", n)
			}
			if n := PendingCount(); n != 0 {
				t.Errorf("pending = %d, want 0 (cursor must advance past the rejection)", n)
			}
		})
	}
}

// TestDrainRetriesServerErrorsWithBackoff pins the 5xx rule: a server error is
// transient, so the event must be retried, not dropped. This is the core of bug
// 2 — the old code advanced the transcript offset regardless and lost it.
func TestDrainRetriesServerErrorsWithBackoff(t *testing.T) {
	newOutboxTest(t)

	var attempts int32
	done := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		once.Do(func() { close(done) })
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt")
	if !runDrain(t, srv, done, 15*time.Second) {
		t.Fatal("5xx must be retried until it succeeds — dropping it is the silent data loss (bug 2)")
	}
	if got := atomic.LoadInt32(&attempts); got < 3 {
		t.Errorf("attempts = %d, want >= 3", got)
	}
	if n := PendingCount(); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
}

// TestDrainSuccessAdvancesCursor is the happy path: delivered events must not
// be re-sent.
func TestDrainSuccessAdvancesCursor(t *testing.T) {
	newOutboxTest(t)

	var seen int32
	done := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&seen, 1) == 3 {
			once.Do(func() { close(done) })
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt", "command", "file_diff")
	if !runDrain(t, srv, done, 10*time.Second) {
		t.Fatal("queued events were not delivered")
	}
	// Let any spurious re-send land before asserting.
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&seen); got != 3 {
		t.Errorf("requests = %d, want exactly 3 — anything more is re-sending delivered events", got)
	}
	if n := PendingCount(); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
}

// TestCursorPersistsAcrossRestart proves durability: events delivered before a
// crash must not be re-sent after it, and undelivered ones must still go.
func TestCursorPersistsAcrossRestart(t *testing.T) {
	newOutboxTest(t)

	var seen int32
	firstDone := make(chan struct{})
	var once sync.Once
	// Session 1: accept exactly one event, then hard-fail so the drain stalls
	// on the second with the cursor durably at 1 event.
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&seen, 1) == 1 {
			w.WriteHeader(http.StatusCreated)
			once.Do(func() { close(firstDone) })
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Setenv("PROMPTSTER_API_URL", srv1.URL)

	enqueue(t, "prompt", "command")
	if !runDrain(t, srv1, firstDone, 10*time.Second) {
		t.Fatal("first event was not delivered")
	}
	srv1.Close()

	cursorAfterFirst := readCursor()
	if cursorAfterFirst <= 0 {
		t.Fatal("cursor must be durably persisted after a delivery")
	}
	if n := PendingCount(); n != 1 {
		t.Fatalf("pending = %d, want 1 (the undelivered second event)", n)
	}

	// "Restart": brand-new drain, new server. It must resume at the cursor and
	// send ONLY the second event.
	var seen2 int32
	secondDone := make(chan struct{})
	var once2 sync.Once
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&seen2, 1)
		w.WriteHeader(http.StatusCreated)
		once2.Do(func() { close(secondDone) })
	}))
	defer srv2.Close()
	t.Setenv("PROMPTSTER_API_URL", srv2.URL)

	if !runDrain(t, srv2, secondDone, 10*time.Second) {
		t.Fatal("the surviving event was not delivered after restart")
	}
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&seen2); got != 1 {
		t.Errorf("post-restart requests = %d, want exactly 1 — re-sending the already-delivered event is the duplicate bug", got)
	}
}

// TestFreshInstallWithLargeLedgerDrainsNothing is THE upgrade-safety
// regression, and the reason the queue is a separate file from the ledger.
//
// buffer.jsonl on a live machine already holds thousands of ALREADY-SENT events
// (~6,884 back to Jul 9 on the reported device). Had the drain used the ledger
// as its queue, a fresh cursor would replay that entire backlog on first run and
// cause exactly the 429 storm this whole change removes — worse than the
// original bug. The outbox is a NEW file, so on upgrade it is empty and there is
// nothing to replay: only newly appended events ever drain.
func TestFreshInstallWithLargeLedgerDrainsNothing(t *testing.T) {
	tmp := newOutboxTest(t)

	// A big pre-existing ledger, exactly as an upgrading device has.
	var ledger strings.Builder
	for i := 0; i < 5000; i++ {
		ledger.WriteString(fmt.Sprintf(`{"kind":"prompt","sessionId":"old","sig":"s%d"}`+"\n", i))
	}
	if err := os.WriteFile(filepath.Join(tmp, "buffer.jsonl"), []byte(ledger.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// Fresh install: no outbox, no cursor.
	if _, err := os.Stat(state.OutboxPath()); !os.IsNotExist(err) {
		t.Fatal("precondition: outbox must not exist on a fresh install")
	}
	if _, err := os.Stat(state.OutboxCursorPath()); !os.IsNotExist(err) {
		t.Fatal("precondition: cursor must not exist on a fresh install")
	}

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	if n := PendingCount(); n != 0 {
		t.Errorf("a fresh install must report 0 pending despite a %d-event ledger; got %d", 5000, n)
	}

	// Run the drain briefly; it must send NOTHING.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	Drain(ctx, srv.Client(), "PSE-TEST")

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("drain sent %d request(s) on a fresh install — the ledger backlog must NEVER be replayed (this is the 429 storm)", got)
	}
}

// TestDrainDeliversOnlyNewEventsAfterUpgrade is the other half: with a large
// ledger present, a newly captured event still drains normally.
func TestDrainDeliversOnlyNewEventsAfterUpgrade(t *testing.T) {
	tmp := newOutboxTest(t)
	if err := os.WriteFile(filepath.Join(tmp, "buffer.jsonl"),
		[]byte(strings.Repeat(`{"kind":"prompt","sessionId":"old","sig":"x"}`+"\n", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}

	var hits int32
	done := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusCreated)
		once.Do(func() { close(done) })
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt") // one NEW event
	if !runDrain(t, srv, done, 10*time.Second) {
		t.Fatal("a newly queued event must still be delivered")
	}
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("requests = %d, want exactly 1 (the new event only, never the ledger)", got)
	}
}

// TestCompactionResetsQueue proves the queue does not grow forever: once fully
// delivered it is truncated and the cursor reset. Safe only because the outbox
// carries no signature chain — the ledger keeps the auditable copy.
func TestCompactionResetsQueue(t *testing.T) {
	newOutboxTest(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt", "command")

	// Unlike the other tests, this one must observe state the drain writes AFTER
	// the last response, so it cannot cancel on the final request — it keeps the
	// drain alive and polls until compaction lands.
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST") }()
	defer func() { cancel(); <-finished }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fi, err := os.Stat(state.OutboxPath())
		if err == nil && fi.Size() == 0 && readCursor() == 0 {
			return // compacted
		}
		time.Sleep(50 * time.Millisecond)
	}
	fi, _ := os.Stat(state.OutboxPath())
	t.Errorf("queue was not compacted after full delivery: size=%d cursor=%d", fi.Size(), readCursor())
}

// TestBackoffForIsBoundedAndJittered guards the retry schedule: bounded by
// backoffCap, never zero (a zero wait busy-loops), and jittered so every
// machine in an org does not resynchronize into a retry storm.
func TestBackoffForIsBoundedAndJittered(t *testing.T) {
	for attempt := 0; attempt < 40; attempt++ {
		d := backoffFor(attempt)
		if d <= 0 {
			t.Fatalf("backoffFor(%d) = %v, must be > 0", attempt, d)
		}
		if d > backoffCap {
			t.Fatalf("backoffFor(%d) = %v, exceeds cap %v", attempt, d, backoffCap)
		}
	}
	// Jitter: repeated calls at one attempt must not all be identical.
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[backoffFor(8)] = true
	}
	if len(seen) == 1 {
		t.Error("backoff is not jittered — synchronized retries recreate the storm")
	}
}

// TestDrainShipsQueuedBytesVerbatim guards the signature chain.
//
// The backend verifies each event's ed25519 signature by recomputing canonical
// JSON from the body it receives. Event.Data is an interface{}, so a drain that
// unmarshalled and re-marshalled would coerce every number to float64 — and any
// integer above 2^53 would come back out with different digits, silently
// failing verification for the events whose integrity we most want to prove.
// The queued line must reach the wire byte-for-byte.
func TestDrainShipsQueuedBytesVerbatim(t *testing.T) {
	newOutboxTest(t)

	bodies := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	// A value that a float64 round-trip mangles: 2^53 + 1 territory.
	const bigInt = int64(1234567890123456789)
	queued := fmt.Sprintf(
		`{"kind":"prompt","sessionId":"s1","sig":"deadbeef","prevSig":"","data":{"tokens":%d}}`, bigInt)
	if err := os.WriteFile(state.OutboxPath(), []byte(queued+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST") }()
	defer func() { cancel(); <-finished }()

	var got []byte
	select {
	case got = <-bodies:
	case <-time.After(5 * time.Second):
		t.Fatal("queued event was never delivered")
	}

	if string(got) != queued {
		t.Errorf("wire body must be the queued bytes verbatim (signature is over these bytes)\n got: %s\nwant: %s", got, queued)
	}
	if !strings.Contains(string(got), fmt.Sprint(bigInt)) {
		t.Errorf("large int was mangled in transit — signature verification would fail: %s", got)
	}
}

// syncBuf is a race-safe io.Writer for capturing warnf output.
type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureWarnings redirects warnf and shrinks the backoff ramp so a test can
// reach the stuck threshold quickly.
func captureWarnings(t *testing.T) *syncBuf {
	t.Helper()
	sb := &syncBuf{}
	prevOut, prevBase := warnOut, backoffBase
	warnOut, backoffBase = sb, time.Millisecond
	t.Cleanup(func() { warnOut, backoffBase = prevOut, prevBase })
	return sb
}

// TestDrainWarnsLoudlyWhenStuck closes the silent-failure gap.
//
// Anything that is not 2xx/400/422 retries forever. That is the right design —
// the queue is bounded by size, not by giving up — but the per-attempt log was
// debug-gated, so a permanently failing head-of-queue was INVISIBLE. A revoked
// engineer key (401, via DELETE /v1/team/engineers/:userId) is neither a
// rejection nor rate-limited, so the drain would spin silently for weeks until
// the outbox filled and began dropping: silent capture loss, the exact failure
// class this package exists to remove. After stuckAttemptThreshold consecutive
// failures on ONE event the drain must say so at warn level, with no
// special-casing by status code.
func TestDrainWarnsLoudlyWhenStuck(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,        // revoked/rotated engineer key
		http.StatusForbidden,           // key valid, org access pulled
		http.StatusInternalServerError, // permanent backend failure / poison event
	} {
		t.Run(fmt.Sprintf("HTTP_%d", status), func(t *testing.T) {
			newOutboxTest(t)
			warnings := captureWarnings(t)

			var attempts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(status)
			}))
			defer srv.Close()
			t.Setenv("PROMPTSTER_API_URL", srv.URL)

			enqueue(t, "prompt")

			ctx, cancel := context.WithCancel(context.Background())
			finished := make(chan struct{})
			go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST") }()
			defer func() { cancel(); <-finished }()

			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if strings.Contains(warnings.String(), "STUCK") {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}

			got := warnings.String()
			if !strings.Contains(got, "STUCK") {
				t.Fatalf("a permanently failing HTTP %d must be reported LOUDLY, not debug-gated "+
					"(this is the silent-capture-loss gap); attempts=%d warnings=%q",
					status, atomic.LoadInt32(&attempts), got)
			}
			if !strings.Contains(got, "prompt") {
				t.Errorf("warning must name the stuck event kind: %q", got)
			}
			// Retry-forever is deliberate: the event must NOT be dropped.
			if n := PendingCount(); n != 1 {
				t.Errorf("pending = %d, want 1 — an auth failure must never drop the event", n)
			}
		})
	}
}

// TestDrainStaysQuietForTransientBlips is the other half: the escalation must
// not cry wolf. A failure that clears within the threshold stays debug-gated.
func TestDrainStaysQuietForTransientBlips(t *testing.T) {
	newOutboxTest(t)
	warnings := captureWarnings(t)

	var attempts int32
	done := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 { // fewer than stuckAttemptThreshold
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		once.Do(func() { close(done) })
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt")
	if !runDrain(t, srv, done, 10*time.Second) {
		t.Fatal("event was not delivered")
	}
	if got := warnings.String(); strings.Contains(got, "STUCK") {
		t.Errorf("a blip that clears inside the threshold must stay quiet, got: %q", got)
	}
}

// TestDrainAnnouncesRecovery: once we have warned, we must also say it cleared,
// or the operator is left with a scary warning and no resolution.
func TestDrainAnnouncesRecovery(t *testing.T) {
	newOutboxTest(t)
	warnings := captureWarnings(t)

	var attempts int32
	done := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) <= stuckAttemptThreshold+1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		once.Do(func() { close(done) })
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt")
	if !runDrain(t, srv, done, 15*time.Second) {
		t.Fatal("event was never delivered after the key came back")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(warnings.String(), "recovered") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("a stuck queue that recovers must say so; warnings=%q", warnings.String())
}

// failCursorAfter makes writeCursor succeed for the first n calls, then fail
// forever — simulating a disk that fills mid-drain.
func failCursorAfter(t *testing.T, n int32) *int32 {
	t.Helper()
	var calls int32
	prev := writeCursor
	writeCursor = func(v int64) error {
		if atomic.AddInt32(&calls, 1) <= n {
			return prev(v)
		}
		return errors.New("write /outbox.jsonl.cursor.tmp: no space left on device")
	}
	t.Cleanup(func() { writeCursor = prev })
	return &calls
}

// TestCursorPersistFailureBacksOffInsteadOfResendingEverySecond is the
// regression for the stale cursor found in review.
//
// If writeCursor fails AFTER deliver already succeeded, the durable cursor still
// names the previous event, so the next pass re-reads and re-POSTs one the
// backend already accepted. Under a persistent cursor fault (disk full,
// read-only FS) that repeats FOREVER — the duplicate-send bug this package
// exists to remove, coming back through a local-IO fault.
//
// MEASURED, because the shape is easy to get wrong: the old code did NOT spin at
// line rate. `delivered++` sits AFTER the cursor write, so a pass whose first
// write fails returns delivered==0 and Drain takes the idle sleep anyway. The
// real behaviour was a re-POST every drainIdleInterval — 14 POSTs in 12s with the
// inter-POST gap pinned at exactly 1.0s. That is ~60/min against the 100/min
// bucket: quieter than "line rate", still ruinous, still forever.
//
// So the discriminator is the GAP, not the count. Old is deterministically capped
// at drainIdleInterval; backed-off retries climb past it, heading for backoffCap.
// A gap above ~1s proves a real backoff is in the loop.
//
// The count cannot be the discriminator: correct backoff reaches 11 POSTs in its
// tail and the bug produces ~14, so the two overlap. The gap separates them
// cleanly — but only once the jitter is pinned, below.
func TestCursorPersistFailureBacksOffInsteadOfResendingEverySecond(t *testing.T) {
	newOutboxTest(t)
	warnings := captureWarnings(t)
	// Real base: the fix's value is that the ramp escapes drainIdleInterval.
	backoffBase = 500 * time.Millisecond
	// Pin the jitter to the ceiling, making the schedule the exact ramp
	// (0.5s, 1s, 2s, 4s...) instead of a uniform draw under it.
	//
	// Without this the assertion below is a coin toss on the RNG: backoffFor
	// returns uniform(0, d], so a run whose draws all land low has every gap
	// under the threshold while the code is behaving perfectly. That is not
	// hypothetical — it failed ~2.7% of runs (~1 in 37) and twice on real CI,
	// both times with the exact signature the model predicts: 7 POSTs, maxGap
	// under 1.5s. Pinning kills the false alarm without weakening the check;
	// the bug still yields a 1.0s cap and still fails. TestBackoffForFullJitter
	// covers the real jitter that this stubs out.
	prevJitter := backoffJitter
	backoffJitter = func(d time.Duration) time.Duration { return d }
	t.Cleanup(func() { backoffJitter = prevJitter })

	var mu sync.Mutex
	var stamps []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	// The first cursor write lands, every later one fails: exactly the
	// delivered>0 + error shape, which also took Drain's "don't idle" fast path.
	failCursorAfter(t, 1)
	enqueue(t, "prompt", "command")

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST") }()
	time.Sleep(12 * time.Second)
	cancel()
	<-finished

	mu.Lock()
	defer mu.Unlock()
	if len(stamps) < 2 {
		t.Fatalf("expected the drain to deliver and then retry; posts = %d", len(stamps))
	}
	var maxGap time.Duration
	for i := 1; i < len(stamps); i++ {
		if g := stamps[i].Sub(stamps[i-1]); g > maxGap {
			maxGap = g
		}
	}
	if maxGap <= 1500*time.Millisecond {
		t.Errorf("re-sends of an already-delivered event never backed off (maxGap = %v, %d posts in 12s) — "+
			"a cursor-persist fault must ramp past drainIdleInterval (%v), not re-POST on every idle tick",
			maxGap.Round(10*time.Millisecond), len(stamps), drainIdleInterval)
	}
	if len(stamps) > 10 {
		t.Errorf("too many duplicate re-sends in 12s: %d (un-backed-off is ~14, backed-off is ~6-8)", len(stamps))
	}
	if w := warnings.String(); !strings.Contains(w, "cannot record delivery progress") {
		t.Errorf("a cursor-write failure is a local disk fault the user can act on and must be surfaced loudly; warnings = %q", w)
	}
}

// TestBackoffForFullJitter covers the real jitter that the test above pins away,
// so pinning trades a false alarm for a stub rather than for lost coverage.
//
// Full jitter is a deliberate property, not an implementation detail: every
// watcher on every machine in an org backs off against the SAME endpoint, so a
// deterministic schedule re-synchronizes them into the retry storm the backoff
// exists to break up. That is a distribution claim, so assert the distribution
// rather than any single draw — the mistake that made the other test flaky.
func TestBackoffForFullJitter(t *testing.T) {
	prevBase := backoffBase
	backoffBase = 500 * time.Millisecond
	t.Cleanup(func() { backoffBase = prevBase })

	const draws = 200
	for _, attempt := range []int{0, 1, 3} {
		ceiling := backoffBase << attempt
		seen := make(map[time.Duration]bool, draws)
		for range draws {
			d := backoffFor(attempt)
			if d <= 0 || d > ceiling {
				t.Fatalf("backoffFor(%d) = %v, want a draw within (0, %v] — a delay above the ceiling "+
					"stalls delivery, and a zero delay busy-loops", attempt, d, ceiling)
			}
			seen[d] = true
		}
		// Jitter gone (a fixed schedule) collapses this to 1 distinct value.
		if len(seen) < 2 {
			t.Errorf("backoffFor(%d) returned %d distinct value(s) over %d draws — the fleet would "+
				"re-synchronize onto one retry schedule", attempt, len(seen), draws)
		}
	}

	// A large attempt must saturate at backoffCap, not overflow the shift into a
	// nonsense (or negative) delay.
	for range draws {
		if d := backoffFor(60); d <= 0 || d > backoffCap {
			t.Fatalf("backoffFor(60) = %v, want a draw within (0, %v] — the ramp must saturate at the cap", d, backoffCap)
		}
	}
}

// TestCursorPersistRecovers: once the disk frees up, the drain must converge
// and stop re-sending.
func TestCursorPersistRecovers(t *testing.T) {
	newOutboxTest(t)
	captureWarnings(t)

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	// Fail the first few cursor writes, then let them through.
	var calls int32
	prev := writeCursor
	writeCursor = func(v int64) error {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return errors.New("no space left on device")
		}
		return prev(v)
	}
	t.Cleanup(func() { writeCursor = prev })

	enqueue(t, "prompt")

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST") }()
	defer func() { cancel(); <-finished }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if PendingCount() == 0 {
			return // converged: cursor durable, queue drained
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Errorf("drain never converged after the cursor became writable; pending = %d, posts = %d",
		PendingCount(), atomic.LoadInt32(&posts))
}
