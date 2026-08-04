// Batch delivery — spec §4.1 (probe and fall back) and §4.2 (advance past
// accepted AND rejected members alike).
//
// The property that matters most here is not throughput. It is that NOTHING is
// lost at the boundary between a client that can batch and a backend that
// cannot: the CLI advances its cursor from what the backend says, so any answer
// it misreads as "done" is an event nobody will ever send again.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const testBatchPath = "/v1/teams/ingest/batch"

// batchCaps is a capability that always says yes on the given terms.
func batchCaps(maxSize int) BatchCapability {
	return func() (string, int, bool) { return testBatchPath, maxSize, true }
}

// newBatchTest isolates outbox state AND clears the process-wide batch
// suppression latch, which would otherwise leak between tests in this file and
// silently turn a batch assertion into a per-event one.
func newBatchTest(t *testing.T) string {
	t.Helper()
	tmp := newOutboxTest(t)
	batchSuppressMu.Lock()
	batchSuppressUntil = time.Time{}
	batchSuppressMu.Unlock()
	t.Cleanup(func() {
		batchSuppressMu.Lock()
		batchSuppressUntil = time.Time{}
		batchSuppressMu.Unlock()
	})
	return tmp
}

// batchEnvelope is the request shape the CLI sends.
type batchEnvelope struct {
	Events []json.RawMessage `json:"events"`
}

// readEnvelope decodes a batch request body.
func readEnvelope(t *testing.T, r *http.Request) batchEnvelope {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read batch body: %v", err)
	}
	var env batchEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode batch envelope: %v (body=%s)", err, truncate(string(raw)))
	}
	return env
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// respond207 writes a per-member 207 with the given statuses, in request order.
func respond207(w http.ResponseWriter, statuses []int) {
	results := make([]map[string]any, 0, len(statuses))
	accepted := 0
	for i, s := range statuses {
		row := map[string]any{"index": i, "id": fmt.Sprintf("ev-%d", i), "status": s}
		if s >= 400 {
			row["error"] = "refused"
		} else {
			accepted++
		}
		results = append(results, row)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMultiStatus)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "accepted": accepted, "rejected": len(statuses) - accepted, "results": results,
	})
}

// allOK is the common case: every member stored.
func allOK(w http.ResponseWriter, n int) {
	statuses := make([]int, n)
	for i := range statuses {
		statuses[i] = http.StatusCreated
	}
	respond207(w, statuses)
}

// runDrainCaps runs a drain with a batch capability until done fires or it times
// out, then cancels and waits so the goroutine cannot race TempDir cleanup.
func runDrainCaps(
	t *testing.T, srv *httptest.Server, caps BatchCapability, done <-chan struct{}, timeout time.Duration,
) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); Drain(ctx, srv.Client(), "PSE-TEST", caps) }()
	defer func() { cancel(); <-finished }()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// appendRaw writes a verbatim line into the queue, bypassing event.NewEvent.
// Used where the exact bytes are the thing under test.
func appendRaw(t *testing.T, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(state.OutboxPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("write outbox line: %v", err)
		}
	}
}

