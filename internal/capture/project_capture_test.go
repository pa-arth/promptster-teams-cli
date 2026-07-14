package capture

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
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
// source-bearing event through the real funnel (buffer + POST) and assert the
// HTTP body the server receives carries no source fields.
func TestWireBodyCarriesNoSource(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
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
	if !ingestClaudeWatchEvent(e, session, srv.Client(), false) {
		t.Fatal("event was not sent")
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
