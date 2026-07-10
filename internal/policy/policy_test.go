package policy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// setup points the resolver at a test server + a scratch state dir.
func setup(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("PROMPTSTER_API_URL", srv.URL)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
}

func TestResolverOptedIn(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "PSE-TEST" {
			t.Errorf("missing/wrong X-API-Key: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"captureAssistantProse":true}`))
	})
	r := NewResolver("PSE-TEST")
	r.Refresh()
	if !r.CaptureAssistantProse() {
		t.Fatal("expected true after a successful opted-in fetch")
	}
}

func TestResolverOptedOut(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"captureAssistantProse":false}`))
	})
	r := NewResolver("PSE-TEST")
	r.Refresh()
	if r.CaptureAssistantProse() {
		t.Fatal("expected false when the org opted out")
	}
}

func TestResolverFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"teams-not-configured 503", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }},
		{"server error 500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"unparseable body", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) }},
		{"unauthorized 401", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup(t, tc.handler)
			r := NewResolver("PSE-TEST")
			r.Refresh()
			if r.CaptureAssistantProse() {
				t.Fatalf("%s: expected fail-closed false", tc.name)
			}
		})
	}
}

// TestResolverDefaultFalse pins that a resolver that has never fetched (no cache)
// reports false — the fail-closed default before any Refresh.
func TestResolverDefaultFalse(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	r := NewResolver("PSE-TEST")
	if r.CaptureAssistantProse() {
		t.Fatal("expected false before any successful fetch")
	}
}