// waitPendingZero polls until the queue reports caught up.
func waitPendingZero(t *testing.T, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if PendingCount() == 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestBatchDeliversManyEventsInOneRequest is the throughput claim, and it is
// asserted as a REQUEST COUNT rather than as elapsed time: 50 events must cost
// one round-trip, not fifty. A timing assertion here would pass on a fast
// machine against a per-event implementation.
func TestBatchDeliversManyEventsInOneRequest(t *testing.T) {
	newBatchTest(t)

	var batchRequests, singleRequests, members int32
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testBatchPath {
			atomic.AddInt32(&singleRequests, 1)
			w.WriteHeader(http.StatusCreated)
			return
		}
		env := readEnvelope(t, r)
		atomic.AddInt32(&batchRequests, 1)
		atomic.AddInt32(&members, int32(len(env.Events)))
		allOK(w, len(env.Events))
		if atomic.LoadInt32(&members) >= 50 {
			once.Do(func() { close(done) })
		}
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	kinds := make([]string, 50)
	for i := range kinds {
		kinds[i] = "prompt"
	}
	enqueue(t, kinds...)

	if !runDrainCaps(t, srv, batchCaps(500), done, 10*time.Second) {
		t.Fatalf("batch never delivered 50 events (batches=%d members=%d)",
			atomic.LoadInt32(&batchRequests), atomic.LoadInt32(&members))
	}
	if got := atomic.LoadInt32(&batchRequests); got != 1 {
		t.Errorf("expected 50 events in ONE batch request, got %d requests", got)
	}
	if got := atomic.LoadInt32(&singleRequests); got != 0 {
		t.Errorf("expected no per-event requests, got %d", got)
	}
}

// TestBatchHonorsMaxBatchSize pins that the backend's advertised ceiling is
// obeyed rather than the whole queue being shipped in one request. Exceeding it
// is a whole-batch 400, which costs every member in it.
func TestBatchHonorsMaxBatchSize(t *testing.T) {
	newBatchTest(t)

	var maxSeen, total int32
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := readEnvelope(t, r)
		n := int32(len(env.Events))
		for {
			cur := atomic.LoadInt32(&maxSeen)
			if n <= cur || atomic.CompareAndSwapInt32(&maxSeen, cur, n) {
				break
			}
		}
		allOK(w, len(env.Events))
		if atomic.AddInt32(&total, n) >= 25 {
			once.Do(func() { close(done) })
		}
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	kinds := make([]string, 25)
	for i := range kinds {
		kinds[i] = "prompt"
	}
	enqueue(t, kinds...)

	if !runDrainCaps(t, srv, batchCaps(10), done, 10*time.Second) {
		t.Fatalf("never delivered 25 events (total=%d)", atomic.LoadInt32(&total))
	}
	if got := atomic.LoadInt32(&maxSeen); got > 10 {
		t.Errorf("sent a batch of %d against an advertised max of 10", got)
	}
}

// TestBatchFallsBackWhenBackendHasNoBatchRoute is the §4.1 acceptance, and the
// one that matters: a client that supports batching against a backend that does
// not must deliver every event individually with ZERO loss.
//
// Falsified by making the fallback advance the cursor without sending: the
// per-event count drops to 0 and this fails.
func TestBatchFallsBackWhenBackendHasNoBatchRoute(t *testing.T) {
	newBatchTest(t)

	var batchAttempts int32
	var delivered int32
	seen := make(map[string]bool)
	var mu sync.Mutex
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testBatchPath {
			atomic.AddInt32(&batchAttempts, 1)
			w.WriteHeader(http.StatusNotFound) // this backend predates batch ingest
			return
		}
		body, _ := io.ReadAll(r.Body)
		var ev struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &ev)
		mu.Lock()
		seen[ev.ID] = true
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		if atomic.AddInt32(&delivered, 1) >= 12 {
			once.Do(func() { close(done) })
		}
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	kinds := make([]string, 12)
	for i := range kinds {
		kinds[i] = "prompt"
	}
	enqueue(t, kinds...)

	if !runDrainCaps(t, srv, batchCaps(500), done, 10*time.Second) {
		t.Fatalf("events were LOST at the batch boundary: %d/12 delivered individually (batch attempts=%d)",
			atomic.LoadInt32(&delivered), atomic.LoadInt32(&batchAttempts))
	}
	if got := atomic.LoadInt32(&batchAttempts); got == 0 {
		t.Error("never probed the batch endpoint — the fallback was not exercised")
	}
	mu.Lock()
	distinct := len(seen)
	mu.Unlock()
	if distinct != 12 {
		t.Errorf("expected 12 DISTINCT events delivered individually, got %d", distinct)
	}
	if !waitPendingZero(t, 3*time.Second) {
		t.Errorf("queue not drained after fallback: %d pending", PendingCount())
	}
}

