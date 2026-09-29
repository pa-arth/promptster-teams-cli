package capture

import (
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"os"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

func beatVendorPollOkAt(t *testing.T) string {
	t.Helper()
	e := buildPresenceEvent(Session{DeviceID: "dev"})
	redact.ProjectEvent(&e, false)
	v, ok := e.Data.(map[string]interface{})["cursorVendorLastPollOkAt"]
	if !ok {
		t.Fatal("heartbeat omits cursorVendorLastPollOkAt; it must always be present")
	}
	return v.(string)
}

// Only a fully queued current-period snapshot advances the stamp. Absences and
// a failed append leave it where it was, so the heartbeat can show a collector
// that keeps running but never succeeds.
func TestCursorVendorPollOkAtAdvancesOnlyOnSuccess(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	resolver := permittedCursorPolicy(t)
	client, _ := fakeVendor(t)
	captureVendorEvents(t)

	if got := beatVendorPollOkAt(t); got != "" {
		t.Fatalf("fresh machine: %q, want \"\" (never polled ok)", got)
	}

	// Absence: an expired login.
	ideLogin(t, "auth0|expired_login", "old@example.com", 1000000000)
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	if got := beatVendorPollOkAt(t); got != "" {
		t.Fatalf("after an absence: %q, want \"\"", got)
	}

	// Valid login, but the outbox append fails: queuedAll is false.
	ideLogin(t, "auth0|login_one", "one@example.com", 4102444800)
	prev := queueCursorVendorEvent
	queueCursorVendorEvent = func(event.Event) bool { return false }
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	queueCursorVendorEvent = prev
	if got := beatVendorPollOkAt(t); got != "" {
		t.Fatalf("after a failed append: %q, want \"\"", got)
	}

	// Success.
	before := time.Now().Add(-time.Second)
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	got := beatVendorPollOkAt(t)
	at, err := time.Parse(time.RFC3339, got)
	if err != nil || at.Before(before.Truncate(time.Second)) || at.Location() != time.UTC {
		t.Fatalf("after a success: %q (err %v), want a fresh RFC3339 UTC stamp", got, err)
	}

	// A later absence does not move it.
	ideLogin(t, "auth0|expired_login", "old@example.com", 1000000000)
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	if again := beatVendorPollOkAt(t); again != got {
		t.Fatalf("absence moved the stamp: %q -> %q", got, again)
	}
}

func TestCursorVendorPollPersistenceErrors(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	if at, err := loadCursorVendorPollOkAt(); err != nil || !at.IsZero() {
		t.Fatalf("missing: %v %v", at, err)
	}
	for _, data := range []string{"broken", `{"v":99,"lastOkAt":"2026-09-29T00:00:00Z"}`, `{"v":1}`} {
		if err := os.WriteFile(cursorVendorPollPath(), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadCursorVendorPollOkAt(); err == nil {
			t.Fatalf("invalid stamp accepted: %s", data)
		}
	}
	if err := os.Remove(cursorVendorPollPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cursorVendorPollPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCursorVendorPollOkAt(); err == nil {
		t.Fatal("read error hidden")
	}
	if err := recordCursorVendorPollOk(time.Now()); err == nil {
		t.Fatal("rename error hidden")
	}
}
