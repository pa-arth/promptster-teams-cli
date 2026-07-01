package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPresenceEventCarriesNoTranscriptContent is the guardrail for the privacy
// promise: a presence event must carry only benign device/environment metadata
// and never any captured transcript content. If someone widens presenceData
// with a transcript-bearing field, this fails.
func TestPresenceEventCarriesNoTranscriptContent(t *testing.T) {
	sess := Session{SessionID: "dev-0123456789abcdef", SessionToken: "PSE-ABCD-2345"}
	e := buildPresenceEvent(sess)

	if e.Kind != "presence" {
		t.Errorf("kind = %q, want presence", e.Kind)
	}
	if e.Source != presenceSource {
		t.Errorf("source = %q, want %q", e.Source, presenceSource)
	}
	if e.RawPayload != "" {
		t.Errorf("presence event carries rawPayload %q — must be empty", e.RawPayload)
	}
	if e.SessionID != sess.SessionID {
		t.Errorf("sessionID = %q, want %q", e.SessionID, sess.SessionID)
	}

	// The Data payload must contain EXACTLY the closed allow-list of keys.
	raw, err := json.Marshal(e.Data)
	if err != nil {
		t.Fatalf("marshal presence data: %v", err)
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal presence data: %v", err)
	}
	allowed := map[string]bool{
		"device": true, "cliVersion": true, "os": true, "arch": true, "watching": true,
	}
	for k := range data {
		if !allowed[k] {
			t.Errorf("presence data has unexpected field %q — presence must stay metadata-only", k)
		}
	}
	for k := range allowed {
		if _, ok := data[k]; !ok {
			t.Errorf("presence data missing expected field %q", k)
		}
	}

	// Belt-and-braces: no transcript-shaped field names anywhere in the full
	// signed envelope. The team key is auth (a header), never in the body.
	full, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal presence event: %v", err)
	}
	lower := strings.ToLower(string(full))
	// Banned as quoted JSON keys/values so the brand name ("promptster-...")
	// doesn't trip the bare word "prompt".
	for _, banned := range []string{`"prompt"`, `"response"`, `"assistant"`, `"content"`, `"command"`, `"diff"`, `"rawpayload"`, `"text"`, `"token"`} {
		if strings.Contains(lower, banned) {
			t.Errorf("presence envelope contains banned key %q: %s", banned, full)
		}
	}
	if strings.Contains(string(full), sess.SessionToken) {
		t.Errorf("presence envelope leaks the team key: %s", full)
	}

	// Sanity: the benign metadata is actually populated.
	if data["os"] == "" || data["arch"] == "" {
		t.Errorf("presence data missing os/arch: %v", data)
	}
}

// TestPresenceEventIsSignable confirms a presence event round-trips through the
// signing path (canonicalization must not choke on the struct payload).
func TestPresenceEventIsSignable(t *testing.T) {
	e := buildPresenceEvent(Session{SessionID: "dev-abc", SessionToken: "PSE-ABCD-2345"})
	if _, err := buildSigningMessage(e, ""); err != nil {
		t.Fatalf("presence event not signable: %v", err)
	}
}
