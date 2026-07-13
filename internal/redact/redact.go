package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"sync"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
	"github.com/praetorian-inc/titus/pkg/scanner"
)

// Secret redaction runs in two layers, both behind redactBytes:
//
//  1. Titus (praetorian-inc/titus, Apache-2.0) — ~490 entropy-aware provider
//     rules (AWS, GitHub, Anthropic, OpenAI, Slack, PEM, JWT, ...). Engine
//     init is once per process; if it ever fails the supplemental layer below
//     still runs, so capture is never blocked and never unscrubbed.
//  2. Supplemental regex patterns — Promptster's own credentials, generic
//     credential shapes (KEY=value / JSON key:value, bearer headers, URL-
//     embedded passwords) and PII / business data (email, SSN, phone, private
//     IPs) that a provider-rule secret scanner doesn't cover.
//
// Both layers operate on raw JSON bytes (the hook + codex paths), so every
// replacement must be JSON-safe: markers contain no quotes or backslashes,
// spans are narrowed to the secret token, and value matches stop at quotes —
// so a match can never consume surrounding JSON structure.
//
// Deliberately NOT here: a blunt entropy catch-all or a digit-run credit-card
// pass. Both ran on the raw line before the normalizers parse it — the entropy
// pass collapsed provider message IDs (msg_/toolu_/call_) to a constant and
// broke turn dedup, and an unquoted [REDACTED_CC] marker corrupted bare numeric
// JSON values. Any future org-internal-secret heuristic must run JSON-aware over
// string values only, skipping structural ID fields.
var redactPatterns = []struct {
	re          *regexp.Regexp
	replacement string
}{
	// --- Credentials & secrets -------------------------------------------
	// KEY=value assignments. Value match stops at whitespace OR a quote so
	// redacting raw JSON bytes never eats a closing quote and corrupts the JSON.
	// PWD is intentionally excluded — it's the working-directory env var far
	// more often than a password, and clobbering it destroys replay context.
	{regexp.MustCompile(`(?i)\b(API[_-]?KEY|APIKEY|ACCESS[_-]?KEY|SECRET[_-]?KEY|CLIENT[_-]?SECRET|SECRET|PASSWORD|PASSWD|PRIVATE[_-]?KEY|AUTH[_-]?TOKEN|TOKEN|CREDENTIALS?|SESSION[_-]?KEY)\s*=\s*[^\s"']+`), "$1=[REDACTED]"},
	// Same secret-ish keys as a JSON/YAML "key": "value" pair. The value match
	// `(?:[^"\\]|\\.)*` honours backslash-escaped quotes so it can't stop short
	// inside the value and mis-bound the JSON. Value is rebuilt quoted.
	{regexp.MustCompile(`(?i)("(?:api[_-]?key|apikey|access[_-]?key|secret[_-]?key|client[_-]?secret|secret|password|passwd|private[_-]?key|auth[_-]?token|token|credentials?|session[_-]?key)"\s*:\s*)"(?:[^"\\]|\\.)*"`), `${1}"[REDACTED]"`},
	// Credentials embedded in a connection string / URL (scheme://user:pass@).
	// Username is optional so userless DSNs (redis://:pass@, amqp://:pass@) are
	// caught too. Keep scheme + user, drop the password up to the @.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:@/"']*):[^\s@/"']+@`), "$1:[REDACTED]@"},
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
	// Teams per-engineer ingest credential (PSE-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX).
	// Long-lived auth that identifies a developer — must never survive into
	// captured content. One-or-more segments so any key length the backend mints
	// is redacted (it currently mints six; older keys had two).
	{regexp.MustCompile(`\bPSE-(?:[A-HJ-NP-Z2-9]{4}-)+[A-HJ-NP-Z2-9]{4}\b`), "[REDACTED_PROMPTSTER_ENGINEER_KEY]"},
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
			state.HookDebugf("titus scanner init failed, supplemental redaction only: %v", err)
			return
		}
		titusCore = core
	})
	return titusCore
}

func RedactBytes(input []byte) []byte {
	out := titusRedact(input)
	for _, p := range redactPatterns {
		out = p.re.ReplaceAll(out, []byte(p.replacement))
	}
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
func ScrubEvent(ev *event.Event) {
	if ev == nil {
		return
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	scrubbed := RedactBytes(raw)
	if bytes.Equal(scrubbed, raw) {
		return
	}
	var clean event.Event
	if err := json.Unmarshal(scrubbed, &clean); err != nil {
		// Redaction broke the JSON (should be impossible — markers are
		// JSON-safe and spans are token-narrowed). Fail closed: rebuild the
		// envelope from scratch keeping only routing metadata, so no field —
		// present or added later — can carry the secret-bearing payload
		// through this branch.
		state.HookDebugf("scrubEvent reparse failed, dropping event payload: %v", err)
		*ev = event.Event{
			ID:        ev.ID,
			SessionID: ev.SessionID,
			Ts:        ev.Ts,
			Kind:      ev.Kind,
			Source:    ev.Source,
			V:         ev.V,
			Data:      map[string]interface{}{"scrubbed": "payload dropped: redaction produced unparseable JSON"},
		}
		return
	}
	*ev = clean
}
