package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Golden corpus: realistic-format secrets that MUST be redacted by the
// combined Titus + supplemental pipeline, regardless of which layer catches
// them. This is the contract any engine swap gets verified against.
//
// Every credential below is a CRAFTED FIXTURE, not a live secret — they're
// taken from the scanner rules' own documented examples or hand-assembled to
// match the format. They must look real: entropy checks and length-exact
// rules reject obviously fake values, which is the point of the corpus.
func TestRedactGoldenSecrets(t *testing.T) {
	cases := []struct {
		name   string
		secret string
	}{
		{"anthropic full", "sk-ant-" + "api03-jSq6OMjv1syXaEUE0bvOckLe_GtCKy8lvZdko3eOJgV8TH-f2iyzRekyZNSby5d9ScikGYuqQhsrxML3X3N3rQ-XwQaQAAA"},
		{"github pat", "ghp_" + "wWPw5k4aXcaT4fNP0UcnZwJUVFk6LO0pINUx"},
		{"aws access key", "AKIAZ9QXR4PW2EXAMPLE"},
		{"slack bot token", "xoxb-" + "2966595867-2940150271555-tCp1q0XlcCRrUbhM2zYbqDvE"},
		{"jwt", "eyJ" + "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJVadQssw5c"},
		{"promptster candidate key", "PST-A7K2-M9Q4"},
		{"promptster org key", "psk_live_3K9dQ7vR2mX8pL4nW6tY1zB5cF0gH_Jk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, wrap := range []struct {
				name, payload string
			}{
				{"plain", "the credential is " + tc.secret + " right there"},
				{"json string", `{"output":"found ` + tc.secret + ` in env"}`},
				{"json quote-adjacent", `{"key":"` + tc.secret + `"}`},
			} {
				out := redactBytes([]byte(wrap.payload))
				if bytes.Contains(out, []byte(tc.secret)) {
					t.Errorf("%s/%s: secret survived: %s", tc.name, wrap.name, out)
				}
				if strings.HasPrefix(wrap.payload, "{") {
					var v map[string]interface{}
					if err := json.Unmarshal(out, &v); err != nil {
						t.Errorf("%s/%s: redacted JSON no longer parses: %v\n%s", tc.name, wrap.name, err, out)
					}
				}
			}
		})
	}
}

// Clean text must pass through byte-identical — no marker noise, no
// reformatting. Guards the false-positive budget.
func TestRedactCleanPassthroughByteIdentical(t *testing.T) {
	clean := []string{
		`{"hook_event_name":"UserPromptSubmit","prompt":"Fix the race condition in the queue worker"}`,
		`{"kind":"command","data":{"command":"go test ./...","exitCode":0}}`,
		"diff --git a/main.go b/main.go\n+func handler(w http.ResponseWriter, r *http.Request) {\n",
		`{"text":"the function returns a pointer-to-struct, see docs/api.md"}`,
		// Prose that superficially resembles key material.
		`{"prompt":"use the secret sauce approach and update the token bucket rate limiter"}`,
	}
	for _, in := range clean {
		out := redactBytes([]byte(in))
		if string(out) != in {
			t.Errorf("clean payload was modified:\n in: %s\nout: %s", in, out)
		}
	}
}

// The quote-adjacency case that motivated token-narrowing: Titus rule patterns
// consume one trailing boundary char, which inside JSON is the closing quote.
// The splice must narrow to the named token group and leave the quote alone.
func TestTitusRedactPreservesJSONQuotes(t *testing.T) {
	if titusScanner() == nil {
		t.Skip("titus engine unavailable")
	}
	in := []byte(`{"apiKey":"sk-ant-` + `api03-jSq6OMjv1syXaEUE0bvOckLe_GtCKy8lvZdko3eOJgV8TH-f2iyzRekyZNSby5d9ScikGYuqQhsrxML3X3N3rQ-XwQaQAAA","next":"value"}`)
	out := titusRedact(in)
	var v map[string]interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("titusRedact corrupted JSON: %v\n%s", err, out)
	}
	if v["next"] != "value" {
		t.Errorf("adjacent field damaged: %s", out)
	}
	if !bytes.Contains(out, []byte("[REDACTED:")) {
		t.Errorf("expected titus marker in output: %s", out)
	}
}

