package redact

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Client-side source exclusion — the on-device twin of the backend's teams
// write-boundary projection. projectEvent runs at the buffer/ingest choke point
// (appendEventToLocalBuffer, before signing), so source-bearing fields are
// stripped BEFORE the event is signed, persisted to the local buffer, or put on
// the wire. The backend applies the same default-deny allowlist again at its
// write boundary (defense-in-depth for older CLI versions), and the teams DB
// carries no-source CHECK constraints below that. This pass is what upgrades
// the product claim from "source is never stored" to "source never leaves the
// engineer's machine".
//
// LOCKSTEP: this allowlist mirrors TEAMS_FIELD_ALLOWLIST in the backend's
// packages/shared/src/eventFieldProjection.ts. Any change there must land here
// (and vice versa) in the same release. A new event kind or field persists
// NOTHING until it is added on both sides — the safe default is exclusion.

// projectUsageFields — per-request token usage: numbers plus a model id, no
// source. Shared between ai_response (main chain) and subagent_usage
// (sidechains). reasoningTokens is OpenAI's reasoning_output_tokens (Codex) — a
// content-free integer the backend uses for reasoning-model pricing; absent on
// providers that don't report it (dropped-by-omission, never leaked).
var projectUsageFields = []string{
	"model",
	"usageScope",
	"inputTokens",
	"outputTokens",
	"reasoningTokens",
	"cacheReadTokens",
	"cacheWriteTokens",
	"cacheWrite5mTokens",
	"cacheWrite1hTokens",
}

