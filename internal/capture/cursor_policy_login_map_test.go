package capture

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/policy"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// fetchedCursorPolicy builds a resolver that has run one real Refresh against a
// test policy endpoint answering status/body.
func fetchedCursorPolicy(t *testing.T, status int, body string) *policy.Resolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PROMPTSTER_API_URL", srv.URL)
	r := policy.NewResolver("PSE-TEST")
	r.Refresh()
	return r
}

// An unknown or stale policy (never fetched, or the fetch failed) stops
// collection but is NOT a revocation: the login map survives byte-for-byte.
// "No collection" is proven by the IDE store holding a login the map does not
// have. A store read would upsert it.
func TestCursorVendorPollUnknownPolicyKeepsLoginsAndDoesNotCollect(t *testing.T) {
	for name, resolver := range map[string]func(t *testing.T) *policy.Resolver{
		"never fetched": func(*testing.T) *policy.Resolver { return &policy.Resolver{} },
		"fetch failed": func(t *testing.T) *policy.Resolver {
			return fetchedCursorPolicy(t, http.StatusServiceUnavailable, `{}`)
		},
		"network error": func(t *testing.T) *policy.Resolver {
			t.Setenv("PROMPTSTER_API_URL", "http://127.0.0.1:1")
			r := policy.NewResolver("PSE-TEST")
			r.Refresh()
			return r
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
			key := state.CursorAttributionKey()
			seeLogin(key, "second.login@example.com", "2e173b81208bddd5", time.Now().Add(-time.Hour))
			before, err := os.ReadFile(cursorAccountReadingPath())
			if err != nil {
				t.Fatalf("precondition: login map not written: %v", err)
			}
			token := jwtWithClaims(t, `{"sub":"user_signed_in_now","exp":4102444800}`)
			t.Setenv(cursorStateDBEnv, makeCursorStateDB(t, map[string]string{
				cursorAuthAccessTokenKey: token, cursorAuthRefreshTokenKey: token, cursorAuthCachedEmailKey: "current@example.com",
			}))

			r := resolver(t)
			if permitted, known := r.CursorVendorUsageDecision(); permitted || known {
				t.Fatalf("precondition: permitted=%v known=%v, want unknown", permitted, known)
			}
			pollCursorVendorUsage("dev", r, nil, time.Now())

			after, err := os.ReadFile(cursorAccountReadingPath())
			if err != nil {
				t.Fatalf("unknown policy deleted the login map: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("unknown policy changed the login map (a store was read?):\nbefore %s\nafter  %s", before, after)
			}
			if ref, ok := cursorHookAccountRef(stopPayload(t, "second.login@example.com")); !ok || ref != "2e173b81208bddd5" {
				t.Fatalf("second login forgotten: ref=%q ok=%v", ref, ok)
			}
		})
	}
}
