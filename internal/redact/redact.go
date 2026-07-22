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

// Secret redaction runs in THREE ordered stages, all behind RedactBytes:
//
//  1. vendorPatterns — precise, high-confidence provider shapes we own (AWS
//     AKIA, GitHub ghp_/ghs_, Anthropic sk-ant-, OpenAI sk-/sk-proj-, Slack
//     xox*, JWT, PEM blocks, Promptster's own PST-/PSE-/psk_live_ keys). These
//     leave an ATTRIBUTED marker naming the vendor.
//  2. Titus (praetorian-inc/titus, Apache-2.0) — ~490 entropy-aware provider
//     rules. The wide net for everything stage 1 doesn't enumerate. Engine
//     init is once per process; if it ever fails the other stages still run,
//     so capture is never blocked and never unscrubbed.
//  3. genericPatterns — shape-blind fallbacks: KEY=value / JSON key:value with
//     a secret-ish NAME, URL-embedded passwords, and PII / business data
//     (email, SSN, phone, private IPs) that a provider-rule scanner misses.
//
// WHY VENDOR RUNS BEFORE TITUS (this ordering is load-bearing, not cosmetic).
// The marker left behind is the ONLY provenance anything downstream has once
// the value is gone — it is what lets the credential paste board say "an
// Anthropic key was pasted" instead of "a secret was pasted". Titus narrows its
// replacement span to the rule's captured group, and its generic rules capture
// only PART of a key: `sk-ant-api03-…` came back as `sk-[REDACTED:np.generic.2]`,
// leaving a bare `sk-` behind AND destroying vendor attribution, because our
// precise `\bsk-ant-` rule then had nothing left to match. Running our own
// precise rules first means Titus only ever sees what we could not name.
//
// WHY GENERIC RUNS LAST, AND WHY IT REFUSES TO TOUCH AN EXISTING MARKER. The
// generic rules match on the NAME, not the value, so they would happily rewrite
// `ANTHROPIC_API_KEY=[REDACTED_ANTHROPIC_KEY]` down to a bare `[REDACTED]` and
// undo both stages above — the common case, since a pasted key usually arrives
// as an assignment. Their value patterns therefore exclude a leading `[`. RE2
// has no lookahead, so that guard is a first-character class; scrubSecrets.ts
// expresses the same rule as an explicit `val.startsWith('[REDACTED_')` check
// because JS regex callbacks allow one.
//
// Every stage operates on raw JSON bytes (the hook + codex paths), so every
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
type redactRule struct {
	re          *regexp.Regexp
	replacement string
}

// alreadyRedactedValue matches an assignment, JSON pair, or URL credential whose
// VALUE is already a redaction marker — `KEY=[REDACTED…]`, `"key": "[REDACTED…]"`,
// `scheme://user:[REDACTED]@`.
//
// Stage 3 skips any match that satisfies this. Those rules run after the vendor
// and Titus stages, so a value they see may already read `[REDACTED_AWS_KEY]`,
// and rewriting it to a bare `[REDACTED]` would destroy the attribution the
// stage ordering exists to produce.
//
// This is a POST-MATCH test rather than a character class inside each pattern.
// RE2 has no lookahead, and the obvious approximation — excluding `[` from the
// value's first byte — silently stops redacting any real secret that happens to
// start with `[`, e.g. `API_KEY=[customer-secret]`. Narrowing a redaction rule to
// protect a marker is exactly the trade that must not be made silently, so the
// test is written against the marker itself. Mirrors scrubSecrets.ts, which
// expresses the same guard as `val.startsWith('[REDACTED')`.
// The trailing class is what makes this a MARKER test rather than a prefix
// test: a real marker is `[REDACTED]`, `[REDACTED_AWS_KEY]` or
// `[REDACTED:np.stripe.1]`, so the token must end right after REDACTED or
// continue with `_` / `:`. Without it, a genuine secret whose value merely
// begins with the letters REDACTED — `TOKEN=[REDACTEDish-not-a-marker]` — would
// be mistaken for a marker and left in plaintext.
var alreadyRedactedValue = regexp.MustCompile(`[=:]\s*"?\[REDACTED[\]_:]`)

