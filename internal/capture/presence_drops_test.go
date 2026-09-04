package capture

import (
	"errors"
	"os"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// TestPresenceReportsAMeasuredZeroDropCount is the contract that makes the whole
// field worth having.
//
// A reported 0 is a MEASUREMENT — the fleet saying "we are not losing anything",
// which is the most common and most valuable value this field will ever carry.
// An OMITTED field is indistinguishable from a client too old to count, and the
// server has no way to tell them apart except by what arrives. `omitempty` here
// would erase every healthy report and leave us exactly where the incident found
// us: unable to tell "nothing is being dropped" from "we cannot see drops".
func TestPresenceReportsAMeasuredZeroDropCount(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	data := buildPresenceEvent(Session{DeviceID: "dev-1"}).Data.(map[string]interface{})

	for _, field := range []string{"droppedEvents", "outboxBytes", "outboxCapacityBytes"} {
		if _, present := data[field]; !present {
			t.Errorf("%s absent on a healthy device — a measured zero must reach the server", field)
		}
	}
	// eventDataMap round-trips through JSON, so every number arrives as float64.
	if got := num(t, data, "droppedEvents"); got != 0 {
		t.Errorf("droppedEvents = %v, want 0", got)
	}
	if got := num(t, data, "outboxBytes"); got != 0 {
		t.Errorf("outboxBytes = %v, want 0 on an empty queue", got)
	}
	// The DIVISOR rides with the dividend. A server that hard-codes the cap
	// silently mis-judges any client whose cap changed, and OutboxMaxBytes is a
	// constant in a repo that ships independently of the backend.
	if got := num(t, data, "outboxCapacityBytes"); got != float64(outbox.OutboxMaxBytes) {
		t.Errorf("outboxCapacityBytes = %v, want %d", got, int64(outbox.OutboxMaxBytes))
	}
}

// TestPresenceCarriesTheDropCount pins the number actually reaching the beat,
// end to end from the discard that produced it.
func TestPresenceCarriesTheDropCount(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)

	// Push the live lane over its cap, then append — which drops.
	f, err := os.OpenFile(state.OutboxPath(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	if err := f.Truncate(outbox.OutboxMaxBytes + 1); err != nil {
		t.Fatalf("grow outbox: %v", err)
	}
	f.Close()
	if err := outbox.Append(event.NewEvent("prompt", "sess-test")); !errors.Is(err, outbox.ErrQueueFull) {
		t.Fatalf("Append onto a full lane = %v, want ErrQueueFull", err)
	}

	data := buildPresenceEvent(Session{DeviceID: "dev-1"}).Data.(map[string]interface{})
	if got := num(t, data, "droppedEvents"); got != 1 {
		t.Errorf("droppedEvents = %v, want 1", got)
	}
	// And the leading indicator agrees with the lagging one: the lane that
	// dropped is at or past its cap right now. This pairing is what lets the
	// backend say "losing events NOW" from a cumulative lifetime counter without
	// storing any history.
	if got := num(t, data, "outboxBytes"); got < float64(outbox.OutboxMaxBytes) {
		t.Errorf("outboxBytes = %v, want >= %d — the lane that dropped must read as full",
			got, int64(outbox.OutboxMaxBytes))
	}
}

// num reads a beat field as a number. Every integer on the beat arrives as
// float64: eventDataMap marshals the payload to JSON and back so the redaction
// projector has a map to work on (see buildPresenceEvent), and encoding/json has
// exactly one number type.
func num(t *testing.T, data map[string]interface{}, field string) float64 {
	t.Helper()
	v, ok := data[field].(float64)
	if !ok {
		t.Fatalf("%s is %T (%v), want a number", field, data[field], data[field])
	}
	return v
}

// TestDropFieldsSurviveTheRedactionProjector pins the trap that has bitten this
// rail before. `redact.ProjectEvent` default-DENIES: a field the CLI sends and
// the allowlist does not name is stripped SILENTLY, the beat still returns 201,
// and the numbers simply never arrive. That is how `os`/`arch`/`watching` were
// dropped for months while every surface looked healthy.
//
// Verified to fail first: removing "droppedEvents" from the presence entry in
// internal/redact/project.go turns this red and nothing else in the suite.
func TestDropFieldsSurviveTheRedactionProjector(t *testing.T) {
	fields := []string{"droppedEvents", "outboxBytes", "outboxCapacityBytes"}
	for _, kind := range []string{"presence", "heartbeat"} {
		e := &event.Event{Kind: kind, Data: map[string]interface{}{
			"device":              "dev-1",
			"cliVersion":          "0.27.0",
			"droppedEvents":       12,
			"outboxBytes":         67108864,
			"outboxCapacityBytes": 67108864,
		}}
		redact.ProjectEvent(e, false)
		out, ok := e.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("%s: Data is %T after projection", kind, e.Data)
		}
		for _, f := range fields {
			if _, ok := out[f]; !ok {
				t.Errorf("%s: %s stripped by the projector", kind, f)
			}
		}
	}
}
