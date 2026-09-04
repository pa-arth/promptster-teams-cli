package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// The lane split exists for ONE observed failure: a schema bump queued 62,302
// replayed events on one device, and the live prompt typed a second later was
// delivered behind all of them. The backlog measured 20,761 deep with its head
// 15 days old while the dashboard reported that engineer idle.
//
// Every test here is written against that episode. The load-bearing ones assert
// a RELATIONSHIP between the lanes (live moved while backfill was still going),
// not a count in isolation — a count alone passes on a build where the split
// does nothing, because a fast enough backend drains one FIFO fast enough too.

// laneTest isolates outbox state and clears the process-wide batch latch.
func laneTest(t *testing.T) string {
	t.Helper()
	tmp := newOutboxTest(t)
	batchSuppressMu.Lock()
	batchSuppressUntil = time.Time{}
	batchSuppressMu.Unlock()
	warnOut = io.Discard
	t.Cleanup(func() { warnOut = os.Stderr })
	return tmp
}

// appendBackfill queues an event whose own timestamp is well past LiveHorizon,
// through the durable-source door — i.e. exactly what a transcript replay does.
func appendBackfill(t *testing.T, kind string) {
	t.Helper()
	ev := event.NewEvent(kind, "sess-test")
	ev.Ts = time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	if err := AppendFromDurableSource(ev); err != nil {
		t.Fatalf("AppendFromDurableSource(%s): %v", kind, err)
	}
}

// kindOf reads the kind off a single-event POST body.
func kindOf(t *testing.T, body []byte) string {
	t.Helper()
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Kind
}

// startDrain runs a Drain and guarantees it is stopped before t.TempDir cleanup.
func startDrain(t *testing.T, client *http.Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); Drain(ctx, client, "PSE-TEST", nil) }()
	t.Cleanup(func() { cancel(); <-finished })
}

