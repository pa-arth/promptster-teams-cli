package capture

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// TestIngestClaudeWatchEventToleratesRejection pins the 400-tolerance
// contract: a backend that rejects a kind (e.g. subagent_usage/config_census
// before the accepting backend deploys) must NOT count as a channel failure —
// otherwise sustained rejections would trip the degraded state machine and
// silently stop ALL capture. Transport/5xx failures still count as failures.
func TestIngestClaudeWatchEventToleratesRejection(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	session := Session{DeviceID: "dev-test", SessionToken: "PSE-TEST", TaskRoot: tmp}

	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Invalid hook event"}`))
	}))
	defer rejecting.Close()
	t.Setenv("PROMPTSTER_API_URL", rejecting.URL)
	if !ingestClaudeWatchEvent(event.NewEvent("subagent_usage", session.DeviceID), session, rejecting.Client(), false) {
		t.Error("4xx rejection must count as handled (no degradation, no retry)")
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	t.Setenv("PROMPTSTER_API_URL", failing.URL)
	if ingestClaudeWatchEvent(event.NewEvent("subagent_usage", session.DeviceID), session, failing.Client(), false) {
		t.Error("5xx must still count as a send failure")
	}
}
