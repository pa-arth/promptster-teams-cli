package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// The presence beat now carries this device's own outbox backlog.
//
// WHY. On 2026-08-04 a manager was told an engineer had ZERO active sessions for
// over an hour while that engineer worked the whole time. The CLI had crossed a
// progress-schema bump and was replaying 28 days of transcripts through a
// strict-FIFO outbox at one event per POST, so the events landing were dated
// three weeks earlier and the server's monotonic `last_activity_at` correctly
// refused to move. Ingest ran at 100% 2xx throughout. Nothing on the wire could
// distinguish "connected and idle" from "connected and hours behind", and the
// device was the only party that knew.
//
// Every test below fails against the pre-change code: `presenceData` had no such
// fields and `redact.ProjectEvent` would have stripped them.

// writeOutbox seeds a queue file with the given event timestamps and a cursor of
// `delivered` bytes, mimicking a partially-drained outbox.
func writeOutbox(t *testing.T, dir string, delivered int, timestamps ...string) {
	t.Helper()
	path := filepath.Join(dir, "outbox.jsonl")
	body := ""
	for i, ts := range timestamps {
		line, err := json.Marshal(map[string]any{
			"id": "evt", "kind": "prompt", "ts": ts, "sessionId": "s",
		})
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		body += string(line) + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write outbox: %v", err)
	}
	if delivered > 0 {
		cursorPath := filepath.Join(dir, "outbox.jsonl.cursor")
		if err := os.WriteFile(cursorPath, []byte(strconv.Itoa(delivered)), 0o600); err != nil {
			t.Fatalf("write cursor: %v", err)
		}
	}
}

func TestPendingStateReportsCountAndOldest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	writeOutbox(t, dir, 0,
		"2026-08-04T14:10:00Z",
		"2026-07-11T01:51:29Z",
		"2026-08-04T14:11:00Z",
	)

	got := outbox.PendingStateNow()
	if got.Count != 3 {
		t.Fatalf("Count = %d, want 3", got.Count)
	}
	// THE ASSERTION THAT MATTERS. Not the head's timestamp — the MINIMUM. Append
	// order stopped being chronological when history replay went newest-first, so
	// during a backfill the head of the queue is recent work and the three-week-old
	// events sit behind it. Reading the head would report the lag as one minute
	// during exactly the episode this field exists to describe.
	want, _ := time.Parse(time.RFC3339, "2026-07-11T01:51:29Z")
	if !got.Oldest.Equal(want) {
		t.Fatalf("Oldest = %v, want %v", got.Oldest, want)
	}
}

func TestPendingStateIgnoresDeliveredLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	old := `{"id":"evt","kind":"prompt","ts":"2026-07-11T01:51:29Z","sessionId":"s"}` + "\n"
	recent := `{"id":"evt","kind":"prompt","ts":"2026-08-04T14:10:00Z","sessionId":"s"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "outbox.jsonl"), []byte(old+recent), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Cursor past the first line: it is DELIVERED and must not count toward the
	// backlog or drag the reported age three weeks into the past. A device that
	// has caught up must be able to say so.
	if err := os.WriteFile(filepath.Join(dir, "outbox.jsonl.cursor"), []byte(strconv.Itoa(len(old))), 0o600); err != nil {
		t.Fatalf("cursor: %v", err)
	}

	got := outbox.PendingStateNow()
	if got.Count != 1 {
		t.Fatalf("Count = %d, want 1", got.Count)
	}
	want, _ := time.Parse(time.RFC3339, "2026-08-04T14:10:00Z")
	if !got.Oldest.Equal(want) {
		t.Fatalf("Oldest = %v, want %v", got.Oldest, want)
	}
}

func TestPendingStateCountsUnparseableLinesButTakesNoTimestamp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	body := "{not json\n" +
		`{"id":"e","kind":"prompt","ts":"2026-08-04T14:10:00Z","sessionId":"s"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "outbox.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := outbox.PendingStateNow()
	// A line we cannot parse is still undelivered work. Dropping it from the count
	// would understate a real backlog, which is the direction of error that hides
	// an outage rather than inventing one.
	if got.Count != 2 {
		t.Fatalf("Count = %d, want 2", got.Count)
	}
	want, _ := time.Parse(time.RFC3339, "2026-08-04T14:10:00Z")
	if !got.Oldest.Equal(want) {
		t.Fatalf("Oldest = %v, want %v", got.Oldest, want)
	}
}

