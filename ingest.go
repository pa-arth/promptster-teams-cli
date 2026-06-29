package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
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
// PROMPTSTER_TEAMS_API_URL (via apiURL); the path is the teams ingest route.
// The normalized Event envelope is identical to the one the hiring backend
// accepts, so this stays compatible with a shared ingest contract.
func ingestEndpoint() string {
	if p := os.Getenv("PROMPTSTER_TEAMS_INGEST_PATH"); p != "" {
		return p
	}
	return "/v1/hooks/ingest"
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
		return fmt.Errorf("ingest failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
