package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestIsIngestRejection(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusBadRequest:          true,  // unknown kind / schema reject
		http.StatusUnprocessableEntity: true,  // validation reject
		http.StatusUnauthorized:        false, // auth problem — real failure
		http.StatusForbidden:           false,
		http.StatusTooManyRequests:     false,
		http.StatusInternalServerError: false,
		http.StatusServiceUnavailable:  false,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		t.Setenv("PROMPTSTER_API_URL", srv.URL)
		err := ingestEventWithClient(srv.Client(), newEvent("config_census", "sess-1"), "PSE-TEST")
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", status)
		}
		if got := isIngestRejection(err); got != want {
			t.Errorf("status %d: isIngestRejection = %v, want %v", status, got, want)
		}
	}
	if isIngestRejection(nil) {
		t.Error("nil error must not be a rejection")
	}
}

// TestIngestClaudeWatchEventToleratesRejection pins the 400-tolerance
// contract: a backend that rejects a kind (e.g. subagent_usage/config_census
// before the accepting backend deploys) must NOT count as a channel failure —
// otherwise sustained rejections would trip the degraded state machine and
// silently stop ALL capture. Transport/5xx failures still count as failures.
func TestIngestClaudeWatchEventToleratesRejection(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	session := Session{SessionID: "dev-test", SessionToken: "PSE-TEST", TaskRoot: tmp}

	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Invalid hook event"}`))
	}))
	defer rejecting.Close()
	t.Setenv("PROMPTSTER_API_URL", rejecting.URL)
	if !ingestClaudeWatchEvent(newEvent("subagent_usage", session.SessionID), session, rejecting.Client()) {
		t.Error("4xx rejection must count as handled (no degradation, no retry)")
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	t.Setenv("PROMPTSTER_API_URL", failing.URL)
	if ingestClaudeWatchEvent(newEvent("subagent_usage", session.SessionID), session, failing.Client()) {
		t.Error("5xx must still count as a send failure")
	}
}
