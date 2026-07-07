package capture

import (
	"bytes"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// PII / business data must be scrubbed regardless of which layer catches it.

// End-to-end: a codex rollout line carrying a secret in the prompt is scrubbed
// before normalization, so the emitted prompt event has no secret.
func TestCodexRolloutRedactsSecret(t *testing.T) {
	line := `{"timestamp":"2026-06-06T20:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"deploy with token=ghp_` + `A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"}}`
	redacted := redact.RedactBytes([]byte(line))
	p := normalize.NewCodexRolloutProcessor("sess-1")
	events := p.Process(redacted)
	if len(events) != 1 || events[0].Kind != "prompt" {
		t.Fatalf("expected 1 prompt event, got %d", len(events))
	}
	data := events[0].Data.(map[string]interface{})
	text, _ := data["text"].(string)
	if bytes.Contains([]byte(text), []byte("ghp_A1b2")) {
		t.Errorf("secret leaked into prompt event: %q", text)
	}
}
