package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRedactEnvVarSecret(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"API_KEY", `API_KEY=supersecret123`},
		{"TOKEN lower", `token=abc123xyz`},
		{"SECRET equals", `SECRET=my-secret-value`},
		{"PASSWORD", `PASSWORD=hunter2`},
		{"PRIVATE_KEY", `PRIVATE_KEY=priv123`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redactBytes([]byte(tc.input))
			if !bytes.Contains(out, []byte("[REDACTED]")) {
				t.Errorf("expected [REDACTED] in output, got %q", out)
			}
		})
	}
}

func TestRedactAWSKey(t *testing.T) {
	input := `{"key": "AKIAIOSFODNN7EXAMPLE", "region": "us-east-1"}`
	out := redactBytes([]byte(input))
	if bytes.Contains(out, []byte("AKIAIOSFODNN7EXAMPLE")) {
		t.Errorf("AWS key not redacted: %q", out)
	}
	if !bytes.Contains(out, []byte("[REDACTED_AWS_KEY]")) {
		t.Errorf("expected [REDACTED_AWS_KEY] marker, got %q", out)
	}
}

func TestRedactEngineerKey(t *testing.T) {
	key := "PSE-AB2C-9XYZ"
	out := redactBytes([]byte(`run with X-API-Key: ` + key + ` to ingest`))
	if bytes.Contains(out, []byte(key)) {
		t.Errorf("engineer key not redacted: %q", out)
	}
	if !bytes.Contains(out, []byte("[REDACTED_PROMPTSTER_ENGINEER_KEY]")) {
		t.Errorf("expected [REDACTED_PROMPTSTER_ENGINEER_KEY] marker, got %q", out)
	}
}

func TestRedactGitHubToken(t *testing.T) {
	token := "ghp_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	out := redactBytes([]byte(`Authorization: token ` + token))
	if bytes.Contains(out, []byte(token)) {
		t.Errorf("GitHub token not redacted: %q", out)
	}
}

func TestRedactPEMBlock(t *testing.T) {
	input := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0Z3VS5JJ\n-----END RSA PRIVATE KEY-----"
	out := redactBytes([]byte(input))
	if bytes.Contains(out, []byte("MIIEowIBAAKCAQEA")) {
		t.Errorf("PEM key data not redacted: %q", out)
	}
}

func TestRedactSafePassthrough(t *testing.T) {
	input := `{"hook_event_name":"UserPromptSubmit","transcript":"Fix the login bug"}`
	out := redactBytes([]byte(input))
	if string(out) != input {
		t.Errorf("safe payload was modified: %q", out)
	}
}

func TestRedactLLMAndBearerAndJWT(t *testing.T) {
	cases := []struct {
		name, secret, leftover string
	}{
		{"openai", "sk-proj-" + "A1b2C3d4E5f6G7h8I9j0K1l2", "sk-proj-A1b2C3d4"},
		{"anthropic", "sk-ant-api03-" + "Zz9Yy8Xx7Ww6Vv5Uu4Tt3", "sk-ant-api03"},
		{"slack", "xoxb-" + "1234567890-ABCDEFghijkl", "xoxb-12345"},
		{"jwt", "eyJhbGciOiJI.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT", "eyJhbGciOiJI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redactBytes([]byte("value=" + tc.secret + " end"))
			if bytes.Contains(out, []byte(tc.leftover)) {
				t.Errorf("%s secret not redacted: %q", tc.name, out)
			}
		})
	}
	bearer := redactBytes([]byte("Authorization: Bearer abcdef0123456789ABCDEF0123"))
	if bytes.Contains(bearer, []byte("abcdef0123456789")) {
		t.Errorf("bearer token not redacted: %q", bearer)
	}
}

// Redacting raw JSON bytes (the pattern used by the hook + codex paths) must
// keep the JSON parseable.
func TestRedactKeepsJSONValid(t *testing.T) {
	line := `{"type":"event_msg","payload":{"type":"user_message","message":"my key is sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2 please use it"}}`
	out := redactBytes([]byte(line))
	var rec map[string]interface{}
	if err := json.Unmarshal(out, &rec); err != nil {
		t.Fatalf("redacted JSON no longer parses: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("sk-proj-A1b2")) {
		t.Errorf("secret survived redaction: %s", out)
	}
}

// End-to-end: a Cursor afterShellExecution line carrying a secret in the command
// output is scrubbed before normalization, so the emitted command event is clean.
func TestCursorAfterShellRedactsSecret(t *testing.T) {
	line := `{"hook_event_name":"afterShellExecution","cursor_version":"2026.02.27","conversation_id":"c1","command":"printenv","output":"TOKEN=ghp_` + `A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8\n"}`
	redacted := redactBytes([]byte(line))
	var payload map[string]interface{}
	if err := json.Unmarshal(redacted, &payload); err != nil {
		t.Fatalf("redacted JSON no longer parses: %v\n%s", err, redacted)
	}
	e, ok := normalizeCursor(payload, "sess-1")
	if !ok || e.Kind != "command" {
		t.Fatalf("expected command event, kind=%q ok=%v", e.Kind, ok)
	}
	data, _ := e.Data.(map[string]interface{})
	out, _ := data["stdout"].(string)
	if bytes.Contains([]byte(out), []byte("ghp_A1b2")) {
		t.Errorf("secret leaked into command output: %q", out)
	}
}

// End-to-end: a codex rollout line carrying a secret in the prompt is scrubbed
// before normalization, so the emitted prompt event has no secret.
func TestCodexRolloutRedactsSecret(t *testing.T) {
	line := `{"timestamp":"2026-06-06T20:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"deploy with token=ghp_` + `A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"}}`
	redacted := redactBytes([]byte(line))
	p := newCodexRolloutProcessor("sess-1")
	events := p.process(redacted)
	if len(events) != 1 || events[0].Kind != "prompt" {
		t.Fatalf("expected 1 prompt event, got %d", len(events))
	}
	data := events[0].Data.(map[string]interface{})
	text, _ := data["text"].(string)
	if bytes.Contains([]byte(text), []byte("ghp_A1b2")) {
		t.Errorf("secret leaked into prompt event: %q", text)
	}
}
