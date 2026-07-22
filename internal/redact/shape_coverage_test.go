package redact

import (
	"strings"
	"testing"
)

// Which real-world secret SHAPES survive redaction, pinned as a table.
//
// This exists because reading the regex list is how the gap got missed. Titus
// carries ~490 provider rules and the supplemental list adds more, so "is
// Hugging Face covered?" is not answerable by inspection — only by running a
// value through the real RedactBytes. Every entry below was verified against the
// pipeline, not against the patterns.
//
// TWO FORMS PER SHAPE, and the split is the whole point:
//
//	bare prose  "here is the key <value> thanks"   — only a VALUE-shape rule fires
//	assignment  "MY_TOKEN=<value>"                 — the generic KEY=value rule fires
//
// The assignment form has never leaked for any shape, which is why the original
// bug looked invisible: the same secret is caught when written `X=...` and
// missed when pasted mid-sentence. Prose is the form a human actually pastes.
//
// Values are assembled by concatenation so no single literal in this file looks
// like a credential to a secret scanner. None of them was ever issued.
func rep(s string, n int) string { return strings.Repeat(s, (n/len(s))+1)[:n] }

const (
	b62 = "A1b2C3d4E5f6G7h8I9j0K1L2m3N4o5P6q7R8s9T0u1V2w3X4y5Z6"
	hx  = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
)

type shapeCase struct {
	vendor string
	val    string
	// bareLeakOK marks a shape we CANNOT catch by value shape and have decided
	// not to chase. See TestShapeCoverage_UncatchableAreDeliberate.
	bareLeakOK bool
}

func shapeCases() []shapeCase {
	return []shapeCase{
		{vendor: "OpenAI sk-proj", val: "sk-" + "proj-" + rep(b62, 48)},
		{vendor: "OpenAI legacy", val: "sk-" + rep(b62, 48)},
		{vendor: "OpenRouter", val: "sk-" + "or-v1-" + rep(hx, 64)},
		{vendor: "Anthropic", val: "sk-" + "ant-api03-" + rep(b62, 95)},
		{vendor: "Google API key", val: "AIza" + rep(b62, 35)},
		{vendor: "Google OAuth", val: "ya29." + rep(b62, 60)},
		{vendor: "AWS access key id", val: "AKIA" + "IOSFODNN7EXAMPLE"},
		{vendor: "GitHub classic PAT", val: "ghp_" + rep(b62, 36)},
		{vendor: "GitHub fine-grained", val: "github_pat_" + rep(b62, 82)},
		{vendor: "GitHub OAuth", val: "gho_" + rep(b62, 36)},
		{vendor: "GitLab PAT", val: "glpat-" + rep(b62, 20)},
		{vendor: "Slack bot", val: "xoxb-" + rep("1234567890-", 24)},
		{vendor: "Stripe secret", val: "sk_" + "live_" + rep(b62, 24)},
		{vendor: "Stripe restricted", val: "rk_" + "live_" + rep(b62, 24)},
		{vendor: "Supabase PAT", val: "sbp_" + rep(hx, 40)},
		{vendor: "SendGrid", val: "SG." + rep(b62, 22) + "." + rep(b62, 43)},
		{vendor: "DigitalOcean", val: "dop_v1_" + rep(hx, 64)},
		{vendor: "npm token", val: "npm_" + rep(b62, 36)},
		{vendor: "Linear API", val: "lin_api_" + rep(b62, 40)},
		{vendor: "Groq", val: "gsk_" + rep(b62, 52)},
		{vendor: "Replicate", val: "r8_" + rep(b62, 37)},
		{vendor: "Perplexity", val: "pplx-" + rep(b62, 48)},
		{vendor: "xAI", val: "xai-" + rep(b62, 80)},
		{vendor: "JWT", val: "eyJhbGciOiJIUzI1NiJ9." + rep(b62, 40) + "." + rep(b62, 43)},

		// The four added alongside this test — each leaked in prose before.
		{vendor: "Svix/Supabase webhook", val: "whsec_" + rep(b62, 32)},
		{vendor: "Hugging Face", val: "hf_" + rep(b62, 34)},
		{vendor: "Sentry", val: "sntrys_" + rep(b62, 60)},
		{vendor: "Notion integration", val: "secret_" + rep(b62, 43)},

		// Deliberately uncatchable by shape — see the dedicated test below.
		{vendor: "AWS SECRET access key", val: "wJalrXUtnFEMI" + rep(b62, 27), bareLeakOK: true},
		{vendor: "Datadog", val: rep(hx, 32), bareLeakOK: true},
		// Twilio's API Key SECRET, the half that actually authenticates: 32 bare
		// alphanumerics, no prefix. Its SID (`SK` + 32 hex) IS matchable and is
		// deliberately not matched — see the identifier-vs-credential note in
		// redact.go. Listing the secret here rather than the SID keeps the table
		// honest about which half we can and cannot see.
		{vendor: "Twilio API key SECRET", val: rep(b62, 32), bareLeakOK: true},
	}
}

