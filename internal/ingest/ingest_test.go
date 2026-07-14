package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// TestParseRetryAfter pins bug 3's plumbing: the backend sends Retry-After
// (seconds) on 429 and the old client discarded the whole header set. Anything
// unparseable must yield 0 so the caller falls back to its own backoff rather
// than retrying instantly or sleeping on a bogus value.
func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{name: "delta seconds", in: "5", want: 5 * time.Second},
		{name: "whitespace tolerated", in: "  3 ", want: 3 * time.Second},
		{name: "absent", in: "", want: 0},
		{name: "garbage", in: "soon", want: 0},
		{name: "zero means no useful delay", in: "0", want: 0},
		{name: "negative is ignored", in: "-5", want: 0},
		{name: "past HTTP-date is ignored", in: "Mon, 02 Jan 2006 15:04:05 GMT", want: 0},
		{name: "absurd value is capped", in: "999999", want: retryAfterCap},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.in); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsRateLimitedSurfacesRetryAfter proves the delay actually reaches the
// drain through the typed error, and that non-429s are not mistaken for it.
func TestIsRateLimitedSurfacesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	err := IngestEventWithClient(srv.Client(), event.NewEvent("prompt", "s1"), "PSE-TEST")
	if err == nil {
		t.Fatal("expected a 429 error")
	}
	d, limited := IsRateLimited(err)
	if !limited {
		t.Fatal("a 429 must be reported as rate limited")
	}
	if d != 7*time.Second {
		t.Errorf("Retry-After = %v, want 7s", d)
	}
	if IsIngestRejection(err) {
		t.Error("a 429 must NOT be treated as a schema rejection — that would drop the event")
	}
}
