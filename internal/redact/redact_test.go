package redact

import (
	"bytes"
	"encoding/json"
	"strings"
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
			out := RedactBytes([]byte(tc.in))
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
	out := RedactBytes([]byte(in))
	if !bytes.Contains(out, []byte("/home/alice/project")) {
		t.Errorf("PWD working-directory value was wrongly redacted: %s", out)
	}
}

// JSON secret-key redaction must keep siblings and leave parseable JSON.
func TestRedactJSONSecretKeyKeepsSiblings(t *testing.T) {
	in := []byte(`{"password":"hunter2","keep":"me"}`)
	out := RedactBytes(in)
	var v map[string]interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("redacted JSON no longer parses: %v\n%s", err, out)
	}
	if v["keep"] != "me" {
		t.Errorf("sibling field damaged: %s", out)
	}
	if !isRedactionMarker(v["password"]) {
		t.Errorf("password not redacted: %s", out)
	}
}

// A value is masked if it was replaced by ANY redaction marker. Tests assert this
// rather than the literal "[REDACTED]" because which marker wins is deliberate,
// ordered behaviour (vendor > Titus rule-id > generic) and the generic stage is
// forbidden from downgrading a more specific marker it finds — see redact.go.
// Pinning the exact string would make an attribution IMPROVEMENT look like a
// regression, which is backwards for a redaction suite: the property that matters
// is "the secret is gone", and the marker only ever gets more informative.
func isRedactionMarker(v interface{}) bool {
	s, ok := v.(string)
	return ok && strings.HasPrefix(s, "[REDACTED") && strings.HasSuffix(s, "]")
}

// A secret value containing an escaped quote must not mis-bound the JSON match:
// the whole value goes, the JSON stays valid, and no tail leaks.
func TestRedactJSONSecretKeyEscapedQuote(t *testing.T) {
	in := []byte(`{"password":"a\"secrettail","keep":"me"}`)
	out := RedactBytes(in)
	var v map[string]interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("escaped-quote value broke JSON: %v\n%s", err, out)
	}
	if !isRedactionMarker(v["password"]) {
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
			out := RedactBytes([]byte(tc.input))
			if !bytes.Contains(out, []byte("[REDACTED]")) {
				t.Errorf("expected [REDACTED] in output, got %q", out)
			}
		})
	}
}

func TestRedactAWSKey(t *testing.T) {
	input := `{"key": "AKIAIOSFODNN7EXAMPLE", "region": "us-east-1"}`
	out := RedactBytes([]byte(input))
	if bytes.Contains(out, []byte("AKIAIOSFODNN7EXAMPLE")) {
		t.Errorf("AWS key not redacted: %q", out)
	}
	if !bytes.Contains(out, []byte("[REDACTED_AWS_KEY]")) {
		t.Errorf("expected [REDACTED_AWS_KEY] marker, got %q", out)
	}
}

func TestRedactEngineerKey(t *testing.T) {
	key := "PSE-VJA3-3W49-6RX8-D2QC-S7CN-CE8N" // gitleaks:allow — fake six-group key
	out := RedactBytes([]byte(`run with X-API-Key: ` + key + ` to ingest`))
	if bytes.Contains(out, []byte(key)) {
		t.Errorf("engineer key not redacted: %q", out)
	}
	if !bytes.Contains(out, []byte("[REDACTED_PROMPTSTER_ENGINEER_KEY]")) {
		t.Errorf("expected [REDACTED_PROMPTSTER_ENGINEER_KEY] marker, got %q", out)
	}
}

func TestRedactGitHubToken(t *testing.T) {
	token := "ghp_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	out := RedactBytes([]byte(`Authorization: token ` + token))
	if bytes.Contains(out, []byte(token)) {
		t.Errorf("GitHub token not redacted: %q", out)
	}
}

func TestRedactPEMBlock(t *testing.T) {
	input := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0Z3VS5JJ\n-----END RSA PRIVATE KEY-----"
	out := RedactBytes([]byte(input))
	if bytes.Contains(out, []byte("MIIEowIBAAKCAQEA")) {
		t.Errorf("PEM key data not redacted: %q", out)
	}
}