// scrubEvent is the choke-point pass covering sources that don't pre-redact
// their raw input (shell hook commands, decision/explain rationales).
func TestScrubEventShellCommand(t *testing.T) {
	event := Event{
		ID:        "evt-1",
		SessionID: "sess-1",
		Kind:      "command",
		Source:    "terminal",
		V:         1,
		Data: map[string]interface{}{
			"command": "export ANTHROPIC_API_KEY=sk-ant-" + "api03-jSq6OMjv1syXaEUE0bvOckLe_GtCKy8lvZdko3eOJgV8TH-f2iyzRekyZNSby5d9ScikGYuqQhsrxML3X3N3rQ-XwQaQAAA",
			"human":   true,
		},
	}
	scrubEvent(&event)
	data, ok := event.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("event.Data lost its shape: %T", event.Data)
	}
	cmd, _ := data["command"].(string)
	if strings.Contains(cmd, "jSq6OMjv1syX") {
		t.Errorf("secret survived scrubEvent: %q", cmd)
	}
	if !strings.Contains(cmd, "export ANTHROPIC_API_KEY=") {
		t.Errorf("non-secret command text damaged: %q", cmd)
	}
	if data["human"] != true {
		t.Errorf("sibling field damaged: %v", data["human"])
	}
}

func TestScrubEventCleanUntouched(t *testing.T) {
	event := Event{
		ID:        "evt-2",
		SessionID: "sess-1",
		Kind:      "decision_event",
		Source:    "cli",
		V:         1,
		Data: map[string]interface{}{
			"description": "Chose streaming parse over buffering the whole response",
		},
	}
	before, _ := json.Marshal(event)
	scrubEvent(&event)
	after, _ := json.Marshal(event)
	if !bytes.Equal(before, after) {
		t.Errorf("clean event was modified:\n before: %s\n after: %s", before, after)
	}
}

// Multiple distinct secrets in one payload — all must go, ordering intact.
func TestRedactMultipleSecretsOnePayload(t *testing.T) {
	in := []byte(`{"output":"ANTHROPIC=sk-ant-` + `api03-jSq6OMjv1syXaEUE0bvOckLe_GtCKy8lvZdko3eOJgV8TH-f2iyzRekyZNSby5d9ScikGYuqQhsrxML3X3N3rQ-XwQaQAAA GITHUB=ghp_` + `wWPw5k4aXcaT4fNP0UcnZwJUVFk6LO0pINUx KEY=PST-A7K2-M9Q4 done"}`)
	out := redactBytes(in)
	for _, leak := range []string{"jSq6OMjv1syX", "wWPw5k4aXcaT", "PST-A7K2"} {
		if bytes.Contains(out, []byte(leak)) {
			t.Errorf("secret %q survived: %s", leak, out)
		}
	}
	var v map[string]interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("multi-secret redaction corrupted JSON: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("done")) {
		t.Errorf("trailing content lost: %s", out)
	}
}

// Large-payload latency guard: a 100KB tool-result-sized payload through the
// full pipeline. Run with: go test -bench BenchmarkRedactLargePayload
func BenchmarkRedactLargePayload(b *testing.B) {
	chunk := `{"type":"tool_result","content":"` + strings.Repeat("func process(items []Item) error { for _, it := range items { if err := validate(it); err != nil { return err } } return nil } ", 700) + `sk-ant-` + `api03-jSq6OMjv1syXaEUE0bvOckLe_GtCKy8lvZdko3eOJgV8TH-f2iyzRekyZNSby5d9ScikGYuqQhsrxML3X3N3rQ-XwQaQAAA"}`
	payload := []byte(chunk)
	b.SetBytes(int64(len(payload)))
	titusScanner() // exclude one-time init from the loop
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		redactBytes(payload)
	}
}
