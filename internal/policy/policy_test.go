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

// TestAutoUpdateDefaultsOpen pins the FAIL-OPEN posture: a resolver that never
// fetched leaves auto-update ON and unpinned, so a network blip can't strand the
// fleet on an old binary.
func TestAutoUpdateDefaultsOpen(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	r := NewResolver("PSE-TEST")
	if !r.AutoUpdateEnabled() {
		t.Fatal("auto-update must default ON before any fetch")
	}
	if r.PinnedCliVersion() != "" {
		t.Fatal("pin must default empty")
	}
}

// TestAutoUpdateFailsOpenOnError proves a failing refresh does NOT flip
// auto-update off — the opposite of the prose fail-closed rule.
func TestAutoUpdateFailsOpenOnError(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	r := NewResolver("PSE-TEST")
	r.Refresh()
	if !r.AutoUpdateEnabled() {
		t.Fatal("auto-update must stay ON when the policy fetch fails")
	}
}

// TestAutoUpdateExplicitDisableAndPin proves an explicit backend response flips
// the switch off and carries the pin.
func TestAutoUpdateExplicitDisableAndPin(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"autoUpdate":false,"pinnedCliVersion":"0.6.0"}`))
	})
	r := NewResolver("PSE-TEST")
	r.Refresh()
	if r.AutoUpdateEnabled() {
		t.Fatal("explicit autoUpdate:false must disable")
	}
	if r.PinnedCliVersion() != "0.6.0" {
		t.Fatalf("pin = %q, want 0.6.0", r.PinnedCliVersion())
	}
}

// TestAutoUpdateAbsentFieldStaysOpen proves that a policy body WITHOUT the
// autoUpdate key (unknown, not false) leaves auto-update ON.
func TestAutoUpdateAbsentFieldStaysOpen(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"captureAssistantProse":true}`))
	})
	r := NewResolver("PSE-TEST")
	r.Refresh()
	if !r.AutoUpdateEnabled() {
		t.Fatal("absent autoUpdate field must be treated as ON, not false")
	}
}

// TestResolverAdoptsDiskCache proves a fresh process (a second Resolver over the
// same state dir) picks up a within-TTL cached policy WITHOUT a network fetch:
// resolver #1 does a successful opted-in Refresh (writing the disk cache), then
// resolver #2 reports true straight from NewResolver, never calling Refresh.
func TestResolverAdoptsDiskCache(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"captureAssistantProse":true}`))
	})

	// Resolver #1 fetches and persists the cache.
	r1 := NewResolver("PSE-TEST")
	r1.Refresh()
	if !r1.CaptureAssistantProse() {
		t.Fatal("resolver #1: expected true after a successful fetch")
	}

	// Resolver #2 shares the state dir (set by setup) and must adopt the cache
	// on construction — no Refresh call.
	r2 := NewResolver("PSE-TEST")
	if !r2.CaptureAssistantProse() {
		t.Fatal("resolver #2: expected true adopted from the disk cache without Refresh")
	}
}
