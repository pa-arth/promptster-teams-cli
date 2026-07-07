package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
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
		err := IngestEventWithClient(srv.Client(), event.NewEvent("config_census", "sess-1"), "PSE-TEST")
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", status)
		}
		if got := IsIngestRejection(err); got != want {
			t.Errorf("status %d: isIngestRejection = %v, want %v", status, got, want)
		}
	}
	if IsIngestRejection(nil) {
		t.Error("nil error must not be a rejection")
	}
}