// TestBatchStopsProbingAfterFallback pins the cooldown latch. Without it every
// chunk pays a wasted 404 round-trip forever against a rolled-back backend,
// which is the slow path plus overhead rather than the slow path.
func TestBatchStopsProbingAfterFallback(t *testing.T) {
	newBatchTest(t)

	var batchAttempts, delivered int32
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testBatchPath {
			atomic.AddInt32(&batchAttempts, 1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		if atomic.AddInt32(&delivered, 1) >= 30 {
			once.Do(func() { close(done) })
		}
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	kinds := make([]string, 30)
	for i := range kinds {
		kinds[i] = "prompt"
	}
	// maxSize 5 would be six chunks, so an un-latched implementation probes six
	// times (or more, across drain passes).
	enqueue(t, kinds...)

	if !runDrainCaps(t, srv, batchCaps(5), done, 10*time.Second) {
		t.Fatalf("only %d/30 delivered", atomic.LoadInt32(&delivered))
	}
	if got := atomic.LoadInt32(&batchAttempts); got != 1 {
		t.Errorf("expected exactly ONE batch probe before the cooldown latched, got %d", got)
	}
}

// TestBatchAdvancesPastAcceptedAndRejected is the §4.2 acceptance. A member the
// backend REFUSES (400 — a kind this backend does not know, a malformed event)
// is as finished as one it stored. Leaving it unadvanced is how one skippable
// event becomes a permanently wedged queue.
//
// Falsified by treating 400 as non-advanceable: the queue never reaches zero and
// the same batch is re-sent forever.
func TestBatchAdvancesPastAcceptedAndRejected(t *testing.T) {
	newBatchTest(t)

	var requests int32
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := readEnvelope(t, r)
		atomic.AddInt32(&requests, 1)
		statuses := make([]int, len(env.Events))
		for i := range statuses {
			switch i {
			case 3:
				statuses[i] = http.StatusBadRequest // refused
			case 7:
				statuses[i] = http.StatusOK // idempotent replay, already known
			default:
				statuses[i] = http.StatusCreated
			}
		}
		respond207(w, statuses)
		once.Do(func() { close(done) })
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	kinds := make([]string, 10)
	for i := range kinds {
		kinds[i] = "prompt"
	}
	enqueue(t, kinds...)

	if !runDrainCaps(t, srv, batchCaps(500), done, 10*time.Second) {
		t.Fatal("batch was never delivered")
	}
	if !waitPendingZero(t, 3*time.Second) {
		t.Fatalf("cursor did not advance past all 10 members — %d still pending; "+
			"a rejected member is wedging the queue", PendingCount())
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("expected the batch to clear in one request, got %d — the rejected "+
			"member is being retried", got)
	}
}

// TestBatchStopsAtAMemberItCannotProve is the other half of §4.2, and it is the
// half that is easy to get wrong in the dangerous direction. A 500 (or a 403 on
// a rotated key) is NOT an answer that the member is finished with — advancing
// past it drops the event silently. The cursor must stop AT it, and the members
// before it must still be banked.
func TestBatchStopsAtAMemberItCannotProve(t *testing.T) {
	newBatchTest(t)

	var round int32
	firstChunk := make(chan int, 1)
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := readEnvelope(t, r)
		n := len(env.Events)
		statuses := make([]int, n)
		for i := range statuses {
			statuses[i] = http.StatusCreated
		}
		if atomic.AddInt32(&round, 1) == 1 {
			// Member 2 fails in a way that proves nothing. Members 0 and 1 are
			// stored and must be banked; 2 onward must come back.
			statuses[2] = http.StatusInternalServerError
			select {
			case firstChunk <- n:
			default:
			}
		} else {
			once.Do(func() { close(done) })
		}
		respond207(w, statuses)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	kinds := make([]string, 6)
	for i := range kinds {
		kinds[i] = "prompt"
	}
	enqueue(t, kinds...)

	if !runDrainCaps(t, srv, batchCaps(500), done, 15*time.Second) {
		t.Fatal("the queue never recovered past the unprovable member")
	}
	if n := <-firstChunk; n != 6 {
		t.Errorf("expected the first batch to carry all 6, got %d", n)
	}
	if !waitPendingZero(t, 5*time.Second) {
		t.Errorf("queue not drained after the retry: %d pending", PendingCount())
	}
}

// TestBatchMemberBytesAreVerbatim is the signature-integrity property, and the
// reason the envelope is spliced by hand rather than built with json.Marshal.
//
// The backend verifies each member's ed25519 signature by recomputing canonical
// JSON from the bytes it received. Event.Data is an interface{}, so a round-trip
// through encoding/json turns every number into a float64: 1234567890123456789
// comes back as 1234567890123456800 and verification fails — for the whole batch
// at once. The assertion is byte equality against what was queued, not a
// semantic comparison, because a semantic one passes on exactly the corruption
// it is meant to catch.
func TestBatchMemberBytesAreVerbatim(t *testing.T) {
	newBatchTest(t)

	const bigNumberEvent = `{"id":"ev-big","kind":"prompt","ts":"2026-08-04T12:00:00Z",` +
		`"sessionId":"s1","data":{"n":1234567890123456789},"sig":"abc"}`

	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := readEnvelope(t, r)
		if len(env.Events) > 0 {
			select {
			case got <- string(env.Events[0]):
			default:
			}
		}
		allOK(w, len(env.Events))
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	appendRaw(t, bigNumberEvent)

	var seen string
	captured := make(chan struct{})
	go func() { seen = <-got; close(captured) }()
	_ = runDrainCaps(t, srv, batchCaps(500), captured, 10*time.Second)

	select {
	case <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("batch member never arrived")
	}
	if seen != bigNumberEvent {
		t.Errorf("member bytes were NOT shipped verbatim — the signature would fail.\n"+
			"queued: %s\n  sent: %s", bigNumberEvent, seen)
	}
}

// TestBatchSkipsUnsendableLinesLocally pins that a blank or malformed queue line
// never reaches the envelope. It cannot: the backend rejects a malformed
// ENVELOPE wholesale, so one unparseable line would take every member alongside
// it down. They still have to be advanced past, or they wedge the queue.
func TestBatchSkipsUnsendableLinesLocally(t *testing.T) {
	newBatchTest(t)
	warnOut = io.Discard
	t.Cleanup(func() { warnOut = os.Stderr })

	members := make(chan []string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := readEnvelope(t, r)
		ids := make([]string, 0, len(env.Events))
		for _, e := range env.Events {
			var ev struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(e, &ev)
			ids = append(ids, ev.ID)
		}
		select {
		case members <- ids:
		default:
		}
		allOK(w, len(env.Events))
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	appendRaw(t,
		`{"id":"good-1","kind":"prompt","ts":"2026-08-04T12:00:00Z"}`,
		``,
		`{"id":"good-2" NOT JSON`,
		`{"id":"good-3","kind":"prompt","ts":"2026-08-04T12:00:02Z"}`,
	)

	captured := make(chan struct{})
	var ids []string
	go func() { ids = <-members; close(captured) }()
	_ = runDrainCaps(t, srv, batchCaps(500), captured, 10*time.Second)

	select {
	case <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("no batch arrived")
	}
	if len(ids) != 2 {
		t.Errorf("expected exactly the 2 sendable events in the envelope, got %d (%v)", len(ids), ids)
	}
	if !waitPendingZero(t, 3*time.Second) {
		t.Errorf("unsendable lines wedged the queue: %d pending", PendingCount())
	}
}

// TestBatchEnvelopeRejectionFallsBackRatherThanDropping is the failure mode with
// the worst payoff. A 400 on the ENVELOPE names no member, so nothing in the
// chunk has a verdict. Skipping the chunk would drop every event in it at once;
// retrying the identical bytes can never succeed. Falling back gives each event
// its own verdict, so a genuinely bad one is skipped ALONE.
func TestBatchEnvelopeRejectionFallsBackRatherThanDropping(t *testing.T) {
	newBatchTest(t)
	warnOut = io.Discard
	t.Cleanup(func() { warnOut = os.Stderr })

	var delivered int32
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testBatchPath {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"Invalid batch envelope"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		if atomic.AddInt32(&delivered, 1) >= 8 {
			once.Do(func() { close(done) })
		}
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	kinds := make([]string, 8)
	for i := range kinds {
		kinds[i] = "prompt"
	}
	enqueue(t, kinds...)

	if !runDrainCaps(t, srv, batchCaps(500), done, 10*time.Second) {
		t.Fatalf("a whole-envelope 400 DROPPED events: only %d/8 were delivered individually",
			atomic.LoadInt32(&delivered))
	}
}

// TestBatchHonorsRetryAfterOn429 pins that the lane budget's own signal survives
// the move to batching. The backend charges a batch in EVENTS, so a large chunk
// can exhaust a window on its own; honouring Retry-After is what keeps the drain
// from converting that into a retry storm.
func TestBatchHonorsRetryAfterOn429(t *testing.T) {
	newBatchTest(t)

	var attempts int32
	var gap time.Duration
	var first time.Time
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := readEnvelope(t, r)
		if atomic.AddInt32(&attempts, 1) == 1 {
			first = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		gap = time.Since(first)
		allOK(w, len(env.Events))
		once.Do(func() { close(done) })
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	enqueue(t, "prompt", "prompt", "prompt")

	if !runDrainCaps(t, srv, batchCaps(500), done, 15*time.Second) {
		t.Fatal("batch never recovered from the 429")
	}
	if gap < 900*time.Millisecond {
		t.Errorf("retried after %s, ignoring the server's Retry-After of 1s", gap)
	}
	if !waitPendingZero(t, 3*time.Second) {
		t.Errorf("queue not drained after the 429: %d pending", PendingCount())
	}
}

// TestAdvanceOverStopsAtAnUnansweredMember is a unit-level guard on the rule
// that silence is not acceptance. A truncated results array is a backend we do
// not understand, and the only safe reading of a missing row is that the event
// did not land. Asserted here rather than through the drain because the drain's
// response cannot distinguish "did not advance" from "advanced and re-sent".
func TestAdvanceOverStopsAtAnUnansweredMember(t *testing.T) {
	chunk := []chunkLine{
		{size: 10, body: []byte(`{"a":1}`)},
		{size: 20, body: []byte(`{"b":2}`)},
		{size: 30, body: []byte(`{"c":3}`)},
	}
	// The backend answered for two of three.
	results := []batchResultForTest{{Index: 0, Status: 201}, {Index: 1, Status: 400}}

	advance, lines := advanceOver(chunk, toMemberResults(results))
	if advance != 30 {
		t.Errorf("expected the cursor to advance 30 bytes (members 0 and 1 only), got %d", advance)
	}
	if lines != 2 {
		t.Errorf("expected 2 lines covered, got %d", lines)
	}
}

// TestMemberAdvanceableMatchesTheSingleEventRule pins the per-member statuses
// against the rule single delivery has always used. A 403 is the one most likely
// to be waved through by accident, and it is exactly a rotated or revoked
// engineer key — advancing past it silently discards capture.
func TestMemberAdvanceableMatchesTheSingleEventRule(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
		why    string
	}{
		{200, true, "already known — idempotent replay"},
		{201, true, "stored"},
		{400, true, "rejected, permanently unsendable"},
		{422, true, "rejected, permanently unsendable"},
		{403, false, "device key refused — retry, never drop"},
		{429, false, "rate limited — retry"},
		{500, false, "backend fault — retry"},
		{0, false, "no answer at all"},
	} {
		if got := memberAdvanceable(tc.status); got != tc.want {
			t.Errorf("memberAdvanceable(%d) = %v, want %v (%s)", tc.status, got, tc.want, tc.why)
		}
	}
}

// --- small shims so the unit tests above read in terms of the wire shape ------

type batchResultForTest struct {
	Index  int
	Status int
}

func toMemberResults(in []batchResultForTest) []ingest.BatchMemberResult {
	out := make([]ingest.BatchMemberResult, 0, len(in))
	for _, r := range in {
		out = append(out, ingest.BatchMemberResult{Index: r.Index, Status: r.Status})
	}
	return out
}

// TestBatchBodyCapDoesNotSkipEvents is the regression Greptile caught on #152,
// and it is the worst failure class this package has: an event consumed from the
// queue, never sent, and the cursor advanced past it anyway.
//
// The byte cap is the only path that reads a line it cannot use. bufio cannot
// put a whole line back, so the first version simply returned — leaving the
// shared reader positioned past the dropped line while the cursor had not
// covered it. The next chunk's bytes were then added to a cursor short by the
// dropped line's length, so the cursor landed MID-LINE: the skipped event was
// lost outright and its neighbour was truncated into garbage.
//
// Falsified by restoring the silent return: 2 of 4 events arrive.
func TestBatchBodyCapDoesNotSkipEvents(t *testing.T) {
	newBatchTest(t)
	warnOut = io.Discard
	t.Cleanup(func() { warnOut = os.Stderr })

	// Small enough that two events never fit in one request, so every chunk
	// after the first is forced through the cap path.
	origCap := batchMaxBodyBytes
	batchMaxBodyBytes = 90
	t.Cleanup(func() { batchMaxBodyBytes = origCap })

	want := []string{"cap-1", "cap-2", "cap-3", "cap-4"}
	lines := make([]string, 0, len(want))
	for i, id := range want {
		lines = append(lines, fmt.Sprintf(
			`{"id":%q,"kind":"prompt","ts":"2026-08-04T12:00:0%dZ","sessionId":"s1"}`, id, i))
	}
	appendRaw(t, lines...)

	var mu sync.Mutex
	seen := map[string]bool{}
	done := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := readEnvelope(t, r)
		mu.Lock()
		for _, e := range env.Events {
			var ev struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(e, &ev)
			seen[ev.ID] = true
		}
		n := len(seen)
		mu.Unlock()
		allOK(w, len(env.Events))
		if n == len(want) {
			once.Do(func() { close(done) })
		}
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	if !runDrainCaps(t, srv, batchCaps(500), done, 15*time.Second) {
		mu.Lock()
		got := make([]string, 0, len(seen))
		for id := range seen {
			got = append(got, id)
		}
		mu.Unlock()
		sort.Strings(got)
		t.Fatalf("events were consumed but never sent — the body cap skipped some.\n"+
			"want all of %v, the backend only ever saw %v", want, got)
	}

	if !waitPendingZero(t, 5*time.Second) {
		t.Errorf("queue not drained: %d pending", PendingCount())
	}
	// A misaligned cursor shows up here: it would sit mid-line rather than at EOF.
	cursor := readCursor()
	fi, err := os.Stat(state.OutboxPath())
	if err != nil {
		t.Fatalf("stat outbox: %v", err)
	}
	if cursor != 0 && cursor != fi.Size() {
		t.Errorf("cursor %d is neither compacted-to-0 nor at EOF %d — it is misaligned "+
			"inside a line, which corrupts every later read", cursor, fi.Size())
	}
}