// A pasted secret must not survive in the form a human actually writes it.
func TestShapeCoverage_BareProse(t *testing.T) {
	for _, c := range shapeCases() {
		if c.bareLeakOK {
			continue
		}
		out := string(RedactBytes([]byte("here is the key " + c.val + " thanks")))
		if strings.Contains(out, c.val) {
			t.Errorf("%s: value survived bare-prose redaction: %s", c.vendor, out)
		}
		if !strings.Contains(out, "[REDACTED") {
			t.Errorf("%s: no marker emitted, so the paste board sees nothing: %s", c.vendor, out)
		}
	}
}

// The KEY=value rule is the backstop that made the prose gap invisible for so
// long. It must stay total: every shape, including the two we cannot match by
// value, is caught when written as an assignment.
func TestShapeCoverage_Assignment(t *testing.T) {
	for _, c := range shapeCases() {
		out := string(RedactBytes([]byte("MY_TOKEN=" + c.val)))
		if strings.Contains(out, c.val) {
			t.Errorf("%s: value survived KEY=value redaction: %s", c.vendor, out)
		}
	}
}

// Pins the two shapes we chose NOT to chase, so the choice stays a decision
// rather than drift — and so anyone tempted to "fix" them reads why first.
//
// An AWS SECRET access key is 40 bare base64 chars; a Datadog key is 32 bare
// hex. Neither carries a prefix, so matching them means matching every hash,
// UUID and git SHA of that length. redact.go's header records that a blunt
// entropy pass was tried and REVERTED: it collapsed msg_/toolu_/call_ provider
// IDs to a constant and broke turn dedup. Working replay is worth more than two
// shapes that only leak in prose and are already caught when assigned.
//
// If this test ever fails because these are now caught, that is fine — delete
// the entries. Just confirm dedup still works first.
func TestShapeCoverage_UncatchableAreDeliberate(t *testing.T) {
	for _, c := range shapeCases() {
		if !c.bareLeakOK {
			continue
		}
		out := string(RedactBytes([]byte("here is the key " + c.val + " thanks")))
		if !strings.Contains(out, c.val) {
			t.Logf("%s is now caught in bare prose — drop bareLeakOK if dedup still passes", c.vendor)
		}
	}
}

// A Twilio API Key SID (`SK` + 32 hex) is an IDENTIFIER, not a credential. It
// grants nothing alone, appears in logs, URLs and API responses, and the half
// that authenticates is a separately issued 32-char alphanumeric with no prefix.
//
// It shipped as a rule in the first draft of this PR precisely because the shape
// is so clean, and that is the trap: `[REDACTED_SECRET_KEY]` grades `critical`
// downstream, so the rule would have fired "rotate this now" on a value with
// nothing to rotate — while the real secret walked past untouched. False
// criticals are what turn a rotation list back into noise, which is the same
// argument that keeps name-only evidence at `high`, pointed the other way.
//
// Pinned as a decision, not a comment. If this ever fails, the new rule is
// matching the wrong half of an id/secret pair.
func TestShapeCoverage_TwilioSidIsNotACredential(t *testing.T) {
	sid := "SK" + rep(hx, 32)
	out := string(RedactBytes([]byte("the api key sid is " + sid + " per the docs")))
	if strings.Contains(out, "[REDACTED_SECRET_KEY]") {
		t.Errorf("Twilio API Key SID graded as a verified key shape — that is a false critical: %s", out)
	}
}

// The four new rules collapse to ONE marker on purpose, and that marker must be
// distinguishable from the name-only generic. `[REDACTED]` means "a secret-ish
// NAME had some value"; `[REDACTED_SECRET_KEY]` means "a value only a real key
// produces". Downstream grades the second critical and the first high, so
// merging them would silently downgrade every shape-matched paste.
func TestShapeCoverage_SecretKeyMarkerIsDistinct(t *testing.T) {
	shaped := string(RedactBytes([]byte("here is the key " + "whsec_" + rep(b62, 32) + " thanks")))
	if !strings.Contains(shaped, "[REDACTED_SECRET_KEY]") {
		t.Fatalf("shape-matched paste lost its dedicated marker: %s", shaped)
	}
	named := string(RedactBytes([]byte("MY_TOKEN=" + "totally-not-a-known-shape-9999")))
	if strings.Contains(named, "[REDACTED_SECRET_KEY]") {
		t.Fatalf("name-only evidence must NOT claim a verified key shape: %s", named)
	}
	if !strings.Contains(named, "[REDACTED]") {
		t.Fatalf("name-only assignment lost its generic marker: %s", named)
	}
}