func TestRedactSafePassthrough(t *testing.T) {
	input := `{"hook_event_name":"UserPromptSubmit","transcript":"Fix the login bug"}`
	out := RedactBytes([]byte(input))
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
			out := RedactBytes([]byte("value=" + tc.secret + " end"))
			if bytes.Contains(out, []byte(tc.leftover)) {
				t.Errorf("%s secret not redacted: %q", tc.name, out)
			}
		})
	}
	bearer := RedactBytes([]byte("Authorization: Bearer abcdef0123456789ABCDEF0123"))
	if bytes.Contains(bearer, []byte("abcdef0123456789")) {
		t.Errorf("bearer token not redacted: %q", bearer)
	}
}

// Redacting raw JSON bytes (the pattern used by the hook + codex paths) must
// keep the JSON parseable.
func TestRedactKeepsJSONValid(t *testing.T) {
	line := `{"type":"event_msg","payload":{"type":"user_message","message":"my key is sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2 please use it"}}`
	out := RedactBytes([]byte(line))
	var rec map[string]interface{}
	if err := json.Unmarshal(out, &rec); err != nil {
		t.Fatalf("redacted JSON no longer parses: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("sk-proj-A1b2")) {
		t.Errorf("secret survived redaction: %s", out)
	}
}

// PREFIXED secret names. `_` is a word character, so the old leading `\b` could
// never match inside `STRIPE_API_KEY` — these all reached the wire in plaintext
// unless their VALUE happened to carry a provider shape Titus knows. Values here
// are deliberately opaque (no `ghp_`/`sk-`/`AKIA` prefix) so a pass proves the
// ASSIGNMENT rule fired, not Titus.
func TestRedactPrefixedSecretNames(t *testing.T) {
	cases := []struct {
		name  string
		input string
		leak  string
	}{
		{"STRIPE_API_KEY", `STRIPE_API_KEY=zzzopaquevalue123`, "zzzopaquevalue123"},
		{"ACME_DB_PASSWORD", `ACME_DB_PASSWORD=hunter2hunter2`, "hunter2hunter2"},
		{"MY_CLIENT_SECRET", `MY_CLIENT_SECRET=qqqopaquevalue456`, "qqqopaquevalue456"},
		{"AWS_SECRET_ACCESS_KEY", `AWS_SECRET_ACCESS_KEY=wwwopaquevalue789`, "wwwopaquevalue789"},
		{"HF_TOKEN", `HF_TOKEN=eeeopaquevalue012`, "eeeopaquevalue012"},
		{"json STRIPE_API_KEY", `{"STRIPE_API_KEY":"rrropaquevalue345","keep":"me"}`, "rrropaquevalue345"},
		{"dotted prefix", `app.client_secret=tttopaquevalue678`, "tttopaquevalue678"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactBytes([]byte(tc.input))
			if bytes.Contains(out, []byte(tc.leak)) {
				t.Errorf("prefixed secret value survived redaction: %q", out)
			}
			if !bytes.Contains(out, []byte("[REDACTED]")) {
				t.Errorf("expected [REDACTED] marker, got %q", out)
			}
		})
	}
}

// The key NAME must SURVIVE redaction — `STRIPE_API_KEY=[REDACTED]` is what makes
// the downstream paste board actionable; a bare `[REDACTED]` names nothing.
func TestRedactPreservesKeyName(t *testing.T) {
	out := RedactBytes([]byte(`STRIPE_API_KEY=zzzopaquevalue123`))
	if !bytes.Contains(out, []byte("STRIPE_API_KEY=[REDACTED]")) {
		t.Errorf("key name lost — paste board cannot attribute the provider: %q", out)
	}
}

// The prefix widening must NOT grow a trailing wildcard. TOKEN is a prefix of
// TOKENS, so `[A-Za-z0-9_.-]*TOKEN[A-Za-z0-9_.-]*` would redact live spend
// telemetry. These counts are numbers, not secrets, and must pass through.
func TestRedactKeepsTokenCountEnvVars(t *testing.T) {
	for _, in := range []string{
		`MAX_TOKENS=4096`,
		`INPUT_TOKENS=1200`,
		`OUTPUT_TOKENS=350`,
		`AWS_ACCESS_KEY_ID=AKIAPUBLICHALFNOTSECRET`,
	} {
		out := RedactBytes([]byte(in))
		if bytes.Contains(out, []byte("[REDACTED]")) {
			t.Errorf("over-redacted a non-secret assignment %q -> %q", in, out)
		}
	}
}