// TestLiveIsDeliveredWhileABacklogIsStillDraining is the episode itself.
//
// A large replay is queued FIRST, then one live event. In one FIFO the live
// event is delivered last by construction; the assertion is that it is not —
// and, more precisely, that it lands while most of the backlog is still queued.
// A bare "it arrived" would also pass on a build with no lanes at all, given a
// backend fast enough to chew through the backlog inside the timeout.
func TestLiveIsDeliveredWhileABacklogIsStillDraining(t *testing.T) {
	laneTest(t)

	const backlog = 400

	var backfillSeen int32
	liveAt := make(chan int32, 1)
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if kindOf(t, body) == "live-prompt" {
			once.Do(func() { liveAt <- atomic.LoadInt32(&backfillSeen) })
		} else {
			atomic.AddInt32(&backfillSeen, 1)
			// A real backend is not instant, and without this the test measures
			// httptest's loopback latency rather than the queueing discipline.
			time.Sleep(3 * time.Millisecond)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	for i := 0; i < backlog; i++ {
		appendBackfill(t, "replayed")
	}
	if err := Append(event.NewEvent("live-prompt", "sess-test")); err != nil {
		t.Fatalf("Append(live): %v", err)
	}

	startDrain(t, srv.Client())

	select {
	case drainedFirst := <-liveAt:
		// Half the backlog is a deliberately loose bound: the point is that the
		// live event did not wait for the WHOLE queue, and a tight number here
		// would just be scheduler noise.
		if drainedFirst >= backlog/2 {
			t.Errorf("the live event waited for %d/%d backfill events — it is still queued behind the replay",
				drainedFirst, backlog)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the live event was never delivered while a backlog drained — this is the reported episode")
	}
}

// TestAWedgedBackfillHeadDoesNotStopLive is the acceptance the two-goroutine
// design was chosen for, and the ORDER of this test is the whole test.
//
// The backfill head 500s forever, so its lane retries in place forever — correct
// and deliberate (the queue is bounded by OutboxMaxBytes, not by giving up).
// What must not happen is live going down with it.
//
// The live events are appended only AFTER the wedge is provably being retried.
// Queueing them up front instead is the version of this test that passes on a
// build with no lane independence at all: a single-goroutine scheduler that
// drains live to empty and only then touches backfill delivers those five and
// looks healthy, having never had to serve live traffic while stuck. The
// question is not whether an already-queued live event escapes; it is whether
// the NEXT one does.
func TestAWedgedBackfillHeadDoesNotStopLive(t *testing.T) {
	laneTest(t)

	var liveSeen, backfillSeen int32
	retrying := make(chan struct{})
	enough := make(chan struct{})
	var retryOnce, enoughOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if kindOf(t, body) == "live-prompt" {
			if atomic.AddInt32(&liveSeen, 1) >= 5 {
				enoughOnce.Do(func() { close(enough) })
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		// A second attempt on the same poison event proves the retry loop is
		// engaged, not merely that one request failed.
		if atomic.AddInt32(&backfillSeen, 1) >= 2 {
			retryOnce.Do(func() { close(retrying) })
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	appendBackfill(t, "poison")
	for i := 0; i < 5; i++ {
		appendBackfill(t, "replayed")
	}

	startDrain(t, srv.Client())

	select {
	case <-retrying:
	case <-time.After(15 * time.Second):
		t.Fatal("the poison backfill event was never retried; the wedge this test needs never formed")
	}

	// Live traffic arriving while backfill is stuck mid-backoff.
	for i := 0; i < 5; i++ {
		if err := Append(event.NewEvent("live-prompt", "sess-test")); err != nil {
			t.Fatalf("Append(live): %v", err)
		}
	}

	select {
	case <-enough:
	case <-time.After(15 * time.Second):
		t.Fatalf("live delivery stalled behind a permanently-failing backfill head; live=%d backfill-attempts=%d",
			atomic.LoadInt32(&liveSeen), atomic.LoadInt32(&backfillSeen))
	}
	// And the wedge really is a wedge: nothing behind it advanced.
	if n := pendingCountIn(LaneBackfill()); n != 6 {
		t.Errorf("backfill pending = %d, want 6 — a 5xx head must NOT be skipped", n)
	}
}

// TestBackfillStillDrainsUnderContinuousLiveTraffic is the mirror of the case
// above: the split must not solve head-of-line blocking by starving the slow
// lane, which would turn a 15-day-old backlog into a permanent one.
func TestBackfillStillDrainsUnderContinuousLiveTraffic(t *testing.T) {
	laneTest(t)

	var backfillSeen int32
	enough := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if kindOf(t, body) == "replayed" {
			if atomic.AddInt32(&backfillSeen, 1) >= 20 {
				once.Do(func() { close(enough) })
			}
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	for i := 0; i < 20; i++ {
		appendBackfill(t, "replayed")
	}

	startDrain(t, srv.Client())

	// Live traffic for as long as the test runs, appended from another goroutine
	// exactly as the watchers do.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = Append(event.NewEvent("live-prompt", "sess-test"))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	select {
	case <-enough:
	case <-time.After(15 * time.Second):
		t.Fatalf("backfill was starved by live traffic; delivered %d/20", atomic.LoadInt32(&backfillSeen))
	}
}

// TestARestartMidDeliveryLosesNothingOnEitherLane pins the durability contract
// per lane: a kill between the POST and the cursor commit may re-send the event
// in flight, and may re-send at most that one, but may never skip.
//
// The two lanes are checked together because the failure this guards against is
// specifically a SHARED cursor — one lane's commit advancing over the other's
// unsent bytes would be invisible in a single-lane test.
func TestARestartMidDeliveryLosesNothingOnEitherLane(t *testing.T) {
	laneTest(t)

	var mu sync.Mutex
	seen := map[string]int{}
	stopAfter := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &probe)
		mu.Lock()
		seen[probe.ID]++
		n := len(seen)
		mu.Unlock()
		if n >= 4 {
			once.Do(func() { close(stopAfter) })
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	want := map[string]bool{}
	for i := 0; i < 8; i++ {
		ev := event.NewEvent("replayed", "sess-test")
		ev.Ts = time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
		want[ev.ID] = true
		if err := AppendFromDurableSource(ev); err != nil {
			t.Fatalf("append backfill: %v", err)
		}
		live := event.NewEvent("live-prompt", "sess-test")
		want[live.ID] = true
		if err := Append(live); err != nil {
			t.Fatalf("append live: %v", err)
		}
	}

	// First run: cancel as soon as delivery is genuinely under way, so the kill
	// lands mid-queue on both lanes rather than after they have both finished.
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST", nil) }()
	select {
	case <-stopAfter:
	case <-time.After(10 * time.Second):
		cancel()
		<-finished
		t.Fatal("no delivery at all on the first run")
	}
	cancel()
	<-finished

	// Second run: a fresh Drain over the same state dir is exactly a restart.
	ctx2, cancel2 := context.WithCancel(context.Background())
	finished2 := make(chan struct{})
	go func() { defer close(finished2); Drain(ctx2, srv.Client(), "PSE-TEST", nil) }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(seen) == len(want)
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	<-finished2

	mu.Lock()
	defer mu.Unlock()
	for id := range want {
		if seen[id] == 0 {
			t.Fatalf("event %s was SKIPPED across the restart — the queue must never lose an event", id)
		}
		if seen[id] > 2 {
			t.Errorf("event %s delivered %d times; a restart may duplicate the in-flight event, not replay the queue",
				id, seen[id])
		}
	}
}

// TestTranscriptTailInsideTheLiveHorizonIsLive is the half of the §1.2 rule that
// is easy to get wrong. A watcher tailing an active session IS a durable source,
// but its events are happening now; routing those to the slow lane would defer
// exactly the traffic that proves the device is alive.
func TestTranscriptTailInsideTheLiveHorizonIsLive(t *testing.T) {
	laneTest(t)

	ev := event.NewEvent("prompt", "sess-test")
	ev.Ts = time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339Nano)
	if err := AppendFromDurableSource(ev); err != nil {
		t.Fatalf("AppendFromDurableSource: %v", err)
	}

	if n := pendingCountIn(LaneLive()); n != 1 {
		t.Errorf("live pending = %d, want 1 — a transcript event from a minute ago is live work", n)
	}
	if n := pendingCountIn(LaneBackfill()); n != 0 {
		t.Errorf("backfill pending = %d, want 0", n)
	}
}

// TestAnOldEventWithNoDurableSourceIsStillLive pins the other half: durability
// is the CALLER's property and is never inferred from age. A hook event is gone
// the moment we decline it, however old its timestamp reads.
func TestAnOldEventWithNoDurableSourceIsStillLive(t *testing.T) {
	laneTest(t)

	ev := event.NewEvent("prompt", "sess-test")
	ev.Ts = time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano)
	if err := Append(ev); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if n := pendingCountIn(LaneLive()); n != 1 {
		t.Errorf("live pending = %d, want 1 — Append never classifies, it only ever queues live", n)
	}
	if n := pendingCountIn(LaneBackfill()); n != 0 {
		t.Errorf("backfill pending = %d, want 0 — an event with no second copy must never be deferred", n)
	}
}

// TestAnUnreadableTimestampFailsTowardLive covers the classification fallback.
// Being wrong toward live costs some queue-jumping; being wrong toward backfill
// parks the event behind a replay that may run for hours.
func TestAnUnreadableTimestampFailsTowardLive(t *testing.T) {
	laneTest(t)

	ev := event.NewEvent("prompt", "sess-test")
	ev.Ts = "not-a-timestamp"
	if err := AppendFromDurableSource(ev); err != nil {
		t.Fatalf("AppendFromDurableSource: %v", err)
	}
	if n := pendingCountIn(LaneLive()); n != 1 {
		t.Errorf("live pending = %d, want 1", n)
	}
}

// TestEachLaneHasItsOwnFullQueueCeiling pins §1.4's per-lane bound. Sharing one
// ceiling would let a replay consume the headroom live capture needs, and live
// capture is the producer that CANNOT defer — its drop is unrecoverable loss.
func TestEachLaneHasItsOwnFullQueueCeiling(t *testing.T) {
	laneTest(t)

	// Fill the backfill lane past OutboxMaxBytes so further backfill appends drop.
	f, err := os.OpenFile(LaneBackfill().path(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open backfill queue: %v", err)
	}
	if err := f.Truncate(OutboxMaxBytes + 1); err != nil {
		t.Fatalf("grow backfill queue: %v", err)
	}
	f.Close()

	// A live append must be entirely unaffected by the backfill lane being full.
	if err := Append(event.NewEvent("live-prompt", "sess-test")); err != nil {
		t.Fatalf("Append(live): %v", err)
	}
	if n := pendingCountIn(LaneLive()); n != 1 {
		t.Errorf("live pending = %d, want 1 — a full BACKFILL lane must not drop live events", n)
	}
}

// TestUnderPressureMeasuresTheBackfillLane pins the §1.4 re-point. Its only
// caller is the history replay, which is the producer that CAN defer; measuring
// the live lane would throttle the one thing that was safe to keep going while
// doing nothing about the pressure live producers actually caused.
func TestUnderPressureMeasuresTheBackfillLane(t *testing.T) {
	laneTest(t)

	prev := PressureHighWater
	PressureHighWater = 1024
	t.Cleanup(func() { PressureHighWater = prev })

	// A live lane well past the high-water mark must NOT throttle replay.
	f, err := os.OpenFile(LaneLive().path(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open live queue: %v", err)
	}
	if err := f.Truncate(PressureHighWater * 4); err != nil {
		t.Fatalf("grow live queue: %v", err)
	}
	f.Close()
	if UnderPressure() {
		t.Error("a full LIVE lane must not defer history replay — replay is not what filled it")
	}

	// The backfill lane past it must.
	bf, err := os.OpenFile(LaneBackfill().path(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open backfill queue: %v", err)
	}
	if err := bf.Truncate(PressureHighWater + 1); err != nil {
		t.Fatalf("grow backfill queue: %v", err)
	}
	bf.Close()
	if !UnderPressure() {
		t.Error("a backfill lane past the high-water mark must defer further replay")
	}
}

// TestBackfillBatchesAreLabelledOnTheWire pins the wire half of the split. The
// backend budgets the lanes separately (live 100 events/min, backfill 60); an
// unlabelled backfill batch is charged against the live budget, which throttles
// exactly the traffic the split exists to protect.
func TestBackfillBatchesAreLabelledOnTheWire(t *testing.T) {
	laneTest(t)

	lanes := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Events []json.RawMessage `json:"events"`
			Lane   string            `json:"lane"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case lanes <- env.Lane:
		default:
		}
		results := make([]map[string]any, 0, len(env.Events))
		for i := range env.Events {
			results = append(results, map[string]any{"index": i, "id": "x", "status": 201})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMultiStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "accepted": len(results), "rejected": 0, "results": results,
		})
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	for i := 0; i < 3; i++ {
		appendBackfill(t, "replayed")
	}

	caps := func() (string, int, bool) { return "/v1/teams/ingest/batch", 100, true }
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); drainLane(ctx, srv.Client(), "PSE-TEST", caps, LaneBackfill()) }()
	defer func() { cancel(); <-finished }()

	select {
	case got := <-lanes:
		if got != "backfill" {
			t.Errorf(`batch lane = %q, want "backfill" — an unlabelled batch is charged the live budget`, got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no batch reached the backend")
	}
}

// TestAFullLaneReportsErrQueueFullInsteadOfSuccess pins the signal a dropped
// event now carries. The drop used to `return nil`, so every caller was told the
// event was queued — and the transcript watchers, which commit their read offset
// from the bytes they consumed, marked the source records read while the events
// derived from them were being discarded. A three-day ingest wedge on one
// customer device dropped ~54k events that way, none of them recoverable without
// hand-rewinding a cursor.
//
// The assertion is errors.Is, not a string match: the callers that matter branch
// on the sentinel, and a wrapped-but-unmatched error would read as an ordinary
// I/O failure at exactly the moment the distinction decides whether bytes are
// re-read or lost.
func TestAFullLaneReportsErrQueueFullInsteadOfSuccess(t *testing.T) {
	laneTest(t)

	f, err := os.OpenFile(LaneLive().path(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open live queue: %v", err)
	}
	if err := f.Truncate(OutboxMaxBytes); err != nil {
		t.Fatalf("grow live queue: %v", err)
	}
	f.Close()

	err = Append(event.NewEvent("prompt", "sess-test"))
	if err == nil {
		t.Fatal("Append onto a lane at OutboxMaxBytes returned nil — a dropped event must never report success")
	}
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Append error = %v, want one matching ErrQueueFull", err)
	}
	if !strings.Contains(err.Error(), "live") {
		t.Errorf("error %q does not name the lane it dropped from", err)
	}

	// The durable-source door must report it too — that is the door the
	// transcript watchers use, and the only one whose caller can act on it.
	stale := event.NewEvent("prompt", "sess-test")
	stale.Ts = time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	bf, err := os.OpenFile(LaneBackfill().path(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open backfill queue: %v", err)
	}
	if err := bf.Truncate(OutboxMaxBytes); err != nil {
		t.Fatalf("grow backfill queue: %v", err)
	}
	bf.Close()
	if err := AppendFromDurableSource(stale); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("AppendFromDurableSource error = %v, want one matching ErrQueueFull", err)
	}
}

// TestTheFullQueueWarningDoesNotPromiseTheLedgerStillHasIt pins the text, which
// is load-bearing: it is the only thing an operator sees at the moment of loss.
//
// It used to end "The event still exists in the signed ledger — the audit trail
// stays complete; only the upload is lost." That was least true exactly when it
// printed. sign.rotateLedgerIfLarge rotates at 16 MiB and keeps
// state.LedgerRetainedSegments beside the live one — ~64 MiB across 4 segments,
// the same bound this lane caps at — and it drops the OLDEST segment while this
// queue drops the NEWEST event. A stall long enough to fill 64 MiB of queue has
// been filling the ledger over the same period, so the reassurance was a guess
// stated as a fact.
func TestTheFullQueueWarningDoesNotPromiseTheLedgerStillHasIt(t *testing.T) {
	newOutboxTest(t)

	var buf bytes.Buffer
	warnMu.Lock()
	warnOut = &buf
	warnMu.Unlock()
	t.Cleanup(func() {
		warnMu.Lock()
		warnOut = os.Stderr
		warnMu.Unlock()
	})

	f, err := os.OpenFile(LaneLive().path(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open live queue: %v", err)
	}
	if err := f.Truncate(OutboxMaxBytes); err != nil {
		t.Fatalf("grow live queue: %v", err)
	}
	f.Close()

	if err := Append(event.NewEvent("prompt", "sess-test")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Append error = %v, want ErrQueueFull", err)
	}

	warnMu.Lock()
	got := buf.String()
	warnMu.Unlock()

	if strings.Contains(got, "audit trail stays complete") || strings.Contains(got, "still exists in the signed ledger") {
		t.Errorf("warning still promises the ledger kept the event: %q", got)
	}
	for _, want := range []string{"DROPPING event", "check connectivity", "ledger is bounded too", "may already have rotated"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q is missing %q — an operator needs the loss, the bound, and the thing to check", got, want)
		}
	}
	// One line: an operator reading stderr must not have to reassemble it.
	if n := strings.Count(strings.TrimRight(got, "\n"), "\n"); n != 0 {
		t.Errorf("warning spans %d extra lines, want one: %q", n+1, got)
	}
}
