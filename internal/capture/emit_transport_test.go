package capture

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
)

// TestCensusIsQueuedAndPresenceIsNot pins the deliberate asymmetry between the
// two background emitters. Both still go through the signed ledger; they differ
// only in HOW they ship.
//
//   - config_census is QUEUED. It is emitted at most once per 24h and its cursor
//     (saveLastCensusAt) advances regardless of the send, so an inline POST that
//     hit a 429 lost the whole census for a day and fleet-health's "no census"
//     signal fired for a device that had collected one. Durability is exactly
//     right for a rare, expensive, non-time-sensitive event.
//   - presence is NOT queued. A heartbeat is a liveness claim stamped with its
//     own ts; redelivering it minutes later asserts "I was alive at 10:04" as
//     though it were news. Dropping a failed heartbeat is the correct semantic —
//     the next one is seconds away and carries a truthful timestamp.
//
// If you are here because you "fixed" presence to use the outbox, read the
// comment on emitPresenceEvent first.
func TestCensusIsQueuedAndPresenceIsNot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))

	// A backend that fails every request: whatever ships inline is LOST, and
	// whatever is queued survives.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	session := Session{DeviceID: "sess-emit", SessionToken: "PSE-TEST", TaskRoot: tmp}

	emitPresenceEvent(session)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("presence must POST inline (fire-and-forget); got %d request(s)", got)
	}
	if n := outbox.PendingCount(); n != 0 {
		t.Errorf("presence must NOT be queued — a replayed heartbeat is a stale liveness claim; pending = %d", n)
	}

	emitConfigCensus(session)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("census must not POST inline; extra request(s) observed (total %d)", got)
	}
	if n := outbox.PendingCount(); n != 1 {
		t.Errorf("census must be durably queued so a 429 cannot lose a day of it; pending = %d, want 1", n)
	}
}