// projectFieldAllowlist — default-deny, per-kind field allowlist. Fields that
// are definitionally customer source or machine output (diff, oldString/
// newString, content, stdout/stderr, tool args/results, assistant text) are
// listed for NO kind, so they can never survive projection.
var projectFieldAllowlist = map[string][]string{
	// Human conversational text (secret-redacted upstream by redactBytes).
	// prompt.command is the slash-command NAME, never the expanded body.
	// followsInterrupt is a boolean flag marking a redirect prompt (the one
	// typed right after an ESC/Ctrl+C interrupt); its relatedEventIds linkage
	// lives on the envelope, outside Data.
	// promptSource is Claude Code's own vendor enum for who authored the turn —
	// "typed" vs harness-injected ("system", which is what <task-notification>
	// blobs carry). Same class as a slash-command name: a short lower-snake token
	// from a fixed vocabulary, shape-clamped at the emitter
	// (normalize.clampPromptSource) so it structurally cannot carry a path, URL,
	// or prose. It's what lets the backend keep pseudo-prompts out of the fluency
	// judge instead of scoring them as an engineer's bad prompting.
	// There is deliberately NO `meta` key: the emitter's meta map carried `cwd`
	// (an absolute filesystem path), and this allowlist keeps KEYS — a map can
	// only be kept whole, so allowlisting it would leak the path. The emitter no
	// longer builds one; `meta` stays pinned as dropped in project_test.go so a
	// smuggled one still dies here.
	// workdir is the session's cwd home-collapsed to "~/…" by the emitter
	// (normalize → state.HomeRelative), so it names the repo/worktree WHERE the
	// session ran without carrying the OS username an absolute path leaks. It is
	// its own individually-allowlisted key precisely so the raw absolute `cwd`
	// can stay dropped (pinned by the leak-canary case in project_test.go): a
	// short home-relative token is allowlistable, an absolute path is not.
	// repoRoot is the canonical per-session repo identity — a git remote slug
	// (owner/name, the same public value stored in outcome_events.repo) or an opaque
	// sha256 hash, resolved on-device in capture. It is canary-safe (never a raw
	// path) and its own individually-allowlisted key, so the backend can key repo
	// rollups + the PR-count join on it; it survives projection exactly like workdir.
	// repoHost is the bare hostname the slug was parsed from ("github.com",
	// "gitlab.com") — scheme, userinfo, port and path all discarded on-device by
	// normalizeRemoteHost, so it is a provider name and structurally cannot carry
	// a path, a URL, or the OS username. It gets its own allowlisted key for the
	// same reason repoRoot does: the slug alone is ambiguous across providers
	// (both hosts' acme/api reduce to "acme/api"), and the backend must be able to
	// require a provider match rather than treat a colliding owner name as one.
	// (model_turn — a backend-proxy kind this CLI never emits — is deliberately
	// absent: unknown kinds project to nothing.)
	"prompt": {"text", "command", "followsInterrupt", "promptSource", "workdir", "repoRoot", "repoHost"},
	// Interrupt (ESC/Ctrl+C mid-response): behavioral metadata only. cutTool is
	// the tool NAME (same class as a slash-command name); subtype/variant are
	// enums. NO cutToolInput — a command body / file path is source-adjacent, so
	// it is neither emitted by the CLI nor allowlisted here (double protection).
	"interrupt": {"subtype", "cutTool", "variant"},
	// ai_response deliberately carries NO text: assistant messages routinely
	// embed source (patches, file bodies). Usage/model metadata only.
	"ai_response": projectUsageFields,
	// `sidechain` marks work done by a subagent. Its events roll up to the
	// PARENT session's id (a subagent transcript records its parent's sessionId),
	// so without this flag subagent work is indistinguishable from the main
	// chain's. The normalizer has always set it; it was silently dropped here.
	// Keep in lockstep with the backend's TEAMS_FIELD_ALLOWLIST.
	"subagent_usage": append(append([]string{}, projectUsageFields...), "attributionSkill", "attributionAgent", "agentId", "sidechain"),
	// File events: PATH + line/byte counts only — never the diff or contents.
	// lineRanges carries WHICH lines were AI as content-free {start,end,
	// attribution} triples (ints + one enum); its element allowlist below is
	// what structurally guarantees no diff/text bytes ride along.
	"file_diff":   {"path", "linesAdded", "linesRemoved", "lineRanges"},
	"file_create": {"path", "linesAdded", "sizeBytes"},
	// credentialKeys is the KEY NAMES harvested on-device from a dotenv-class
	// file the agent read — {"STRIPE_SECRET_KEY", "DATABASE_URL"}, never a value.
	// normalize.HarvestCredentialKeyNames splits each line with strings.Cut and
	// discards the right-hand side, so a value has no path into this field at
	// all; the clamp below is a second, independent filter on shape and volume.
	// ABSENT (not empty) when nothing was harvested — the backend reads absent as
	// "this CLI does not harvest" and empty as "the file held no keys".
	// Lockstep with the backend's TEAMS_FIELD_ALLOWLIST + TEAMS_STRING_ARRAY_CLAMPS.
	"file_read":    {"path", "credentialKeys"},
	"file_search":  {"path", "query"},
	"file_delete":  {"path"},
	"dir_list":     {"path"},
	"editor_focus": {"path"},
	"editor_edit":  {"path"},
	"editor_idle":  {"path"},
	// Command-family: invocation + result metadata — never stdout/stderr.
	// Kept command strings additionally get inline-exec code bodies masked
	// (scrubInlineCommand below).
	"command":    {"command", "exitCode", "durationMs"},
	"test_run":   {"suite", "command", "passed", "failed", "skipped", "durationMs"},
	"build_run":  {"command", "exitCode", "durationMs"},
	"lint_run":   {"command", "exitCode", "durationMs"},
	"git_action": {"action", "ref", "message", "commit"},
	"web_lookup": {"url", "query"},
	// Tooling: identity + status only — never args/results (can embed file bodies).
	"tool_intent":    {"name", "tool", "status"},
	"tool_use":       {"name", "tool", "status", "skill"},
	"tool_result":    {"name", "tool", "status"},
	"tool_decision":  {"name", "tool", "status"},
	"mcp_call":       {"name", "tool", "status"},
	"task_dispatch":  {"name", "status", "summary"},
	"subagent_start": {"name", "status"},
	"subagent_stop":  {"name", "status"},
	// Planning / decisions: engineer-authored prose (prompt-context).
	// planning.status is TaskUpdate's resolved transition — a short token from a
	// fixed vendor vocabulary (pending/in_progress/completed), same class as the
	// status already kept for tool_use/subagent_*. It is what distinguishes plan
	// PROGRESS from plan definition; without it a TaskUpdate projects to {}.
	// NOTE: planning.todos is absent on purpose — legacy TodoWrite task bodies are
	// prose (pinned by TestProjectEvent "planning drops todo bodies").
	"planning":        {"summary", "title", "status"},
	"planning_read":   {"summary", "title"},
	"plan_decision":   {"summary", "title"},
	"context_compact": {"summary", "trigger"},
	"checkpoint":      {"label", "trigger", "summary"},
	"decision_event": {
		"title", "description", "chosenOption", "context", "tradeoffs",
		"rationale", "category", "categoryHint", "severity", "impactScore",
		"capturedVia", "missedDecision",
	},
	// Session / API lifecycle: status metadata only.
	"session_start": {"reason"},
	"session_end":   {"reason"},
	"api_request":   {"status"},
	"api_error":     {"status", "error"},
	// Presence / liveness: device + CLI/host metadata. All non-source.
	"heartbeat": {"device", "cliVersion", "os", "arch", "watching"},
	"presence":  {"device", "cliVersion", "os", "arch", "watching", "state"},
	// Config census: token-count inventory — counts and names only.
	// workspaceKey is a git remote slug (owner/name) or an opaque sha256(path)
	// hash — never a filesystem path and never file contents (pinned by
	// TestConfigCensusWorkspaceKey) — and the backend keys workspace de-dupe on
	// it, so it must survive projection.
	"config_census": {
		"workspaceKey",
		"globalClaudeMdTokens", "projectClaudeMdTokens",
		// WHERE the counted project memory sits: the bare enum root|nested|absent,
		// never a path. It says whether those tokens actually load at launch or
		// only lazily, which coverage and the config tax read in OPPOSITE
		// directions — so a strip here doesn't degrade the signal, it inverts it.
		// LOCKSTEP with the backend's own default-deny allowlist in
		// packages/shared/src/eventFieldProjection.ts: a field missing from
		// EITHER side is dropped silently, with no error and no telemetry.
		"projectClaudeMdPosition",
		"skillListingTokens", "skillCount", "skills",
		"pluginListingTokens", "pluginCount", "plugins",
		"mcpServers", "mcpDeferred",
		// Capture-health counts: integers only (files on disk vs active in 7d),
		// never a transcript path/filename/slug. Content-free by construction.
		"claudeTranscriptsTotal", "claudeTranscriptsActive7d",
	},
	// Post-commit AI attribution: a PUBLIC commit hash + privacy-safe workspace
	// id + a per-file list of content-free line-range attributions. workspaceKey
	// is the same git-slug-or-hash identity config_census sends (never a path);
	// commitSha is a public content hash the notes backend is keyed on. NEVER a
	// diff, file body, commit message, author, or old/new string. The nested
	// files[]/lineRanges[] element allowlists below are the load-bearing privacy
	// line — they clamp both array levels to scalar keys only. aiTokens is a
	// content-free scalar: the o200k tiktoken count of this commit's likely_ai
	// added lines (the denominator for the backend's token-efficiency ratio),
	// never the line text or bytes.
	"commit_attribution": {"commitSha", "workspaceKey", "files", "aiTokens"},
	// durability_verdict reports WHICH AI line ranges survived (durableRanges),
	// were rewritten (churnedRanges), or are still tracked and undecided
	// (livingRanges) on a path over time — content-free metadata: integer line
	// numbers, an age, and a lineage id (a `sha:path` handle, never content).
	// commitSha/workspaceKey/path are the same public identities the other watcher
	// events carry. The three range element allowlists below are the load-bearing
	// privacy line. NEVER a diff, file body, old/new string, or content
	// fingerprint (fingerprints stay on-device). livingRanges introduces no new
	// class of data — it is the SAME scalar shape as the other two.
	// LOCKSTEP with promptster-backend packages/shared/src/eventFieldProjection.ts:
	// a field allowed here but not there is silently dropped at ingest.
	"durability_verdict": {"commitSha", "workspaceKey", "path", "durableRanges", "churnedRanges", "livingRanges", "measuredTsMs"},
	// rework_verdict reports WHICH AI line ranges were rewritten on a feature
	// branch BEFORE it merged (reworkedRanges) — the same content-free metadata as
	// durability: integer line numbers, an age, and a `sha:path` lineage handle,
	// never content. commitSha/workspaceKey/path are the same public identities.
	// The reworkedRanges[] element allowlist below is the load-bearing privacy
	// line. NEVER a diff, file body, old/new string, or content fingerprint.
	"rework_verdict": {"commitSha", "workspaceKey", "path", "reworkedRanges", "measuredTsMs"},
}

