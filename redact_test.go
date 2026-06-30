package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// PII / business data must be scrubbed regardless of which layer catches it.
func TestRedactPII(t *testing.T) {
	cases := []struct{ name, in, leak string }{
		{"email", `reach me at jane.doe+ci@example.co.uk please`, "jane.doe+ci@example.co.uk"},
		{"ssn", `SSN 123-45-6789 on file`, "123-45-6789"},
		{"phone dashes", `call 555-123-4567 today`, "555-123-4567"},
		{"phone parens", `call (555) 123-4567 today`, ") 123-4567"},
		{"phone e164", `wa +15551234567 now`, "+15551234567"},
		{"private ip 10", `host 10.1.2.3 internal`, "10.1.2.3"},
		{"private ip 192", `host 192.168.0.42 internal`, "192.168.0.42"},
		{"db url password", `dsn postgres://app:s3cretpw@db.example.com:5432/x end`, "s3cretpw"},
		{"userless dsn password", `dsn redis://:s3cretpw@host:6379/0 end`, "s3cretpw"},
		{"json password", `{"password":"hunter2","keep":"me"}`, "hunter2"},
		{"json apiKey", `{"apiKey":"abc123def456","other":1}`, "abc123def456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redactBytes([]byte(tc.in))
			if bytes.Contains(out, []byte(tc.leak)) {
				t.Errorf("PII survived:\n in: %s\nout: %s", tc.in, out)
			}
		})
	}
}

// PWD is the working-directory env var, not a secret — it must pass through so
// replay/audit keeps its directory context.
func TestRedactKeepsPWDEnvVar(t *testing.T) {
	in := `PWD=/home/alice/project`
	out := redactBytes([]byte(in))
	if !bytes.Contains(out, []byte("/home/alice/project")) {
		t.Errorf("PWD working-directory value was wrongly redacted: %s", out)
	}
}

// JSON secret-key redaction must keep siblings and leave parseable JSON.
func TestRedactJSONSecretKeyKeepsSiblings(t *testing.T) {
	in := []byte(`{"password":"hunter2","keep":"me"}`)
	out := redactBytes(in)
	var v map[string]interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("redacted JSON no longer parses: %v\n%s", err, out)
	}
	if v["keep"] != "me" {
		t.Errorf("sibling field damaged: %s", out)
	}
	if v["password"] != "[REDACTED]" {
		t.Errorf("password not redacted: %s", out)
	}
}

// A secret value containing an escaped quote must not mis-bound the JSON match:
// the whole value goes, the JSON stays valid, and no tail leaks.
func TestRedactJSONSecretKeyEscapedQuote(t *testing.T) {
	in := []byte(`{"password":"a\"secrettail","keep":"me"}`)
	out := redactBytes(in)
	var v map[string]interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("escaped-quote value broke JSON: %v\n%s", err, out)
	}
	if v["password"] != "[REDACTED]" {
		t.Errorf("password not fully redacted: %s", out)
	}
	if bytes.Contains(out, []byte("secrettail")) {
		t.Errorf("secret tail leaked past the escaped quote: %s", out)
	}
	if v["keep"] != "me" {
		t.Errorf("sibling field damaged: %s", out)
	}
}

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
