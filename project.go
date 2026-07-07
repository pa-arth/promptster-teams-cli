package main

import (
	"regexp"
	"strings"
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
// (sidechains).
var projectUsageFields = []string{
	"model",
	"usageScope",
	"inputTokens",
	"outputTokens",
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
	// (model_turn — a backend-proxy kind this CLI never emits — is deliberately
	// absent: unknown kinds project to nothing.)
	"prompt": {"text", "command"},
	// ai_response deliberately carries NO text: assistant messages routinely
	// embed source (patches, file bodies). Usage/model metadata only.
	"ai_response":    projectUsageFields,
	"subagent_usage": append(append([]string{}, projectUsageFields...), "attributionSkill", "attributionAgent", "agentId"),
	// File events: PATH + line/byte counts only — never the diff or contents.
	"file_diff":    {"path", "linesAdded", "linesRemoved"},
	"file_create":  {"path", "linesAdded", "sizeBytes"},
	"file_read":    {"path"},
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
	"planning":        {"summary", "title"},
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
	"config_census": {
		"globalClaudeMdTokens", "projectClaudeMdTokens",
		"skillListingTokens", "skillCount", "skills",
		"pluginListingTokens", "pluginCount", "plugins",
		"mcpServers", "mcpDeferred",
	},
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

// projectEvent strips a fully-built Event down to its source-free shape,
// in place. Default-deny: an unknown kind (or a non-map Data) keeps nothing.
// Envelope fields (id, sessionId, ts, kind, source, actor, provenance) are
// untouched; RawPayload — a raw-line preview that can carry source and is not
// covered by the signature — is always cleared.
func projectEvent(e *Event) {
	if e == nil {
		return
	}
	e.RawPayload = ""
	allowed := projectFieldAllowlist[e.Kind]
	data, ok := e.Data.(map[string]interface{})
	if !ok || len(allowed) == 0 {
		e.Data = map[string]interface{}{}
		return
	}
	elementAllowlists := projectArrayElementAllowlist[e.Kind]
	projected := make(map[string]interface{}, len(allowed))
	for _, key := range allowed {
		value, present := data[key]
		if !present || value == nil {
			continue
		}
		if elementFields, hasElementList := elementAllowlists[key]; hasElementList {
			projected[key] = projectArrayElements(value, elementFields)
			continue
		}
		projected[key] = value
	}
	if shellCommandKinds[e.Kind] {
		if cmd, isString := projected["command"].(string); isString {
			projected["command"] = scrubInlineCommand(cmd)
		}
	}
	e.Data = projected
}

// projectArrayElements projects each element of an array field down to its
// allowlisted keys. Non-array values and non-object elements yield nothing.
func projectArrayElements(value interface{}, fields []string) []interface{} {
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
			if v, present := obj[field]; present && v != nil {
				projected[field] = v
			}
		}
		out = append(out, projected)
	}
	return out
}
