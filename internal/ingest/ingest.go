package ingest

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
	"strconv"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
)

// devicePubKeyB64 returns this device's Ed25519 public key (base64), derived
// from the locally-stored signing seed. The backend uses it to verify the
// sig/prevSig chain on ingested events. It is a PUBLIC key — safe to send; the
// private seed never leaves the machine. The backend should pin the first key
// it sees for a device and reject later changes.
func devicePubKeyB64() string {
	priv, err := sign.LoadSessionKeypair()
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

func IngestEventWithAPIKey(ev event.Event, apiKey string) error {
	return IngestEventWithClient(httpClient, ev, apiKey)
}

// IngestEventWithClient POSTs a single normalized, redacted, signed event to
// the configured teams ingest endpoint.
func IngestEventWithClient(client *http.Client, ev event.Event, apiKey string) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return IngestRawEventWithClient(client, body, apiKey)
}

// IngestRawEventWithClient POSTs pre-marshalled event bytes VERBATIM.
//
// The outbox drain uses this deliberately instead of unmarshalling to an
// event.Event and re-marshalling. The backend verifies the ed25519 signature by
// recomputing canonical JSON from the body it receives, and Event.Data is an
// interface{}: a round-trip through encoding/json turns every number into a
// float64, so a value above 2^53 would re-serialize differently
// (1234567890123456789 -> 1234567890123456800) and silently fail signature
// verification. Shipping the exact bytes that were signed removes that class of
// bug entirely rather than relying on round-trip fidelity.
func IngestRawEventWithClient(client *http.Client, body []byte, apiKey string) error {
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
		return &ingestHTTPError{
			status:     resp.StatusCode,
			body:       strings.TrimSpace(string(respBody)),
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	return nil
}

// retryAfterCap bounds a server-supplied Retry-After. The backend's rate limit
// is per-minute, so a value beyond this is a misconfiguration or a hostile
// intermediary; honoring it verbatim would wedge the drain for hours.
const retryAfterCap = 5 * time.Minute

// parseRetryAfter reads an RFC 7231 Retry-After: delta-seconds or an HTTP-date.
// Returns 0 when absent/unparseable/non-positive — callers fall back to their
// own backoff rather than hammering.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return min(time.Duration(secs)*time.Second, retryAfterCap)
	}
	// HTTP-date form. The backend sends seconds, but an intermediary (proxy,
	// CDN, WAF) may rewrite it to a date, and treating that as "no header" would
	// silently drop back to blind backoff during exactly the storm it describes.
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, retryAfterCap)
		}
	}
	return 0
}

// ingestHTTPError is a non-2xx ingest response, kept typed so callers can tell
// a schema/kind rejection apart from a transport or infrastructure failure.
type ingestHTTPError struct {
	status int
	body   string
	// retryAfter is the parsed Retry-After header (0 when absent). The backend
	// returns it on 429 alongside x-ratelimit-*; the drain honors it instead of
	// guessing a backoff.
	retryAfter time.Duration
}

func (e *ingestHTTPError) Error() string {
	return fmt.Sprintf("ingest failed: HTTP %d: %s", e.status, e.body)
}

// IsRateLimited reports whether err is a 429, and returns the server's
// Retry-After delay (0 when the header was absent or unparseable).
func IsRateLimited(err error) (time.Duration, bool) {
	var httpErr *ingestHTTPError
	if !errors.As(err, &httpErr) {
		return 0, false
	}
	if httpErr.status != http.StatusTooManyRequests {
		return 0, false
	}
	return httpErr.retryAfter, true
}

// isIngestRejection reports whether err is the backend REJECTING the event's
// shape or kind (HTTP 400/422) — e.g. a newly-added kind the deployed backend
// doesn't accept yet. Rejections mean the capture channel itself is healthy:
// callers must tolerate them (drop the event, no retries) rather than treating
// the channel as broken. Auth (401/403), rate limiting (429), and 5xx are NOT
// rejections.
func IsIngestRejection(err error) bool {
	var httpErr *ingestHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	return httpErr.status == http.StatusBadRequest || httpErr.status == http.StatusUnprocessableEntity
}