// projectArrayElementAllowlist — element-level allowlists for array-of-object
// fields. The top-level projection is shallow (key-only), so an allowlisted
// array field would otherwise carry its elements VERBATIM — including any
// source nested inside. Every array-of-object field in projectFieldAllowlist
// must have an entry here.
var projectArrayElementAllowlist = map[string]map[string][]string{
	"config_census": {
		"skills":     {"slug", "name", "descTokens"},
		"plugins":    {"name", "listingTokens"},
		"mcpServers": {"name", "deferred"},
	},
	// lineRanges elements are content-free by construction (ints + one enum), but
	// this allowlist is the LOAD-BEARING privacy line: it strips every element to
	// exactly these three scalar keys, so a nested `text`/content key (a code
	// leak) can never survive projection.
	"file_diff": {
		"lineRanges": {"start", "end", "attribution"},
	},
	// commit_attribution nests TWO array levels: files[] each carrying a
	// lineRanges[]. projectArrayElements recurses when a kept element field has
	// its own entry here, so `files` keeps {path, lineRanges} and the nested
	// `lineRanges` is itself clamped to {start,end,attribution} — a smuggled
	// non-scalar (a `text`/content key) cannot survive either level. `lineRanges`
	// is NOT a top-level allowlisted field for this kind (only commitSha/
	// workspaceKey/files are), so this entry serves purely as the nested spec.
	"commit_attribution": {
		"files":      {"path", "lineRanges"},
		"lineRanges": {"start", "end", "attribution"},
	},
	// durability_verdict's two range arrays are content-free by construction
	// (ints + one lineage handle), but this allowlist is the LOAD-BEARING privacy
	// line: it strips every element to exactly these scalar keys, so a smuggled
	// `text`/byte/fingerprint key can never survive projection.
	"durability_verdict": {
		"durableRanges": {"start", "end", "ageDays", "lineageId"},
		"churnedRanges": {"start", "end", "ageDays", "lineageId"},
		"livingRanges":  {"start", "end", "ageDays", "lineageId"},
	},
	// rework_verdict's range array is content-free by construction (ints + a
	// lineage handle), but this allowlist is the LOAD-BEARING privacy line: it
	// strips every element to exactly these scalar keys, so a smuggled
	// text/byte/fingerprint key can never survive projection.
	"rework_verdict": {
		"reworkedRanges": {"start", "end", "ageDays", "lineageId"},
	},
}

