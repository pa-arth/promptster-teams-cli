package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Go re-sends X-API-Key on a cross-host redirect, so the client must not follow one.
func TestHTTPClientDoesNotFollowRedirectsWithTheKey(t *testing.T) {
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			leaked = true
		}
	}))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer api.Close()

	req, _ := http.NewRequest(http.MethodPost, api.URL+"/v1/teams/ingest", nil)
	req.Header.Set("X-API-Key", "PSE-SECRET")
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if leaked {
		t.Fatal("engineer key followed a redirect to another host")
	}
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want the 302 surfaced", res.StatusCode)
	}
}
