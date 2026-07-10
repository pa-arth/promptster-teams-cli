package ingest

import (
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

const DefaultAPIURL = "https://api.promptster.ai"

// apiURL returns the ingest base URL. The teams CLI sets PROMPTSTER_API_URL
// from PROMPTSTER_TEAMS_API_URL at watch start (see loadSession), so every
// outbound request targets the team's configured backend.
func apiURL() string {
	if u := os.Getenv("PROMPTSTER_API_URL"); u != "" {
		return u
	}
	return DefaultAPIURL
}

// APIURL is the exported form of apiURL so sibling packages (e.g. policy)
// resolve the SAME teams base URL the ingest path uses, instead of duplicating
// the PROMPTSTER_API_URL resolution.
func APIURL() string {
	return apiURL()
}

// HTTPClient returns the shared outbound client (15s timeout, injects
// X-Promptster-CLI-Version via versionTransport). Exported so sibling packages
// reuse the same transport — every request the CLI makes carries the version
// header — rather than rebuilding it.
func HTTPClient() *http.Client {
	return httpClient
}

// apiHost returns the host portion of the configured API URL for display in
// diagnostics, falling back to the raw URL if it doesn't parse.
func APIHost() string {
	if u, err := url.Parse(apiURL()); err == nil && u.Host != "" {
		return u.Host
	}
	return apiURL()
}

// versionTransport injects X-Promptster-CLI-Version on every outbound request.
type versionTransport struct {
	base http.RoundTripper
}

func (t *versionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("X-Promptster-CLI-Version", version.Version)
	return t.base.RoundTrip(req)
}

var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &versionTransport{base: http.DefaultTransport},
}