// stringArrayClamp — shape + volume bounds for an allowlisted array-of-STRING
// field.
type stringArrayClamp struct {
	maxItems  int
	maxLength int
	// allow reports whether an element's shape is acceptable. A false verdict
	// DROPS the element; nothing is ever truncated, because a truncated secret is
	// still a secret prefix and a truncated name is a lie.
	allow func(string) bool
}

// projectStringArrayClamp — element-level clamps for array-of-STRING fields.
//
// projectArrayElements handles arrays of OBJECTS and skips every scalar, so
// WITHOUT this an allowlisted string array would be copied VERBATIM by the
// shallow projection — the whole array trusted because its key was allowlisted.
// That is exactly the trust this file exists not to extend, and it is the same
// hole the backend's TEAMS_STRING_ARRAY_CLAMPS closes. Keep the two in lockstep.
//
// HONEST LIMIT: this cannot prove an element is a NAME rather than a VALUE —
// `AKIAIOSFODNN7EXAMPLE` is a real AWS key id and passes any identifier charset.
// What it buys is volume (a bug can't turn one file_read into a string dump),
// shape (whitespace, quotes, `=`, `/`, `+`, a redaction marker: all dropped) and
// length. The guarantee that no value is ever a candidate comes from the
// producer — normalize.HarvestCredentialKeyNames discards every right-hand side.
var projectStringArrayClamp = map[string]map[string]stringArrayClamp{
	"file_read": {
		"credentialKeys": {maxItems: 40, maxLength: 64, allow: isIdentifierName},
	},
}

