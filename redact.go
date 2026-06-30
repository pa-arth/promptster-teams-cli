package main

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"sync"

	"github.com/praetorian-inc/titus/pkg/scanner"
)

// Secret redaction runs in layers, all behind redactBytes:
//
//  1. Titus (praetorian-inc/titus, Apache-2.0) — ~490 entropy-aware provider
//     rules (AWS, GitHub, Anthropic, OpenAI, Slack, PEM, JWT, ...). Engine
//     init is once per process; if it ever fails the supplemental layers
//     below still run, so capture is never blocked and never unscrubbed.
//  2. Supplemental regex patterns — Promptster's own credentials, generic
//     credential shapes (KEY=value / JSON key:value, bearer headers, URL-
//     embedded passwords) and PII / business data (email, SSN, phone, private
//     IPs) that a provider-rule secret scanner doesn't cover.
//  3. Heuristic passes — Luhn-validated credit-card numbers, and a
//     high-entropy catch-all for org-internal tokens with no named rule. The
//     catch-all is deliberately greedy (org secrets matter more than the odd
//     over-redacted blob); UUIDs, git SHAs and ordinary code identifiers stay
//     below its entropy floor and pass through.
//
// Every layer operates on raw JSON bytes (the hook + codex paths), so every
// replacement must be JSON-safe: markers contain no quotes or backslashes,
// and spans are narrowed to the secret token so a match can never consume
// surrounding JSON structure.
var redactPatterns = []struct {
	re          *regexp.Regexp
	replacement string
}{
	// --- Credentials & secrets -------------------------------------------
	// KEY=value assignments. Value match stops at whitespace OR a quote so
	// redacting raw JSON bytes never eats a closing quote and corrupts the JSON.
	{regexp.MustCompile(`(?i)\b(API[_-]?KEY|APIKEY|ACCESS[_-]?KEY|SECRET[_-]?KEY|CLIENT[_-]?SECRET|SECRET|PASSWORD|PASSWD|PWD|PRIVATE[_-]?KEY|AUTH[_-]?TOKEN|TOKEN|CREDENTIALS?|SESSION[_-]?KEY)\s*=\s*[^\s"']+`), "$1=[REDACTED]"},
	// Same secret-ish keys as a JSON/YAML "key": "value" pair. The value is
	// rebuilt quoted so the surrounding JSON stays valid.
	{regexp.MustCompile(`(?i)("(?:api[_-]?key|apikey|access[_-]?key|secret[_-]?key|client[_-]?secret|secret|password|passwd|pwd|private[_-]?key|auth[_-]?token|token|credentials?|session[_-]?key)"\s*:\s*)"[^"]*"`), `${1}"[REDACTED]"`},
	// Credentials embedded in a connection string / URL (scheme://user:pass@).
	// Keep the scheme + user, drop the password up to the @.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:@/"']+):[^\s@/"']+@`), "$1:[REDACTED]@"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[REDACTED_AWS_KEY]"},
	{regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`), "[REDACTED_GITHUB_TOKEN]"},
	{regexp.MustCompile(`\bghs_[A-Za-z0-9]{36}\b`), "[REDACTED_GITHUB_TOKEN]"},
	// OpenAI / Anthropic-style keys (sk-, sk-proj-, sk-ant-, ...).
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), "[REDACTED_LLM_KEY]"},
	// Slack tokens (bot/user/app/refresh).
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`), "[REDACTED_SLACK_TOKEN]"},
	// HTTP bearer credentials.
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}=*`), "Bearer [REDACTED]"},
	// JSON Web Tokens (header.payload.signature).
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[REDACTED_JWT]"},
	{regexp.MustCompile(`-----BEGIN[A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z0-9 ]*PRIVATE KEY-----`), "[REDACTED_PEM_BLOCK]"},
	// Promptster's own credentials: candidate keys are live auth until the
	// session completes, and the results page is key-authenticated — they must
	// not survive into terminal-command events or replay payloads.
	{regexp.MustCompile(`\bPST-[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}\b`), "[REDACTED_PROMPTSTER_KEY]"},
	{regexp.MustCompile(`\bpsk_live_[A-Za-z0-9_-]{20,}`), "[REDACTED_PROMPTSTER_ORG_KEY]"},

	// --- PII / business data ---------------------------------------------
	{regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,24}\b`), "[REDACTED_EMAIL]"},
	{regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), "[REDACTED_SSN]"},
	// Phone numbers that carry separators / parens / a country code. Bare
	// 10-digit runs are left alone — they're far more often IDs or timestamps.
	{regexp.MustCompile(`(?:\+?1[ .\-]?)?(?:\(\d{3}\)[ .\-]?|\d{3}[ .\-])\d{3}[ .\-]\d{4}`), "[REDACTED_PHONE]"},
	{regexp.MustCompile(`\+[1-9]\d{9,14}\b`), "[REDACTED_PHONE]"},
	// RFC1918 private IPv4 — internal infrastructure addressing.
	{regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})\b`), "[REDACTED_PRIVATE_IP]"},
}

var (
	titusOnce sync.Once
	titusCore *scanner.Core
)

// titusScanner returns the process-wide Titus core, or nil when init failed
// (callers then rely on the supplemental patterns alone). Init compiles the
// builtin ruleset once; hook processes are one-shot so this runs per event
// (~tens of ms), watchers amortize it across the session.
func titusScanner() *scanner.Core {
	titusOnce.Do(func() {
		core, err := scanner.NewCore("builtin", nil)
		if err != nil {
			hookDebugf("titus scanner init failed, supplemental redaction only: %v", err)
			return
		}
		titusCore = core
	})
	return titusCore
}

func redactBytes(input []byte) []byte {
	out := titusRedact(input)
	// Credit cards before the phone pattern so a 13-19 digit run isn't half-
	// eaten as a phone number first.
	out = redactCreditCards(out)
	for _, p := range redactPatterns {
		out = p.re.ReplaceAll(out, []byte(p.replacement))
	}
	// High-entropy catch-all runs last: the named patterns above have already
	// replaced everything they recognise with low-entropy markers, so this only
	// sees the leftovers (and never re-redacts a marker).
	out = redactHighEntropy(out)
	return out
}

type redactSpan struct {
	start, end int
	ruleID     string
}

// isSecretMaterial reports whether b can plausibly be part of key/token
// material. JSON structure (quotes, braces, colons, commas) and whitespace
// are not — span edges are trimmed past them so a splice never breaks JSON.
func isSecretMaterial(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b == '-', b == '+', b == '/', b == '=', b == '.', b == '~':
		return true
	}
	return false
}

// titusRedact replaces every Titus match in input with [REDACTED:<rule-id>].
// Best-effort by design: on any engine error the input passes through to the
// supplemental layer untouched rather than blocking capture.
func titusRedact(input []byte) []byte {
	core := titusScanner()
	if core == nil || len(input) == 0 {
		return input
	}
	res, err := core.Scan(string(input), "payload")
	if err != nil || res == nil || len(res.Matches) == 0 {
		return input
	}

	spans := make([]redactSpan, 0, len(res.Matches))
	for _, m := range res.Matches {
		s, e := int(m.Location.Offset.Start), int(m.Location.Offset.End)
		if s < 0 || e > len(input) || s >= e {
			continue
		}
		// Rule patterns often span more than the secret itself — a trailing
		// boundary char, or for generic rules the `apiKey":"` context before
		// the value. Splicing those bytes out of raw JSON corrupts it, so
		// narrow the span to the captured secret: the named token group when
		// the rule has one, else the longest positional group (Titus rules
		// capture the secret value as a group either way). The named group
		// must take strict precedence — a longer positional group can span
		// JSON context whose alphanumeric edges survive the trim below.
		secret := m.NamedGroups["token"]
		if len(secret) == 0 {
			for _, g := range m.Groups {
				if len(g) > len(secret) {
					secret = g
				}
			}
		}
		if len(secret) > 0 {
			if idx := bytes.Index(input[s:e], secret); idx >= 0 {
				s, e = s+idx, s+idx+len(secret)
			}
		}
		// Final JSON-safety invariant: never let a span edge sit on a byte
		// that can't be secret material (quotes, braces, whitespace, ...).
		for e > s && !isSecretMaterial(input[e-1]) {
			e--
		}
		for s < e && !isSecretMaterial(input[s]) {
			s++
		}
		if s >= e {
			continue
		}
		spans = append(spans, redactSpan{start: s, end: e, ruleID: m.RuleID})
	}
	if len(spans) == 0 {
		return input
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	var out bytes.Buffer
	out.Grow(len(input))
	cursor := 0
	for _, sp := range spans {
		if sp.start < cursor {
			// Overlaps a span already redacted; extend the cut if it reaches further.
			if sp.end > cursor {
				cursor = sp.end
			}
			continue
		}
		out.Write(input[cursor:sp.start])
		out.WriteString("[REDACTED:" + sp.ruleID + "]")
		cursor = sp.end
	}
	out.Write(input[cursor:])
	return out.Bytes()
}

// scrubEvent redacts secrets from a fully-built Event by round-tripping it
// through JSON. This is the defense-in-depth pass at the buffer/ingest choke
// point: sources that build events from un-redacted text (shell hook commands,
// decision/explain rationales) get scrubbed here, sources that already redact
// their raw input (claude/cursor hooks, codex-watch, git watcher) pass through
// unchanged. Must run BEFORE the event is signed — scrubbing after signing
// would break chain verification.
func scrubEvent(event *Event) {
	if event == nil {
		return
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	scrubbed := redactBytes(raw)
	if bytes.Equal(scrubbed, raw) {
		return
	}
	var clean Event
	if err := json.Unmarshal(scrubbed, &clean); err != nil {
		// Redaction broke the JSON (should be impossible — markers are
		// JSON-safe and spans are token-narrowed). Fail closed: rebuild the
		// envelope from scratch keeping only routing metadata, so no field —
		// present or added later — can carry the secret-bearing payload
		// through this branch.
		hookDebugf("scrubEvent reparse failed, dropping event payload: %v", err)
		*event = Event{
			ID:        event.ID,
			SessionID: event.SessionID,
			Ts:        event.Ts,
			Kind:      event.Kind,
			Source:    event.Source,
			V:         event.V,
			Data:      map[string]interface{}{"scrubbed": "payload dropped: redaction produced unparseable JSON"},
		}
		return
	}
	*event = clean
}

// ccCandidate matches a 13-19 digit run with optional single space/dash
// separators (the way card numbers are written). It over-matches on purpose;
// redactCreditCards confirms each hit with a Luhn check before redacting so
// ordinary long numeric IDs survive.
var ccCandidate = regexp.MustCompile(`\b\d[\d -]{11,17}\d\b`)

// redactCreditCards replaces Luhn-valid 13-19 digit card numbers with a marker.
// The Luhn gate keeps the false-positive rate on arbitrary numeric IDs to ~10%.
func redactCreditCards(in []byte) []byte {
	return ccCandidate.ReplaceAllFunc(in, func(m []byte) []byte {
		digits := make([]byte, 0, len(m))
		for _, c := range m {
			if c >= '0' && c <= '9' {
				digits = append(digits, c)
			}
		}
		if n := len(digits); n < 13 || n > 19 {
			return m
		}
		if !luhnValid(digits) {
			return m
		}
		return []byte("[REDACTED_CC]")
	})
}

// luhnValid reports whether the ASCII digit string passes the Luhn checksum.
func luhnValid(digits []byte) bool {
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if alt {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

// High-entropy catch-all tuning. A token must clear ALL of these to be cut:
// long enough to be a credential, mixing letters and digits, and dense enough
// that it can't be ordinary structured text. The 4.3 bits/char floor sits above
// hex (≤4.0, so UUIDs and git SHAs survive) and above English-ish camelCase /
// snake_case identifiers (~4.0-4.2), but below random base62/base64 tokens
// (~4.4+), which is exactly the org-internal secret shape we want to catch.
const (
	highEntropyMinLen = 24
	highEntropyBits   = 4.3
)

// redactHighEntropy splices out every high-entropy token. Tokens are maximal
// runs of secret-material bytes (isSecretMaterial), so a splice never touches
// JSON structure or whitespace — the same invariant titusRedact relies on.
func redactHighEntropy(in []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(in))
	i := 0
	for i < len(in) {
		if !isSecretMaterial(in[i]) {
			out.WriteByte(in[i])
			i++
			continue
		}
		j := i
		for j < len(in) && isSecretMaterial(in[j]) {
			j++
		}
		if tok := in[i:j]; isHighEntropyToken(tok) {
			out.WriteString("[REDACTED_HIGH_ENTROPY]")
		} else {
			out.Write(tok)
		}
		i = j
	}
	return out.Bytes()
}

func isHighEntropyToken(tok []byte) bool {
	if len(tok) < highEntropyMinLen {
		return false
	}
	var hasDigit, hasLetter bool
	for _, c := range tok {
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			hasLetter = true
		}
	}
	if !hasDigit || !hasLetter {
		return false
	}
	return shannonBits(tok) >= highEntropyBits
}

// shannonBits returns the Shannon entropy of b in bits per byte.
func shannonBits(b []byte) float64 {
	var freq [256]int
	for _, c := range b {
		freq[c]++
	}
	n := float64(len(b))
	h := 0.0
	for _, f := range freq {
		if f == 0 {
			continue
		}
		p := float64(f) / n
		h -= p * math.Log2(p)
	}
	return h
}