// Stage 1 — precise vendor shapes. Within this block sk-ant- precedes the
// generic sk- rule, or the latter would consume it.
var vendorPatterns = []redactRule{
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[REDACTED_AWS_KEY]"},
	{regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`), "[REDACTED_GITHUB_TOKEN]"},
	{regexp.MustCompile(`\bghs_[A-Za-z0-9]{36}\b`), "[REDACTED_GITHUB_TOKEN]"},
	// OpenAI / Anthropic-style keys (sk-, sk-proj-, sk-ant-, ...), SPLIT by vendor
	// so the board can answer "whose key did an engineer paste into the agent?".
	// The Anthropic body is `{16,}` because it is measured AFTER a longer literal
	// prefix — not a weaker match than the generic `{20,}`. Older CLIs still emit
	// [REDACTED_LLM_KEY]; the backend classifier keeps accepting that marker and
	// degrades it to an unattributed LLM-key finding.
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}`), "[REDACTED_ANTHROPIC_KEY]"},
	{regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}`), "[REDACTED_OPENAI_KEY]"},
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

	// --- Shape-matched, vendor-UNATTRIBUTED ------------------------------
	//
	// Precise vendor prefixes that neither Titus nor the rules above cover, all
	// collapsed to ONE marker on purpose. We know the vendor at each rule site
	// and deliberately throw it away: half the ecosystem has converged on the
	// same `<prefix>_<body>` shape, so per-vendor markers would mean a new
	// marker + provider enum + map entry for every SaaS that ships an API, and
	// each of those is an unguarded join that can silently drift to
	// `other`/`high`. One marker that always means "a real key, of some vendor,
	// left the machine" is the durable contract; the replay gives the analyst
	// the actual prompt, which carries more context than a vendor name would.
	//
	// This marker is CRITICAL downstream, unlike the generic `[REDACTED]` from
	// the name-only assignment rules above. The difference is evidence: those
	// fire on a secret-ish NAME with an unverified value, while these fire on a
	// value SHAPE that only one thing produces.
	//
	// Every rule here is an exact-length or long-minimum prefix match. Do NOT
	// add a bare-entropy or bare-hex rule: an entropy pass was tried and
	// reverted (see the header) because it collapsed msg_/toolu_/call_ provider
	// IDs and broke turn dedup. Shapes with no prefix — an AWS SECRET access key
	// (40 bare base64) or a Datadog key (32 bare hex) — are indistinguishable
	// from a hash or an ID and are intentionally left to the KEY=value rules.
	//
	// A rule here must match the CREDENTIAL, not the identifier that names it.
	// Twilio is the worked example and the reason this paragraph exists: its API
	// Key SID (`SK` + 32 hex) has a beautifully precise shape, and it is not a
	// secret — it grants nothing on its own, ships in logs, URLs and API
	// responses, and the thing that actually authenticates is a separately issued
	// 32-char alphanumeric secret with NO prefix. A rule on the SID is the worst
	// of both: it fires `critical` — "rotate this now" — on a value with nothing
	// to rotate, while the real credential still walks past, and false criticals
	// are what turn a rotation list back into noise. The precise shape is bait.
	// Same trap in AWS (`AKIA…` key id vs the 40-char secret), Twilio account
	// SIDs (`AC…`), and every other id/secret pair. Check which half you matched.
	{regexp.MustCompile(`\bwhsec_[A-Za-z0-9+/=_-]{16,}`), "[REDACTED_SECRET_KEY]"},  // Svix / Supabase / Stripe webhook signing
	{regexp.MustCompile(`\bhf_[A-Za-z0-9]{34}\b`), "[REDACTED_SECRET_KEY]"},         // Hugging Face
	{regexp.MustCompile(`\bsntrys_[A-Za-z0-9+/=_-]{40,}`), "[REDACTED_SECRET_KEY]"}, // Sentry
	{regexp.MustCompile(`\bsecret_[A-Za-z0-9]{43}\b`), "[REDACTED_SECRET_KEY]"},     // Notion internal integration
}

// Stage 3 — shape-blind fallbacks, applied AFTER Titus. Every value pattern here
// refuses a leading `[` so it can never rewrite a marker stage 1 or 2 produced.
var genericPatterns = []redactRule{
	// KEY=value assignments. Value match stops at whitespace OR a quote so
	// redacting raw JSON bytes never eats a closing quote and corrupts the JSON.
	// PWD is intentionally excluded — it's the working-directory env var far
	// more often than a password, and clobbering it destroys replay context.
	//
	// NAME_PREFIX is why these rules are not simply `\b(API[_-]?KEY|...)`. `_` is
	// a word character, so a leading `\b` can never match inside a prefixed name:
	// `STRIPE_API_KEY=…`, `ACME_DB_PASSWORD=…` and `MY_CLIENT_SECRET=…` all sailed
	// through this layer unredacted, and only landed masked when their VALUE
	// happened to carry a known provider shape Titus recognizes (`ghp_`, `sk-ant-`,
	// `AKIA`). An org-internal secret with an opaque value matched nothing and was
	// stored in plaintext — verified against the live teams DB, where the only key
	// names this rule ever masked were the bare ones (API_KEY, TOKEN, SECRET,
	// PASSWORD). Prefixed names are the common case in real .env files, so this was
	// the majority of the space.
	//
	// The widening is a PREFIX ONLY — deliberately no trailing `[A-Za-z0-9_.-]*`.
	// A trailing wildcard would swallow `MAX_TOKENS=4096`, `INPUT_TOKENS=…`,
	// `OUTPUT_TOKENS=…` (TOKEN is a prefix of TOKENS), destroying live spend
	// telemetry to redact a number. Requiring the name to END on a secret word
	// keeps `AWS_SECRET_ACCESS_KEY` (ends on ACCESS_KEY) while dropping
	// `AWS_ACCESS_KEY_ID` — which is the public half and not a secret anyway.
	//
	// The key NAME is preserved on purpose: `STRIPE_API_KEY=[REDACTED]` is what
	// makes the paste board actionable ("a Stripe key was pasted"), where a bare
	// `[REDACTED]` is not. Only the value is ever cut.
	//
	// The value matches ANY non-space run, including one starting with `[`, so a
	// real secret like `API_KEY=[customer-secret]` is still masked. Markers
	// written by an earlier stage are protected by skipIfMarked, not by narrowing
	// the value class — see the field's comment for why that distinction matters.
	{regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:API[_-]?KEY|APIKEY|ACCESS[_-]?KEY|SECRET[_-]?KEY|CLIENT[_-]?SECRET|SECRET|PASSWORD|PASSWD|PRIVATE[_-]?KEY|AUTH[_-]?TOKEN|TOKEN|CREDENTIALS?|SESSION[_-]?KEY))\s*=\s*[^\s"']+`), "$1=[REDACTED]"},
	// Same secret-ish keys as a JSON/YAML "key": "value" pair. The value match
	// `(?:[^"\\]|\\.)*` honours backslash-escaped quotes so it can't stop short
	// inside the value and mis-bound the JSON. Value is rebuilt quoted. Carries
	// the same prefix widening (and the same no-trailing-wildcard rule) as the
	// assignment form above — `"STRIPE_API_KEY": "…"` was equally unmasked — and
	// the same skipIfMarked guard against re-wrapping an attributed marker. The
	// whole value is optional so an empty `""` still matches harmlessly.
	{regexp.MustCompile(`(?i)("[A-Za-z0-9_.-]*(?:api[_-]?key|apikey|access[_-]?key|secret[_-]?key|client[_-]?secret|secret|password|passwd|private[_-]?key|auth[_-]?token|token|credentials?|session[_-]?key)"\s*:\s*)"(?:[^"\\]|\\.)*"`), `${1}"[REDACTED]"`},
	// Credentials embedded in a connection string / URL (scheme://user:pass@).
	// Username is optional so userless DSNs (redis://:pass@, amqp://:pass@) are
	// caught too. Keep scheme + user, drop the password up to the @.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:@/"']*):[^\s@/"']+@`), "$1:[REDACTED]@"},

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

// RedactBytes masks secrets in raw bytes through the three ordered stages
// documented at the top of this file: precise vendor shapes, then Titus's wide
// net, then shape-blind generic fallbacks. The order is what preserves vendor
// ATTRIBUTION in the marker; see that header before reordering anything.
func RedactBytes(input []byte) []byte {
	out := input
	for _, p := range vendorPatterns {
		out = p.re.ReplaceAll(out, []byte(p.replacement))
	}
	out = titusRedact(out)
	for _, p := range genericPatterns {
		// Stage 3 is shape-blind, so it must not touch a value the attributed
		// stages above already marked — see alreadyRedactedValue. Rules whose
		// match can never contain a `KEY=` / `"key":` pair (email, SSN, phone, IP)
		// are unaffected by the test; it simply never fires for them.
		rule := p
		out = rule.re.ReplaceAllFunc(out, func(m []byte) []byte {
			if alreadyRedactedValue.Match(m) {
				return m
			}
			return rule.re.Expand(nil, []byte(rule.replacement), m, rule.re.FindSubmatchIndex(m))
		})
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