// isIdentifierName — leading letter/underscore, then word characters. Mirrors
// normalize.isIdentifierName and the backend's `/^[A-Za-z_][A-Za-z0-9_]*$/`.
// Duplicated rather than imported so the redaction package depends on nothing:
// a projection that could be weakened by an unrelated package's refactor is not
// a default-deny boundary.
func isIdentifierName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			continue
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// projectStringArray clamps an array-of-string field: drop non-strings, drop
// anything failing the shape or length test, de-duplicate, cap the count.
// Order-preserving, so the result is deterministic for a given input. A
// non-array yields an empty slice — the same default-deny posture as
// projectArrayElements.
func projectStringArray(value interface{}, clamp stringArrayClamp) []interface{} {
	arr, isArray := value.([]interface{})
	if !isArray {
		// A []string never reaches here from the normalizer (Data is
		// map[string]interface{} built from JSON), but handle it rather than
		// silently dropping a field a future emitter might set natively.
		if typed, isTyped := value.([]string); isTyped {
			arr = make([]interface{}, 0, len(typed))
			for _, s := range typed {
				arr = append(arr, s)
			}
		} else {
			return []interface{}{}
		}
	}
	out := make([]interface{}, 0, len(arr))
	seen := make(map[string]struct{}, len(arr))
	for _, el := range arr {
		if len(out) >= clamp.maxItems {
			break
		}
		s, isString := el.(string)
		if !isString || s == "" || len(s) > clamp.maxLength {
			continue
		}
		if clamp.allow != nil && !clamp.allow(s) {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// shellCommandKinds — kinds whose kept `command` field is a SHELL INVOCATION
// string (as opposed to prompt.command, a slash-command name). These get
// scrubInlineCommand applied during projection.
var shellCommandKinds = map[string]bool{
	"command":   true,
	"test_run":  true,
	"build_run": true,
	"lint_run":  true,
}

const inlineCodeMarker = "<inline-code-redacted>"

// Quoted inline-exec bodies: flag (-c/-e/--eval) + optional space/`=` +
// optional ANSI-C `$` + quoted body. Separate patterns per quote character:
// double-quoted bodies honor backslash escapes (`-c "print(\"hi\")"` masks
// fully); single-quoted bodies can't contain escaped quotes in shell, so they
// end at the next `'`.
var (
	inlineExecSingle = regexp.MustCompile(`(\s(?:-(?:c|e)|--eval)[= ]?\s*\$?)'[^']*'`)
	inlineExecDouble = regexp.MustCompile(`(\s(?:-(?:c|e)|--eval)[= ]?\s*\$?)"(?:\\.|[^"\\])*"`)
	// Heredoc marker: `<<TAG`, `<<-TAG`, `<<'TAG'`, `<<"TAG"`. Here-strings
	// (`<<<`) are excluded by a preceding-char check in scrubHeredocBodies
	// (RE2 has no lookbehind).
	heredocMarker = regexp.MustCompile(`<<-?[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)`)
)

// scrubInlineCommand masks inline code passed to an interpreter so a kept
// `command` string can't smuggle source. Keeps the interpreter + flag
// (behavioral signal for the backend's command classifiers) and drops only the
// code body. Covers quoted -c/-e/--eval bodies and heredoc bodies (marker line
// survives, body is dropped).
//
// KNOWN LIMITS (defense-in-depth, not a hard guarantee): unquoted inline code
// (`python -c print(1)` — also codex argv joins, which lose quoting) and
// here-strings (`<<<`) aren't masked. Deliberate: matching unquoted bodies
// would false-positive on flags like `gcc -c file.c`. The no-source contract
// rests on the field allowlist + the backend's projection and DB CHECKs.
//
// LOCKSTEP: mirrors scrubInlineCode in the backend's
// packages/shared/src/eventFieldProjection.ts.
func scrubInlineCommand(command string) string {
	out := inlineExecSingle.ReplaceAllString(command, "$1'"+inlineCodeMarker+"'")
	out = inlineExecDouble.ReplaceAllString(out, `$1"`+inlineCodeMarker+`"`)
	return scrubHeredocBodies(out)
}

// scrubHeredocBodies replaces terminated heredoc bodies with the marker,
// keeping the `<<TAG` line and the terminator line intact.
func scrubHeredocBodies(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	var out strings.Builder
	rest := cmd
	for {
		loc := heredocMarker.FindStringSubmatchIndex(rest)
		if loc == nil {
			out.WriteString(rest)
			break
		}
		start, matchEnd := loc[0], loc[1]
		// Exclude here-strings: `<<<TAG` matches the regex starting at its
		// second `<`, which leaves a `<` immediately before the match.
		if start > 0 && rest[start-1] == '<' {
			out.WriteString(rest[:matchEnd])
			rest = rest[matchEnd:]
			continue
		}
		quote := rest[loc[2]:loc[3]]
		tag := rest[loc[4]:loc[5]]
		markerEnd := loc[5]
		if quote != "" {
			// A quoted tag must close with the same quote (`<<'EOF'`).
			if markerEnd >= len(rest) || rest[markerEnd:markerEnd+1] != quote {
				out.WriteString(rest[:markerEnd])
				rest = rest[markerEnd:]
				continue
			}
			markerEnd++
		}
		nl := strings.Index(rest[markerEnd:], "\n")
		if nl < 0 {
			// Marker with no body in this string — nothing to scrub.
			out.WriteString(rest)
			break
		}
		bodyStart := markerEnd + nl + 1
		termStart := findHeredocTerminator(rest[bodyStart:], tag)
		if termStart < 0 {
			// Unterminated heredoc — leave as-is (parity with the backend scrub).
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:bodyStart])
		out.WriteString(inlineCodeMarker + "\n")
		rest = rest[bodyStart+termStart:]
	}
	return out.String()
}

// findHeredocTerminator returns the offset of the line that terminates a
// heredoc body — the tag alone on its own line (leading tabs/spaces allowed
// for `<<-`) — or -1 if the body is unterminated. A plain line scan; no
// per-heredoc regex compilation on the event hot path.
func findHeredocTerminator(body, tag string) int {
	offset := 0
	for offset <= len(body) {
		lineEnd := strings.Index(body[offset:], "\n")
		line := body[offset:]
		next := len(body) + 1
		if lineEnd >= 0 {
			line = body[offset : offset+lineEnd]
			next = offset + lineEnd + 1
		}
		trimmed := strings.TrimRight(strings.TrimLeft(line, " \t"), " \t")
		if trimmed == tag {
			return offset
		}
		offset = next
	}
	return -1
}

// --- assistant-prose scrubber -------------------------------------------------
//
// Opt-in assistant-prose capture (org-gated, default off). When an org turns on
// GET /v1/teams/policy { captureAssistantProse: true }, ProjectEvent KEEPS the
// ai_response `text` field — but only after scrubbing every code-bearing span
// out of it ON-DEVICE, so narration ("I'll use the frontend-design skill, then
// clear context") survives while patches, fenced blocks, and long inline code
// are replaced with a marker before the text is signed, buffered, or sent. This
// preserves the never-store-source guarantee even with prose capture enabled.
//
// LOCKSTEP: this scrubber must produce BYTE-FOR-BYTE identical output to
// scrubAssistantProse in the backend's packages/shared/src/eventFieldProjection.ts.
// The two are a documented pinned contract (mirrored input→output test tables on
// both sides). Any change to the redaction rules must land in the same release
// on both, or the on-device scrub and the backend's defense-in-depth re-scrub
// diverge. See scrubInlineCommand above for the sibling command-body scrubber.

const proseRedactionMarker = "<code-redacted>"

// inlineSpanMax is the max length (in runes) of an inline `backtick` span that
// is kept verbatim; longer spans are code, not a symbol reference, and get
// redacted. Rune count matches the JS UTF-16 length for the ASCII-ish spans
// that occur here; non-BMP input could differ, but that is out of scope.
const inlineSpanMax = 40

var (
	// fenceInfoRe matches an opening/closing code-fence line: up to 3 leading
	// spaces, then a run of 3+ backticks or tildes, then an info string.
	fenceInfoRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	// inlineSpanRe matches a single-line inline code span (no backticks or
	// newlines inside).
	inlineSpanRe = regexp.MustCompile("`([^`\n]*)`")
)

type proseFence struct {
	char   string // "`" or "~"
	length int    // run length of the fence
	info   string // trimmed info string after the fence
}

// fenceInfo parses a line as a code fence, returning ok=false when it is not a
// fence line. Mirrors fenceInfo() in the backend.
func fenceInfo(line string) (proseFence, bool) {
	m := fenceInfoRe.FindStringSubmatch(line)
	if m == nil {
		return proseFence{}, false
	}
	run := m[1]
	return proseFence{
		char:   string(run[0]),
		length: len(run),
		info:   strings.TrimSpace(m[2]),
	}, true
}

// diffLineKind reports whether a line looks like part of a diff/patch and, if
// so, whether it is a structural anchor (diff --git / @@ / --- / +++ / index).
// Mirrors diffLineKind() in the backend.
func diffLineKind(line string) (isDiff bool, isAnchor bool) {
	isAnchor = strings.HasPrefix(line, "diff --git ") ||
		strings.HasPrefix(line, "@@ ") ||
		strings.HasPrefix(line, "--- ") ||
		strings.HasPrefix(line, "+++ ") ||
		strings.HasPrefix(line, "index ")
	isMarker := strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")
	return isAnchor || isMarker, isAnchor
}

// scrubInlineSpans redacts inline code spans longer than inlineSpanMax while
// keeping short symbol references (`useState`, `src/x.ts`). Mirrors
// scrubInlineSpans() in the backend.
func scrubInlineSpans(line string) string {
	return inlineSpanRe.ReplaceAllStringFunc(line, func(full string) string {
		// full includes the surrounding backticks; the class excludes them so
		// stripping the first/last byte yields the body.
		body := full[1 : len(full)-1]
		if len([]rune(body)) <= inlineSpanMax {
			return full
		}
		return "`" + proseRedactionMarker + "`"
	})
}

// scrubAssistantProse strips code out of assistant narration, keeping the prose.
// Three passes, mirroring the backend byte-for-byte: (1) fenced code blocks
// collapse to a single marker, (2) unfenced diff/patch runs anchored by a
// diff header collapse to one marker, (3) over-long inline backtick spans are
// redacted on the remaining prose lines.
func scrubAssistantProse(text string) string {
	if text == "" {
		return text
	}
	lines := strings.Split(text, "\n")

	// Pass 1 — fenced code blocks.
	var out []string
	var locked []bool
	for i := 0; i < len(lines); {
		fi, ok := fenceInfo(lines[i])
		if ok && !(fi.char == "`" && strings.Contains(fi.info, "`")) {
			out = append(out, lines[i])
			locked = append(locked, true)
			i++
			body := 0
			closed := false
			for i < len(lines) {
				ci, cok := fenceInfo(lines[i])
				if cok && ci.char == fi.char && ci.length >= fi.length && ci.info == "" {
					if body > 0 {
						out = append(out, proseRedactionMarker)
						locked = append(locked, true)
					}
					out = append(out, lines[i])
					locked = append(locked, true)
					i++
					closed = true
					break
				}
				body++
				i++
			}
			if !closed && body > 0 {
				out = append(out, proseRedactionMarker)
				locked = append(locked, true)
			}
			continue
		}
		out = append(out, lines[i])
		locked = append(locked, false)
		i++
	}

	// Pass 2 — unfenced diff/patch runs collapse to one marker when a run has an anchor.
	var diffed []string
	var diffLocked []bool
	for i := 0; i < len(out); {
		if isDiff, _ := diffLineKind(out[i]); locked[i] || !isDiff {
			diffed = append(diffed, out[i])
			diffLocked = append(diffLocked, locked[i])
			i++
			continue
		}
		j := i
		hasAnchor := false
		for j < len(out) {
			isDiff, _ := diffLineKind(out[j])
			if locked[j] || !isDiff {
				break
			}
			if _, anchor := diffLineKind(out[j]); anchor {
				hasAnchor = true
			}
			j++
		}
		if hasAnchor {
			diffed = append(diffed, proseRedactionMarker)
			diffLocked = append(diffLocked, true)
		} else {
			for k := i; k < j; k++ {
				diffed = append(diffed, out[k])
				diffLocked = append(diffLocked, false)
			}
		}
		i = j
	}

	// Pass 3 — inline backtick spans on remaining prose lines.
	for i := 0; i < len(diffed); i++ {
		if !diffLocked[i] {
			diffed[i] = scrubInlineSpans(diffed[i])
		}
	}

	return strings.Join(diffed, "\n")
}

// projectEvent strips a fully-built Event down to its source-free shape,
// in place. Default-deny: an unknown kind (or a non-map Data) keeps nothing.
// Envelope fields (id, sessionId, ts, kind, source, actor, provenance) are
// untouched; RawPayload — a raw-line preview that can carry source and is not
// covered by the signature — is always cleared.
//
// captureAssistantProse is the org policy (GET /v1/teams/policy), resolved and
// threaded down from the watch loop. When true, an ai_response's `text` is
// KEPT — scrubbed of code by scrubAssistantProse — instead of dropped; when
// false (the default) assistant text is dropped exactly as before. This is a
// conditional special-case, NOT an allowlist entry, so the default projection
// stays source-free even if the flag plumbing regresses.
func ProjectEvent(e *event.Event, captureAssistantProse bool) {
	if e == nil {
		return
	}
	e.RawPayload = ""
	allowed := projectFieldAllowlist[e.Kind]
	data, ok := e.Data.(map[string]interface{})
	if !ok && e.Data != nil {
		// Dropping a non-map payload is the correct, safe default — but when an
		// emitter hands us one it is almost always a bug at the emitter (a typed
		// payload struct assigned straight to Data never asserts to a map, so its
		// whole payload silently becomes {} before signing; see
		// capture.eventDataMap). The default-deny below stands either way; this
		// log just means the next occurrence is discoverable instead of invisible.
		state.HookDebugf("projectEvent: %s Data is %T, not map[string]interface{} — payload dropped", e.Kind, e.Data)
	}
	if ok && len(allowed) == 0 && len(data) > 0 {
		// A kind we actually emit, carrying a payload, with no allowlist entry:
		// the whole payload is about to become {} and nothing downstream will
		// ever know it existed. Default-deny is right, but silence is not — this
		// exact shape shipped census and presence as empty objects for a release.
		//
		// Ungated on purpose (unlike HookDebugf, which needs PROMPTSTER_DEBUG=1):
		// it fires once per unknown kind, not per event, and a payload vanishing
		// without a trace is indistinguishable from one that was never set.
		fmt.Fprintf(os.Stderr, "promptster-teams: redact: kind %q has no field allowlist — its entire payload (%d field(s)) is being dropped\n", e.Kind, len(data))
	}
	if !ok || len(allowed) == 0 {
		e.Data = map[string]interface{}{}
		return
	}
	elementAllowlists := projectArrayElementAllowlist[e.Kind]
	stringClamps := projectStringArrayClamp[e.Kind]
	projected := make(map[string]interface{}, len(allowed))
	for _, key := range allowed {
		value, present := data[key]
		if !present || value == nil {
			continue
		}
		if elementFields, hasElementList := elementAllowlists[key]; hasElementList {
			projected[key] = projectArrayElements(value, elementFields, elementAllowlists)
			continue
		}
		if clamp, hasClamp := stringClamps[key]; hasClamp {
			projected[key] = projectStringArray(value, clamp)
			continue
		}
		projected[key] = value
	}
	if shellCommandKinds[e.Kind] {
		if cmd, isString := projected["command"].(string); isString {
			projected["command"] = scrubInlineCommand(cmd)
		}
	}
	// Opt-in assistant prose: keep ai_response.text (code-scrubbed) only when the
	// org policy is on. lastAssistantMessage stays dropped (redundant with text).
	if captureAssistantProse && e.Kind == "ai_response" {
		if text, isString := data["text"].(string); isString {
			projected["text"] = scrubAssistantProse(text)
		}
	}
	e.Data = projected
}

// projectArrayElements projects each element of an array field down to its
// allowlisted keys. Non-array values and non-object elements yield nothing.
//
// A kept element field that is ITSELF an array-of-objects — it has its own entry
// in the kind's element allowlist (elementAllowlists), e.g.
// commit_attribution.files[].lineRanges — is projected RECURSIVELY against that
// entry, so no non-scalar can ride through a deeper nesting level. A field with
// no such entry is treated as a scalar and kept verbatim; the allowlist author
// asserts (by listing it) that it carries no source. Recursion terminates on
// the DATA's finite nesting depth, so a self-referential spec cannot loop.
func projectArrayElements(value interface{}, fields []string, elementAllowlists map[string][]string) []interface{} {
	arr, ok := value.([]interface{})
	if !ok {
		return []interface{}{}
	}
	out := make([]interface{}, 0, len(arr))
	for _, el := range arr {
		obj, isObj := el.(map[string]interface{})
		if !isObj {
			continue
		}
		projected := make(map[string]interface{}, len(fields))
		for _, field := range fields {
			v, present := obj[field]
			if !present || v == nil {
				continue
			}
			if nested, hasNested := elementAllowlists[field]; hasNested {
				projected[field] = projectArrayElements(v, nested, elementAllowlists)
				continue
			}
			// No nested allowlist entry: this field must be a scalar. The
			// allowlist limits key NAMES; without this type check a map or
			// slice smuggled into a scalar key (start/end/attribution/path)
			// would ride through verbatim. Clamp kept values to scalars.
			if isScalar(v) {
				projected[field] = v
			}
		}
		out = append(out, projected)
	}
	return out
}

// isScalar reports whether v is a leaf JSON value safe to copy verbatim through
// an element allowlist: nil, string, bool, or any numeric type. Maps and slices
// (which could nest smuggled source) return false.
func isScalar(v interface{}) bool {
	switch v.(type) {
	case nil, string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return true
	default:
		return false
	}
}
