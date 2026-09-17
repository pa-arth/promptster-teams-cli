package outbox

import (
	"os"

	"context"
	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// resetDeliveryHealth clears the process-wide health map so a test sees only
// the outcomes its own drain records.
func resetDeliveryHealth(t *testing.T) {
	t.Helper()
	wipe := func() {
		deliveryHealthMu.Lock()
		deliveryHealthByLane = map[string]DeliveryHealth{}
		deliveryHealthMu.Unlock()
	}
	wipe()
	t.Cleanup(wipe)
}

// TestHealthIsNotOKWhileTheHeadCannotAdvance: the beat's deliveryState must
// describe whether the QUEUE HEAD moves, not whether the last POST returned 2xx.
//
// Before this, success was recorded the moment the backend answered, one step
// BEFORE the cursor write that actually advances the head. A drain whose cursor
// could not be persisted re-sent the same accepted head forever and reported
// "ok" the whole time. And the local-fault branch recorded nothing at all, so a
// drain that failed before any POST (a cursor past EOF it cannot rewind) left the
// beat at "unknown" for the life of the process — the value the backend reads as
// "no health data". Either way the head was frozen and the beat said otherwise.
func TestHealthIsNotOKWhileTheHeadCannotAdvance(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"cursor write fails after the backend accepted the head": func(t *testing.T) {},
		"cursor past EOF cannot be rewound": func(t *testing.T) {
			if err := writeCursorFile(LaneLive(), 1<<20); err != nil {
				t.Fatalf("seed cursor: %v", err)
			}
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			newOutboxTest(t)
			captureWarnings(t)
			resetDeliveryHealth(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()
			t.Setenv("PROMPTSTER_API_URL", srv.URL)

			enqueue(t, "prompt")
			seed(t)
			failCursorAfter(t, 0)

			ctx, cancel := context.WithCancel(context.Background())
			finished := make(chan struct{})
			go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST", nil) }()
			time.Sleep(500 * time.Millisecond)
			cancel()
			<-finished

			// PendingStateNow, not PendingCount: it is what the beat reports, and it
			// reads a past-EOF cursor as "compacted underneath us" (count from 0).
			if n := PendingStateNow().Count; n != 1 {
				t.Fatalf("precondition: the beat must still see the head queued, pending = %d", n)
			}
			got := DeliveryHealthNow()
			if got.State != "retrying" || got.Lane != "live" || got.FailureAt == "" {
				t.Fatalf("head frozen on a local fault but beat health = %+v, want retrying on live", got)
			}
		})
	}
}

// TestHealthOKOnlyAfterTheHeadAdvances pins the other half: a drain that does
// advance still reports ok, and a recovered cursor clears the local fault.
func TestHealthOKOnlyAfterTheHeadAdvances(t *testing.T) {
	newOutboxTest(t)
	captureWarnings(t)
	resetDeliveryHealth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt", "command")
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST", nil) }()
	defer func() { cancel(); <-finished }()

	if !waitPendingZero(t, 5*time.Second) {
		t.Fatalf("queue never drained: %d pending", PendingCount())
	}
	if got := DeliveryHealthNow(); got.State != "ok" {
		t.Fatalf("drained queue health = %+v, want ok", got)
	}
}

// TestAwaitDeliveryOutcome: the startup beat waits for the drain's first answer
// (or an empty queue) instead of reporting "unknown" beside a queue nobody has
// tried yet.
func TestAwaitDeliveryOutcome(t *testing.T) {
	t.Run("empty queue returns at once", func(t *testing.T) {
		newOutboxTest(t)
		resetDeliveryHealth(t)
		start := time.Now()
		AwaitDeliveryOutcome(nil, 5*time.Second)
		if time.Since(start) > time.Second {
			t.Fatalf("waited %v on an empty queue", time.Since(start))
		}
	})
	t.Run("queued work waits for an outcome recorded after the call", func(t *testing.T) {
		newOutboxTest(t)
		resetDeliveryHealth(t)
		recordDeliverySuccess("live") // stale outcome from before the wait: must not count
		enqueue(t, "prompt")
		go func() {
			time.Sleep(300 * time.Millisecond)
			recordDeliveryFailure("live", context.DeadlineExceeded, 0)
		}()
		start := time.Now()
		AwaitDeliveryOutcome(nil, 5*time.Second)
		if waited := time.Since(start); waited < 250*time.Millisecond || waited > 3*time.Second {
			t.Fatalf("waited %v, want roughly the 300ms until the drain answered", waited)
		}
	})
	t.Run("bounded when nothing drains", func(t *testing.T) {
		newOutboxTest(t)
		resetDeliveryHealth(t)
		enqueue(t, "prompt")
		start := time.Now()
		AwaitDeliveryOutcome(nil, 300*time.Millisecond)
		if waited := time.Since(start); waited > 2*time.Second {
			t.Fatalf("waited %v past a 300ms bound", waited)
		}
	})
}

// TestAwaitDeliveryOutcomeCoversEveryLane: live answering first must not end the
// wait while a non-empty backfill lane has not been tried — the beat would call
// the whole queue ok on half of it.
func TestAwaitDeliveryOutcomeCoversEveryLane(t *testing.T) {
	newOutboxTest(t)
	resetDeliveryHealth(t)
	enqueue(t, "prompt")
	if err := AppendTo(LaneBackfill(), event.NewEvent("prompt", "sess-test")); err != nil {
		t.Fatalf("seed backfill: %v", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		recordDeliverySuccess("live")
		time.Sleep(500 * time.Millisecond)
		recordDeliverySuccess("backfill")
	}()
	start := time.Now()
	AwaitDeliveryOutcome(nil, 5*time.Second)
	if waited := time.Since(start); waited < 550*time.Millisecond {
		t.Fatalf("wait ended after %v, on live's outcome, before the non-empty backfill lane answered", waited)
	}
}

// TestAwaitDeliveryOutcomeUnreadableQueueIsNotEmpty: a queue that cannot be read
// is not an empty one, and must not release the wait before the drain reports.
func TestAwaitDeliveryOutcomeUnreadableQueueIsNotEmpty(t *testing.T) {
	newOutboxTest(t)
	resetDeliveryHealth(t)
	enqueue(t, "prompt")
	if err := os.Chmod(LaneLive().path(), 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(LaneLive().path(), 0o600) })
	start := time.Now()
	AwaitDeliveryOutcome(nil, 700*time.Millisecond)
	if waited := time.Since(start); waited < 600*time.Millisecond {
		t.Fatalf("an unreadable queue released the wait as empty after %v", waited)
	}
}
