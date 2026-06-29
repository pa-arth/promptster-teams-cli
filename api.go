package main

import (
	"net/http"
	"net/url"
	"os"
	"time"
)

const defaultAPIURL = "https://api.promptster.ai"

// apiURL returns the ingest base URL. The teams CLI sets PROMPTSTER_API_URL
// from PROMPTSTER_TEAMS_API_URL at watch start (see loadSession), so every
// outbound request targets the team's configured backend.
func apiURL() string {
	if u := os.Getenv("PROMPTSTER_API_URL"); u != "" {
		return u
	}
	return defaultAPIURL
}

// usingDefaultAPI reports whether the CLI is pointed at the hosted Promptster
// API (vs a self-hosted backend). Used to soften hosted-only hints in doctor.
func usingDefaultAPI() bool {
	return apiURL() == defaultAPIURL
}

// apiHost returns the host portion of the configured API URL for display in
// diagnostics, falling back to the raw URL if it doesn't parse.
func apiHost() string {
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
	req.Header.Set("X-Promptster-CLI-Version", version)
	return t.base.RoundTrip(req)
}

var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &versionTransport{base: http.DefaultTransport},
}
