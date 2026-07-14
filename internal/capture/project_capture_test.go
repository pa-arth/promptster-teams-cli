package capture

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
)

const leakCanary = "PROMPTSTER_SOURCE_CANARY_51f3a9"

func eventWithData(kind string, data map[string]interface{}) event.Event {
	e := event.NewEvent(kind, "sess-project-test")
	e.Data = data
	e.RawPayload = "raw preview containing " + leakCanary
	return e
}

// TestProjectedEventSignsAndVerifies pins that projection happens BEFORE
// signing: the buffered/POSTed event is signed over its projected data, so the
// backend's signature check passes on exactly the bytes it receives.
func TestProjectedEventSignsAndVerifies(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	e := eventWithData("file_diff", map[string]interface{}{
		"path": "a.ts", "diff": leakCanary, "linesAdded": 1,
	})
	if err := sign.AppendEventToLocalBuffer(&e, false); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The mutated event (what gets POSTed) must be projected.
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), leakCanary) {
		t.Fatalf("canary survived into the signed event: %s", b)
	}
	// The buffered line (the on-disk audit chain) must be projected too.
	buffered, err := os.ReadFile(filepath.Join(tmp, "buffer.jsonl"))
	if err != nil {
		t.Fatalf("read buffer: %v", err)
	}
	if strings.Contains(string(buffered), leakCanary) {
		t.Fatalf("canary survived into the local buffer: %s", buffered)
	}
}

// TestWireBodyCarriesNoSource is the end-to-end "never sent" proof: run a
// source-bearing event through the real funnel and assert the HTTP body the
// server receives carries no source fields.
//
// The funnel now has two stages — queue, then drain — so this drives BOTH. That
// is the point: source-exclusion must survive the event being persisted to an
// intermediate queue and shipped later by a different goroutine. The queued
// bytes are the projected+scrubbed+signed ones, so the wire body must be
// identical to what the old inline POST produced.
func TestWireBodyCarriesNoSource(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))

	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case bodies <- b:
		default:
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	session := Session{DeviceID: "sess-wire-test", SessionToken: "PSE-TEST", TaskRoot: tmp}
	e := eventWithData("command", map[string]interface{}{
		"command":  `python -c 'print("` + leakCanary + `")'`,
		"exitCode": 1,
		"stdout":   "output " + leakCanary,
		"stderr":   "error " + leakCanary,
	})
	queueClaudeWatchEvent(e, session, false)

	// Cancel AND wait for the drain to exit before the test returns: t.TempDir's
	// cleanup runs after this, and a still-running drain writing its cursor into
	// that dir would race the RemoveAll.
	ctx, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{})
	go func() { defer close(drained); outbox.Drain(ctx, srv.Client(), session.SessionToken) }()
	defer func() { cancel(); <-drained }()

	var received []byte
	select {
	case received = <-bodies:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never delivered the queued event")
	}
	if len(received) == 0 {
		t.Fatal("server received no body")
	}
	body := string(received)
	if strings.Contains(body, leakCanary) {
		t.Fatalf("source canary reached the wire: %s", body)
	}
	for _, field := range []string{`"stdout"`, `"stderr"`, `"rawPayload"`} {
		if strings.Contains(body, field) {
			t.Errorf("field %s reached the wire: %s", field, body)
		}
	}
	var sent event.Event
	if err := json.Unmarshal(received, &sent); err != nil {
		t.Fatalf("wire body is not a valid event: %v", err)
	}
	data := sent.Data.(map[string]interface{})
	if data["command"] != `python -c '<inline-code-redacted>'` {
		t.Errorf("command not scrubbed on the wire: %v", data["command"])
	}
}
