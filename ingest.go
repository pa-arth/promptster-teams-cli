package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// devicePubKeyB64 returns this device's Ed25519 public key (base64), derived
// from the locally-stored signing seed. The backend uses it to verify the
// sig/prevSig chain on ingested events. It is a PUBLIC key — safe to send; the
// private seed never leaves the machine. The backend should pin the first key
// it sees for a device and reject later changes.
func devicePubKeyB64() string {
	priv, err := loadSessionKeypair()
	if err != nil || priv == nil {
		return ""
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return base64.StdEncoding.EncodeToString(pub)
}

// ingestEndpoint is the path events are POSTed to. The base URL comes from
// PROMPTSTER_TEAMS_API_URL (via apiURL); the path is the dedicated teams
// ingest route, which authenticates an org API key and persists to the teams
// database (distinct from the hiring `/v1/hooks/ingest` route, which uses
// candidate-key auth). Override with PROMPTSTER_TEAMS_INGEST_PATH if your
// backend mounts it elsewhere.
func ingestEndpoint() string {
	if p := os.Getenv("PROMPTSTER_TEAMS_INGEST_PATH"); p != "" {
		return p
	}
	return "/v1/teams/ingest"
}

func ingestEventWithAPIKey(event Event, apiKey string) error {
	return ingestEventWithClient(httpClient, event, apiKey)
}

// ingestEventWithClient POSTs a single normalized, redacted, signed event to
// the configured teams ingest endpoint.
func ingestEventWithClient(client *http.Client, event Event, apiKey string) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, apiURL()+ingestEndpoint(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	// Ship the device's public verifying key so the backend can check the
	// event's signature chain. Public key only — the seed stays on-device.
	if pub := devicePubKeyB64(); pub != "" {
		req.Header.Set("X-Promptster-Device-Pubkey", pub)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ingest request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return &ingestHTTPError{status: resp.StatusCode, body: strings.TrimSpace(string(respBody))}
	}
	return nil
}

// ingestHTTPError is a non-2xx ingest response, kept typed so callers can tell
// a schema/kind rejection apart from a transport or infrastructure failure.
type ingestHTTPError struct {
	status int
	body   string
}

func (e *ingestHTTPError) Error() string {
	return fmt.Sprintf("ingest failed: HTTP %d: %s", e.status, e.body)
}

// isIngestRejection reports whether err is the backend REJECTING the event's
// shape or kind (HTTP 400/422) — e.g. a newly-added kind the deployed backend
// doesn't accept yet. Rejections mean the capture channel itself is healthy:
// callers must tolerate them (drop the event, no retries) rather than treating
// the channel as broken. Auth (401/403), rate limiting (429), and 5xx are NOT
// rejections.
func isIngestRejection(err error) bool {
	var httpErr *ingestHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	return httpErr.status == http.StatusBadRequest || httpErr.status == http.StatusUnprocessableEntity
}