func TestPendingStateSurvivesACompactionUnderneathIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	// The race, reproduced as its aftermath: the drain compacted (truncate to 0,
	// cursor reset) and the watchers appended fresh work, but this reader is
	// holding the pre-compaction cursor. It now points far past EOF.
	writeOutbox(t, dir, 0, "2026-08-04T14:10:00Z", "2026-08-04T14:11:00Z", "2026-08-04T14:12:00Z")
	if err := os.WriteFile(filepath.Join(dir, "outbox.jsonl.cursor"), []byte("5000000"), 0o600); err != nil {
		t.Fatalf("cursor: %v", err)
	}

	got := outbox.PendingStateNow()
	// Seeking past EOF SUCCEEDS and reads nothing, so the unguarded version
	// reported 0 — a measured "this device is caught up" while three events sat
	// queued. Reporting a false zero during a backlog is the single failure this
	// field exists to prevent, which is why it is worth a branch.
	if got.Count != 3 {
		t.Fatalf("Count = %d, want 3 — a cursor past EOF must rewind, not read as caught-up", got.Count)
	}
	want, _ := time.Parse(time.RFC3339, "2026-08-04T14:10:00Z")
	if !got.Oldest.Equal(want) {
		t.Fatalf("Oldest = %v, want %v", got.Oldest, want)
	}
}

func TestPendingStateOnEmptyQueue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	got := outbox.PendingStateNow()
	if got.Count != 0 || !got.Oldest.IsZero() {
		t.Fatalf("empty queue = %+v, want {0 zero}", got)
	}
}

func TestPresenceEventCarriesTheBacklog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	writeOutbox(t, dir, 0, "2026-07-11T01:51:29Z", "2026-08-04T14:10:00Z")

	ev := buildPresenceEvent(Session{DeviceID: "dev-1"})
	data, ok := ev.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("Data is %T, want map", ev.Data)
	}
	if got := data["pendingEvents"]; got != 2 && got != float64(2) {
		t.Fatalf("pendingEvents = %v (%T), want 2", got, got)
	}
	oldest, _ := data["pendingOldestEventAt"].(string)
	if oldest == "" {
		t.Fatal("pendingOldestEventAt missing — the count alone cannot tell a busy afternoon from an outage")
	}
	parsed, err := time.Parse(time.RFC3339Nano, oldest)
	if err != nil {
		t.Fatalf("pendingOldestEventAt %q is not RFC3339: %v", oldest, err)
	}
	if parsed.UTC().Format(time.RFC3339) != "2026-07-11T01:51:29Z" {
		t.Fatalf("pendingOldestEventAt = %s, want 2026-07-11T01:51:29Z", oldest)
	}
}

func TestPresenceEventReportsAMEASUREDZeroWithNoStaleAge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)

	ev := buildPresenceEvent(Session{DeviceID: "dev-1"})
	data := ev.Data.(map[string]interface{})
	// PRESENT AND ZERO, never absent. A caught-up device must SAY it is caught up:
	// the server distinguishes a reported 0 from silence, and a client that omits
	// the field is indistinguishable from one too old to report — which puts the
	// whole fleet back to being unreadable.
	got, present := data["pendingEvents"]
	if !present {
		t.Fatal("pendingEvents absent on an empty queue — a measured zero must reach the server")
	}
	if got != 0 && got != float64(0) {
		t.Fatalf("pendingEvents = %v, want 0", got)
	}
	// And no age, because there is nothing to be behind on. Carrying a stale
	// timestamp next to a count of 0 would leave a phantom outage standing.
	if v, ok := data["pendingOldestEventAt"]; ok && v != "" {
		t.Fatalf("pendingOldestEventAt = %v on an empty queue, want absent", v)
	}
}

func TestPendingFieldsSurviveTheRedactionProjector(t *testing.T) {
	// THE TRAP THIS PINS. `redact.ProjectEvent` default-DENIES: a field the
	// CLI sends that the allowlist does not name is stripped silently, the beat
	// still returns 201, and the numbers simply never arrive. That is exactly how
	// `os`/`arch`/`watching` were dropped for months while looking healthy.
	//
	// Verified to fail first: removing "pendingEvents" from the presence entry in
	// internal/redact/project.go turns this red and nothing else in the suite.
	for _, kind := range []string{"presence", "heartbeat"} {
		e := &event.Event{Kind: kind, Data: map[string]interface{}{
			"device":               "dev-1",
			"cliVersion":           "0.12.4",
			"pendingEvents":        42,
			"pendingOldestEventAt": "2026-07-11T01:51:29Z",
		}}
		redact.ProjectEvent(e, false)
		out, ok := e.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("%s: Data is %T after projection", kind, e.Data)
		}
		if _, ok := out["pendingEvents"]; !ok {
			t.Fatalf("%s: pendingEvents stripped by the projector", kind)
		}
		if _, ok := out["pendingOldestEventAt"]; !ok {
			t.Fatalf("%s: pendingOldestEventAt stripped by the projector", kind)
		}
	}
}