// The widened JSON rule must still leave parseable JSON and untouched siblings.
func TestRedactPrefixedJSONKeepsSiblings(t *testing.T) {
	in := []byte(`{"ACME_DB_PASSWORD":"hunter2hunter2","keep":"me"}`)
	out := RedactBytes(in)
	var v map[string]interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("redacted JSON no longer parses: %v\n%s", err, out)
	}
	if !isRedactionMarker(v["ACME_DB_PASSWORD"]) {
		t.Errorf("prefixed JSON secret not redacted: %s", out)
	}
	if bytes.Contains(out, []byte("hunter2hunter2")) {
		t.Errorf("prefixed JSON secret value leaked: %s", out)
	}
	if v["keep"] != "me" {
		t.Errorf("sibling field damaged: %s", out)
	}
}

// Vendor split: the marker is the paste board's only provenance once the value is
// gone, so sk-ant- must not collapse into the generic OpenAI marker.
func TestRedactLLMKeyVendorSplit(t *testing.T) {
	ant := RedactBytes([]byte(`ANTHROPIC_API_KEY=sk-ant-api03-A1b2C3d4E5f6G7h8I9j0K1l2M3n4`))
	if !bytes.Contains(ant, []byte("[REDACTED_ANTHROPIC_KEY]")) {
		t.Errorf("anthropic key not attributed to anthropic: %q", ant)
	}
	oai := RedactBytes([]byte(`OPENAI_API_KEY=sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2`))
	if !bytes.Contains(oai, []byte("[REDACTED_OPENAI_KEY]")) {
		t.Errorf("openai key not attributed to openai: %q", oai)
	}
	if bytes.Contains(ant, []byte("sk-ant-api03")) || bytes.Contains(oai, []byte("sk-proj-A1b2")) {
		t.Errorf("vendor split leaked a key value: %q %q", ant, oai)
	}
}

// A value wrapped in square brackets is still a secret. An earlier revision of
// this fix guarded against re-wrapping attributed markers by excluding `[` from
// the value's first byte, which also stopped redacting every real secret that
// happens to start with `[` — narrowing a redaction rule to protect a marker.
// The guard is now a post-match test against the marker itself.
func TestRedactBracketedValues(t *testing.T) {
	cases := []struct {
		name  string
		input string
		leak  string
	}{
		{"assignment", `API_KEY=[customer-secret]`, "customer-secret"},
		{"prefixed assignment", `STRIPE_API_KEY=[zzzopaque123]`, "zzzopaque123"},
		{"json", `{"STRIPE_API_KEY":"[customer-secret]","keep":"me"}`, "customer-secret"},
		{"bracketed but not a marker", `TOKEN=[REDACTEDish-not-a-marker]`, "REDACTEDish"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactBytes([]byte(tc.input))
			if bytes.Contains(out, []byte(tc.leak)) {
				t.Errorf("bracketed secret survived redaction: %q", out)
			}
		})
	}
}

// ...and the reason the guard exists at all: stage 3 must never overwrite an
// attributed marker written by the vendor or Titus stages. Losing the vendor
// name costs the paste board its provenance — "an Anthropic key was pasted"
// degrades to "something was pasted".
func TestRedactDoesNotDowngradeAttributedMarkers(t *testing.T) {
	cases := []struct{ input, keep string }{
		{`ANTHROPIC_API_KEY=sk-ant-aaaaaaaaaaaaaaaaaaaa`, "[REDACTED_ANTHROPIC_KEY]"},
		{`OPENAI_API_KEY=sk-proj-aaaaaaaaaaaaaaaaaaaaaaaa`, "[REDACTED_OPENAI_KEY]"},
		{`AWS_SECRET_ACCESS_KEY=AKIAIOSFODNN7EXAMPLE`, "[REDACTED_AWS_KEY]"},
		{`{"ANTHROPIC_API_KEY":"sk-ant-aaaaaaaaaaaaaaaaaaaa"}`, "[REDACTED_ANTHROPIC_KEY]"},
	}
	for _, tc := range cases {
		out := RedactBytes([]byte(tc.input))
		if !bytes.Contains(out, []byte(tc.keep)) {
			t.Errorf("attributed marker lost for %q -> %q (want %s)", tc.input, out, tc.keep)
		}
	}
}
