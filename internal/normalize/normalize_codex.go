package normalize

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Codex instrumentation works by tailing the per-session rollout JSONL that the
// `codex` CLI writes to ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl. Unlike
// Claude Code / Cursor, the codex hooks engine does NOT fire for `codex exec`
// (and is interactive-TUI-gated besides), so the rollout file is the reliable
// capture channel — it is written in every mode and carries prompts, tool
// calls, command output, file patches, assistant messages and token usage.
//
// Each rollout line is one RolloutItem:
//
//	{"timestamp":"...","type":"session_meta|event_msg|response_item|turn_context","payload":{...}}
//
// The payload's own "type" discriminates further (user_message, agent_message,
// function_call, custom_tool_call, patch_apply_end, token_count, ...).

// codexPendingCall holds a tool call awaiting its output line so the two can be
// merged into a single canonical event (mirrors how Claude's PostToolUse
// carries both input and response).
type codexPendingCall struct {
	name string
	args map[string]interface{}
}

type codexRunningCall struct {
	call   codexPendingCall
	callID string
}

// codexRolloutProcessor converts codex rollout JSONL lines into canonical
// Events. It is stateful: function-call lines are correlated with their
// *_output lines by call_id, and the latest token usage is attached to the next
// final assistant message.
type CodexRolloutProcessor struct {
	sessionID      string
	pending        map[string]codexPendingCall
	running        map[string]codexRunningCall
	lastTokenUsage map[string]interface{}
	// workdir is the session's cwd, home-collapsed to "~/…", captured from the
	// session_meta header (the ONLY codex rollout line that carries cwd). It is
	// stamped onto each prompt event so the teams dashboard can show where the
	// session ran; the raw absolute cwd is never emitted on prompts.
	workdir string
	// model is the per-turn model id captured from the LATEST turn_context line
	// (session_meta carries only model_provider — the vendor, e.g. "openai"). It is
	// stamped onto each ai_response so the backend can price the turn against the
	// real model. turn_context precedes the turn's agent messages, so the captured
	// value is always the model in force for the response it lands on.
	model string
	// RepoRoot is the canonical per-session repository identity (a git remote slug
	// owner/name, or a stable opaque hash for a no-remote/non-git dir). Unlike
	// workdir — which normalize derives itself from the payload cwd via
	// HomeRelativeStrict — repoRoot needs `git config` (fs/exec) and so CANNOT be
	// derived here; it is resolved ONCE per session in internal/capture
	// (capture.sessionRepoRoot) and threaded in as session state, then stamped onto
	// each prompt event beside workdir when non-empty. Empty (omitted) when the cwd
	// was gone/unresolvable, mirroring workdir's empty-on-failure.
	RepoRoot string
	// RepoHost is the remote's bare hostname (e.g. "github.com"), resolved and
	// threaded exactly like RepoRoot. The slug alone cannot tell providers apart —
	// gitlab.com/acme/api and github.com/acme/api both reduce to "acme/api" — so
	// the backend needs the host to require a real provider match instead of
	// treating a colliding owner name as one. Non-empty ONLY when RepoRoot is a
	// remote slug; omitted when empty.
	RepoHost string
}

func NewCodexRolloutProcessor(sessionID string) *CodexRolloutProcessor {
	return &CodexRolloutProcessor{
		sessionID: sessionID,
		pending:   map[string]codexPendingCall{},
		running:   map[string]codexRunningCall{},
	}
}

// stableEventID derives a deterministic event id from a STABLE per-source key
// scoped by session and kind, so the same rollout record always yields the same
// id no matter how often it is re-observed. Codex resume/fork writes a NEW
// rollout file that copies prior history verbatim, and the watcher runs one
// processor (its own dedup state) per file — so without this a copied line
// re-emits as a byte-identical duplicate carrying a fresh random id the backend
// can't collapse. The stable key is the record's own identity: the session id
// for session_start, the tool call_id for command/tool/mcp/plan/file_diff, and
// the rollout line timestamp for the event_msg-derived prompt/ai_response (Codex
// event_msg lines carry no per-item id — the line timestamp is copied verbatim
// on fork, so it is the record's stable identity). When sourceKey is empty it
// falls back to a random id — never worse than the previous always-random
// behavior. Mirrors ClaudeTranscriptProcessor.stableEventID.
func (p *CodexRolloutProcessor) stableEventID(sourceKey, kind string) string {
	if sourceKey == "" {
		return event.NewUUID()
	}
	return event.DeterministicUUID(p.sessionID + "\x1f" + kind + "\x1f" + sourceKey)
}

// newCodexEvent builds a canonical Event stamped with the rollout line's own
// timestamp (so the replay timeline reflects when things actually happened, not
// when the watcher observed them) and source="codex". Actor is derived from
// kind: prompts are the candidate, session lifecycle is the system, and every
// tool/output event is the agent acting. sourceKey is the stable identity of the
// source record (session id / tool call_id / rollout line ts); pass "" only when
// no stable key exists.
func (p *CodexRolloutProcessor) newCodexEvent(kind, ts, sourceKey string) event.Event {
	e := event.NewEvent(kind, p.sessionID)
	// Overwrite NewEvent's random id with the stable, source-derived one (keeping
	// NewEvent as the single source of Ts/Source/V defaults).
	e.ID = p.stableEventID(sourceKey, kind)
	e.Source = "codex"
	switch kind {
	case "prompt":
		e.Actor = event.HumanActor()
	case "session_start", "session_end":
		e.Actor = event.SystemActor()
	default:
		e.Actor = event.AIActor()
	}
	if t := parseCodexTs(ts); t != "" {
		e.Ts = t
	}
	return e
}

// process parses one rollout line and returns zero or more canonical events.
func (p *CodexRolloutProcessor) Process(line []byte) []event.Event {
	var rec map[string]interface{}
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	typ, _ := rec["type"].(string)
	payload, _ := rec["payload"].(map[string]interface{})
	if payload == nil {
		return nil
	}
	ts, _ := rec["timestamp"].(string)
	raw := strPreview(string(line), 500)

	switch typ {
	case "session_meta":
		return p.sessionMeta(payload, ts, raw)
	case "event_msg":
		return p.eventMsg(payload, ts, raw)
	case "response_item":
		return p.responseItem(payload, ts, raw)
	case "turn_context":
		// turn_context carries the per-turn model (the only rollout line that does);
		// stash it for the turn's ai_response. Assigned UNCONDITIONALLY: a
		// turn_context that declares no model clears any prior value rather than
		// carrying it forward, so the next ai_response omits model instead of pricing
		// against a stale one (never attribute a model we do not currently know). A
		// turn with no turn_context at all leaves the last value untouched — the model
		// genuinely persists until the next turn_context changes it. Emits no event.
		p.model = stringField(payload, "model")
		return nil
	default:
		// Unknown wrappers carry no candidate-visible signal.
		return nil
	}
}

func (p *CodexRolloutProcessor) sessionMeta(payload map[string]interface{}, ts, raw string) []event.Event {
	// The session id is stable per rollout (and identical in a forked copy).
	// The rollout filename normally supplies it before the first line is read;
	// fall back to session_meta here if that path did not parse, so events are
	// never stamped "unknown" and pooled into a shared cross-session chain.
	if p.sessionID == "" {
		p.sessionID = stringField(payload, "id")
	}
	// Stash the home-collapsed cwd for prompt events: session_meta is the only
	// rollout line carrying cwd, and it precedes every prompt. HomeRelativeStrict
	// emits ONLY a provably home-relative ("~"-prefixed) value — an outside-home
	// cwd or a home-lookup failure yields "", so the prompt omits workdir rather
	// than leaking an absolute path that may carry the OS username.
	p.workdir = state.HomeRelativeStrict(stringField(payload, "cwd"))
	e := p.newCodexEvent("session_start", ts, stringField(payload, "id"))
	data := map[string]interface{}{
		"ideSessionId": stringField(payload, "id"),
		"cwd":          stringField(payload, "cwd"),
		"source":       stringField(payload, "originator"),
		"cliVersion":   stringField(payload, "cli_version"),
		"model":        stringField(payload, "model_provider"),
	}
	e.Data = data
	e.RawPayload = raw
	return []event.Event{e}
}

func (p *CodexRolloutProcessor) eventMsg(payload map[string]interface{}, ts, raw string) []event.Event {
	switch stringField(payload, "type") {
	case "user_message":
		text := stringField(payload, "message")
		// event_msg lines carry no per-item id; the rollout line ts is the
		// record's stable identity (copied verbatim on resume/fork).
		e := p.newCodexEvent("prompt", ts, ts)
		e.Provenance = event.HumanProvenance()
		data := map[string]interface{}{"text": text}
		// workdir rides its own allowlisted key (never raw cwd); set only when the
		// session_meta header supplied one.
		if p.workdir != "" {
			data["workdir"] = p.workdir
		}
		// repoRoot — the canonical repo identity resolved in capture and threaded in;
		// stamped only when non-empty (mirrors workdir). It de-fragments the repo
		// across subdirs/worktrees and joins exactly to outcome_events.repo.
		if p.RepoRoot != "" {
			data["repoRoot"] = p.RepoRoot
		}
		// repoHost — the provider the slug came from, so the backend can tell
		// gitlab.com/acme/api apart from github.com/acme/api. Same omit-when-empty
		// rule; empty is the honest answer for a repo with no remote.
		if p.RepoHost != "" {
			data["repoHost"] = p.RepoHost
		}
		e.Data = data
		e.RawPayload = raw
		saveLastPromptTs()
		return []event.Event{e}

	case "agent_message":
		// Codex emits multiple agent_message lines per turn: "commentary" (interim
		// narration) and "final_answer". Only the final answer is the turn-end
		// assistant message analogous to Claude's Stop.
		if stringField(payload, "phase") != "final_answer" {
			return nil
		}
		// Same as the prompt: no per-item id on the event_msg line, so key off
		// the (fork-stable) rollout line ts.
		e := p.newCodexEvent("ai_response", ts, ts)
		data := map[string]interface{}{
			"lastAssistantMessage": stringField(payload, "message"),
		}
		if p.model != "" {
			data["model"] = p.model
		}
		p.attachTokenUsage(data)
		if last := loadLastPromptTs(); !last.IsZero() {
			data["turnDurationMs"] = time.Since(last).Milliseconds()
		}
		e.Data = data
		e.RawPayload = raw
		return []event.Event{e}

	case "patch_apply_end":
		return p.patchApplyEnd(payload, ts, raw)

	case "token_count":
		// Stash the latest usage; attached to the next final assistant message.
		if info, ok := payload["info"].(map[string]interface{}); ok {
			if usage, ok := info["total_token_usage"].(map[string]interface{}); ok {
				p.lastTokenUsage = usage
			}
		}
		return nil

	default:
		return nil
	}
}

// patchApplyEnd emits one file_diff per changed file. The payload carries a
// ready-made unified_diff per path, plus the change type (add/update/delete),
// so no apply-patch envelope parsing is needed.
func (p *CodexRolloutProcessor) patchApplyEnd(payload map[string]interface{}, ts, raw string) []event.Event {
	changes, ok := payload["changes"].(map[string]interface{})
	if !ok || len(changes) == 0 {
		return nil
	}
	// call_id is the apply_patch call's stable identity; one patch_apply_end
	// emits one file_diff per path, so scope the key by path too.
	callID := stringField(payload, "call_id")
	var events []event.Event
	for path, rawChange := range changes {
		change, _ := rawChange.(map[string]interface{})
		if change == nil {
			continue
		}
		diff := stringField(change, "unified_diff")
		added, removed := countDiffLines(diff)
		e := p.newCodexEvent("file_diff", ts, callID+"\x1f"+path)
		e.Provenance = event.AIProvenance()
		data := map[string]interface{}{
			"path":         path,
			"diff":         diff,
			"linesAdded":   added,
			"linesRemoved": removed,
			"attribution":  "likely_ai",
			"changeType":   stringField(change, "type"),
		}
		if ranges := lineRangesFromUnifiedDiff(diff, e.Provenance.Attribution); len(ranges) > 0 {
			data["lineRanges"] = ranges
		}
		if mv := stringField(change, "move_path"); mv != "" {
			data["movePath"] = mv
		}
		e.Data = data
		e.RawPayload = strPreview(diff, 500)
		events = append(events, e)
	}
	return events
}

func (p *CodexRolloutProcessor) responseItem(payload map[string]interface{}, ts, raw string) []event.Event {
	switch stringField(payload, "type") {
	case "function_call", "custom_tool_call":
		name := stringField(payload, "name")
		args := parseCodexArgs(payload)
		// Newer Codex hosts expose one generic custom tool named `exec`. Its input
		// is a small JavaScript orchestration program which calls the real tool
		// (`tools.exec_command`, `tools.update_plan`, `tools.apply_patch`, ...).
		// Unwrap the tool identity here so shell/planning/edit telemetry does not
		// collapse into an empty generic tool_use event. Direct codex-cli calls are
		// left unchanged for backwards compatibility.
		if name == "exec" {
			name, args = unwrapCodexExec(args)
		}
		// apply_patch is reported via the richer event_msg/patch_apply_end; skip
		// the call line so we don't double-count file edits.
		if name == "apply_patch" {
			return nil
		}
		callID := stringField(payload, "call_id")
		if callID != "" {
			p.pending[callID] = codexPendingCall{name: name, args: args}
		}
		return nil

	case "function_call_output", "custom_tool_call_output":
		callID := stringField(payload, "call_id")
		call, ok := p.pending[callID]
		if !ok {
			return nil
		}
		delete(p.pending, callID)
		output := codexOutputText(payload["output"])
		// Current hosts detach long-running exec_command calls and complete them
		// through one or more write_stdin calls. Hold the original command until
		// completion so we never report the handoff as a false exitCode=0, and use
		// the original call id so replay remains deterministic.
		if isCodexShellTool(call.name) {
			if cellID := codexRunningCellID(output); cellID != "" {
				p.running[cellID] = codexRunningCall{call: call, callID: callID}
				return nil
			}
		}
		if call.name == "write_stdin" {
			cellID := stringField(call.args, "session_id")
			if running, ok := p.running[cellID]; ok {
				if nextID := codexRunningCellID(output); nextID != "" {
					if nextID != cellID {
						delete(p.running, cellID)
						p.running[nextID] = running
					}
					return nil
				}
				delete(p.running, cellID)
				return p.emitToolEvent(running.call, running.callID, output, ts, raw)
			}
		}
		return p.emitToolEvent(call, callID, output, ts, raw)

	default:
		// message / reasoning context items duplicate event_msg signal — skip.
		return nil
	}
}

// emitToolEvent converts a completed tool call (call + output) into the right
// canonical event, branching on the codex tool name. callID is the tool call's
// stable identity (the OpenAI call_id, copied verbatim on resume/fork), used to
// derive a deterministic event id — mirrors how the Claude path keys off
// tool_use_id.
func (p *CodexRolloutProcessor) emitToolEvent(call codexPendingCall, callID, output, ts, raw string) []event.Event {
	switch {
	case call.name == "wrapped_apply_patch":
		return p.emitWrappedPatch(call, callID, ts, raw)

	case isCodexShellTool(call.name):
		cmd := codexCommandString(call.args)
		// The backend's canonical command schema requires a non-empty invocation.
		// Current exec wrappers occasionally build arguments indirectly, where the
		// narrow non-evaluating extractor cannot recover cmd safely. Preserve the
		// tool occurrence without fabricating an empty command or retaining wrapper
		// source; a future parser can widen this only with a pinned safe shape.
		if strings.TrimSpace(cmd) == "" {
			e := p.newCodexEvent("tool_use", ts, callID)
			e.Data = map[string]interface{}{
				"tool":   call.name,
				"status": codexToolStatus(output),
			}
			e.RawPayload = raw
			return []event.Event{e}
		}
		exitCode, stdout := parseCodexExecOutput(output)
		e := p.newCodexEvent("command", ts, callID)
		e.Provenance = event.AIProvenance()
		e.Data = map[string]interface{}{
			"command":  cmd,
			"exitCode": exitCode,
			"stdout":   stdout,
		}
		e.RawPayload = raw
		return []event.Event{e}

	case call.name == "update_plan":
		e := p.newCodexEvent("planning", ts, callID)
		data := map[string]interface{}{}
		// Codex carries the plan steps under "plan" (array of {step,status}).
		if plan, ok := call.args["plan"]; ok {
			data["todos"] = plan
		} else if steps, ok := call.args["steps"]; ok {
			data["todos"] = steps
		}
		e.Data = data
		e.RawPayload = raw
		return []event.Event{e}

	case isCodexMCPTool(call.name):
		e := p.newCodexEvent("mcp_call", ts, callID)
		e.Data = map[string]interface{}{
			"tool":        call.name,
			"argsPreview": jsonPreview(call.args, 100),
		}
		e.RawPayload = raw
		return []event.Event{e}

	default:
		e := p.newCodexEvent("tool_use", ts, callID)
		e.Data = map[string]interface{}{
			"tool":   call.name,
			"status": codexToolStatus(output),
		}
		e.RawPayload = raw
		return []event.Event{e}
	}
}

func (p *CodexRolloutProcessor) attachTokenUsage(data map[string]interface{}) {
	u := p.lastTokenUsage
	if u == nil {
		return
	}
	input := intField(u, "input_tokens")
	output := intField(u, "output_tokens")
	cacheRead := intField(u, "cached_input_tokens")
	if input > 0 || output > 0 {
		data["inputTokens"] = input
		data["outputTokens"] = output
		data["cacheReadTokens"] = cacheRead
		// reasoning_output_tokens is OpenAI-only and absent on non-reasoning
		// turns. Attach reasoningTokens ONLY when the provider actually reported
		// it — emitting 0 for an unreported count would conflate "no reasoning
		// data" with "reported zero," the same fabrication the attribution
		// buckets refuse. Absent-by-omission is the honest signal.
		if _, ok := u["reasoning_output_tokens"].(float64); ok {
			data["reasoningTokens"] = intField(u, "reasoning_output_tokens")
		}
	}
}

// --- helpers ---------------------------------------------------------------

func isCodexShellTool(name string) bool {
	switch name {
	case "exec_command", "shell", "local_shell", "local_shell_call", "container.exec", "unified_exec":
		return true
	}
	return false
}

// unwrapCodexExec recovers the real tool identity from the JavaScript wrapper
// used by current Codex hosts. Only narrowly-recognized tool calls are lifted;
// unknown programs remain generic `exec` tool_use events. The wrapper source is
// never emitted. For shell commands we recover only the cmd string (which the
// source-exclusion projector subsequently applies its inline-code scrub to).
func unwrapCodexExec(args map[string]interface{}) (string, map[string]interface{}) {
	input := stringField(args, "input")
	switch {
	case strings.Contains(input, "tools.exec_command("):
		out := map[string]interface{}{}
		if cmd := extractJSObjectStringField(input, "cmd"); cmd != "" {
			out["cmd"] = cmd
		}
		return "exec_command", out
	case strings.Contains(input, "tools.update_plan("):
		return "update_plan", map[string]interface{}{}
	case strings.Contains(input, "tools.apply_patch("):
		out := map[string]interface{}{}
		if patch := extractJSCallStringArg(input, "tools.apply_patch"); patch != "" {
			out["patch"] = patch
		}
		// Keep this distinct from a direct apply_patch call: direct codex-cli also
		// emits patch_apply_end and is skipped above, while the wrapper has no such
		// companion record and must derive content-free file metadata itself.
		return "wrapped_apply_patch", out
	case strings.Contains(input, "tools.write_stdin("):
		out := map[string]interface{}{}
		if id := extractJSNumericField(input, "session_id"); id != "" {
			out["session_id"] = id
		}
		return "write_stdin", out
	default:
		return "exec", map[string]interface{}{}
	}
}

var jsCmdFieldRe = regexp.MustCompile(`(?s)\bcmd\s*:\s*("(?:\\.|[^"\\])*")`)

func extractJSNumericField(input, field string) string {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(field) + `\s*:\s*(\d+)`)
	m := re.FindStringSubmatch(input)
	if m == nil {
		return ""
	}
	return m[1]
}

func extractJSObjectStringField(input, field string) string {
	if field != "cmd" { // keep the parser deliberately narrow
		return ""
	}
	m := jsCmdFieldRe.FindStringSubmatch(input)
	if m == nil {
		return ""
	}
	s, err := strconv.Unquote(m[1])
	if err != nil {
		return ""
	}
	return s
}

// extractJSCallStringArg extracts a JSON-style double-quoted first argument.
// Codex serializes the wrapper this way today. Refuse other JavaScript syntax
// rather than attempting to evaluate it or accidentally retaining source.
func extractJSCallStringArg(input, callee string) string {
	start := strings.Index(input, callee+"(")
	if start < 0 {
		return ""
	}
	rest := strings.TrimSpace(input[start+len(callee)+1:])
	if rest == "" {
		return ""
	}
	if rest[0] != '"' {
		// functions.exec commonly assigns a large patch to a local first:
		//   const patch = "..."; await tools.apply_patch(patch)
		// Resolve only a plain identifier bound to a double-quoted literal. Never
		// evaluate expressions or template strings.
		end := 0
		for end < len(rest) && ((rest[end] >= 'a' && rest[end] <= 'z') ||
			(rest[end] >= 'A' && rest[end] <= 'Z') ||
			(rest[end] >= '0' && rest[end] <= '9') || rest[end] == '_') {
			end++
		}
		if end == 0 {
			return ""
		}
		name := rest[:end]
		for _, decl := range []string{"const ", "let ", "var "} {
			assign := decl + name
			idx := strings.Index(input, assign)
			if idx < 0 {
				continue
			}
			value := strings.TrimSpace(input[idx+len(assign):])
			if !strings.HasPrefix(value, "=") {
				continue
			}
			rest = strings.TrimSpace(strings.TrimPrefix(value, "="))
			break
		}
	}
	if rest == "" || rest[0] != '"' {
		return ""
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] != '"' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && rest[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			continue
		}
		s, err := strconv.Unquote(rest[:i+1])
		if err == nil {
			return s
		}
		return ""
	}
	return ""
}

// codexOutputText accepts both the legacy scalar output and the current
// Responses-style content array: [{type:"input_text", text:"..."}, ...].
func codexOutputText(v interface{}) string {
	switch out := v.(type) {
	case string:
		return out
	case []interface{}:
		parts := make([]string, 0, len(out))
		for _, item := range out {
			m, _ := item.(map[string]interface{})
			if text := stringField(m, "text"); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func codexToolStatus(output string) string {
	if strings.Contains(strings.ToLower(output), "script failed") {
		return "failed"
	}
	return "completed"
}

var codexRunningCellRe = regexp.MustCompile(`(?m)^Script running with cell ID ([0-9]+)\s*$`)

func codexRunningCellID(output string) string {
	m := codexRunningCellRe.FindStringSubmatch(output)
	if m == nil {
		return ""
	}
	return m[1]
}

// emitWrappedPatch reduces an apply_patch envelope to paths and line counts.
// Patch bodies stay only in transient normalizer memory and are never attached
// to the event; the normal source-exclusion pass remains a second backstop.
func (p *CodexRolloutProcessor) emitWrappedPatch(call codexPendingCall, callID, ts, raw string) []event.Event {
	patch := stringField(call.args, "patch")
	if patch == "" {
		return nil
	}
	type change struct {
		path           string
		added, removed int
	}
	var changes []change
	current := -1
	for _, line := range strings.Split(patch, "\n") {
		var path string
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: "} {
			if strings.HasPrefix(line, prefix) {
				path = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				break
			}
		}
		if path != "" {
			changes = append(changes, change{path: path})
			current = len(changes) - 1
			continue
		}
		if current < 0 || strings.HasPrefix(line, "*** ") || strings.HasPrefix(line, "@@") {
			continue
		}
		if strings.HasPrefix(line, "+") {
			changes[current].added++
		} else if strings.HasPrefix(line, "-") {
			changes[current].removed++
		}
	}
	events := make([]event.Event, 0, len(changes))
	for _, c := range changes {
		e := p.newCodexEvent("file_diff", ts, callID+"\x1f"+c.path)
		e.Provenance = event.AIProvenance()
		e.Data = map[string]interface{}{
			"path":         c.path,
			"linesAdded":   c.added,
			"linesRemoved": c.removed,
		}
		// RawPayload deliberately excludes the wrapper/patch source.
		e.RawPayload = "wrapped apply_patch"
		events = append(events, e)
	}
	return events
}

func isCodexMCPTool(name string) bool {
	// Codex namespaces MCP tools (e.g. "server__tool" or "mcp__server__tool").
	return strings.Contains(name, "__")
}

// codexCommandString extracts a human-readable command from codex tool args,
// which may be {"cmd":"..."}, {"command":"..."} or {"command":["bash","-lc","..."]}.
func codexCommandString(args map[string]interface{}) string {
	if s := stringField(args, "cmd"); s != "" {
		return s
	}
	switch v := args["command"].(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// parseCodexArgs decodes a function_call's "arguments" field, which codex sends
// as a JSON-encoded string. custom_tool_call carries a plain "input" string.
func parseCodexArgs(payload map[string]interface{}) map[string]interface{} {
	if s, ok := payload["arguments"].(string); ok && s != "" {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
	}
	if m, ok := payload["arguments"].(map[string]interface{}); ok {
		return m
	}
	if s, ok := payload["input"].(string); ok && s != "" {
		return map[string]interface{}{"input": s}
	}
	return map[string]interface{}{}
}

var codexExitCodeRe = regexp.MustCompile(`(?i)(?:exited with code|exit code:?)\s*(\d+)`)

// parseCodexExecOutput pulls an exit code and the trailing stdout out of codex's
// exec output blob, which looks like:
//
//	Chunk ID: ...\nWall time: ...\nProcess exited with code 0\nOriginal token count: 2\nOutput:\n<stdout>
func parseCodexExecOutput(output string) (int, string) {
	exitCode := 0
	if m := codexExitCodeRe.FindStringSubmatch(output); m != nil {
		_, _ = fmt.Sscanf(m[1], "%d", &exitCode) // regex guarantees digits; exitCode stays 0 otherwise.
	}
	stdout := output
	if idx := strings.Index(output, "Output:\n"); idx >= 0 {
		stdout = output[idx+len("Output:\n"):]
	}
	return exitCode, stdout
}

// countDiffLines counts added/removed lines in a unified diff, excluding the
// ---/+++ file headers.
func countDiffLines(diff string) (added, removed int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return
}

// unifiedHunkHeaderRe captures the new-file side of a `@@ -a,b +c,d @@` hunk
// header: group 1 = new start (c), group 2 = new length (d, optional — a
// missing count means 1 per the unified-diff spec).
var unifiedHunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// lineRangesFromUnifiedDiff derives the new-file-side line span of each hunk in
// a unified diff as content-free {start,end,attribution} triples. Codex has no
// structured hunks, so this scans ONLY the `@@` header lines — never any
// `+`/`-` body content — mirroring the Claude structuredPatch path.
//
// Shape/attribution and the pure-deletion (new length 0) skip match
// lineRangesFromStructuredPatch; see its doc comment.
func lineRangesFromUnifiedDiff(diff, attribution string) []interface{} {
	var ranges []interface{}
	for _, line := range strings.Split(diff, "\n") {
		m := unifiedHunkHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start := 0
		_, _ = fmt.Sscanf(m[1], "%d", &start) // regex guarantees digits.
		count := 1
		if m[2] != "" {
			_, _ = fmt.Sscanf(m[2], "%d", &count)
		}
		if count == 0 {
			continue
		}
		ranges = append(ranges, map[string]interface{}{
			"start":       start,
			"end":         start + count - 1,
			"attribution": attribution,
		})
	}
	return ranges
}

// parseCodexTs normalizes a rollout timestamp ("2026-06-06T20:38:45.965Z") to
// RFC3339Nano. Returns "" if it can't be parsed (caller keeps the default).
func parseCodexTs(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func stringField(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

func intField(m map[string]interface{}, key string) int64 {
	if f, ok := m[key].(float64); ok {
		return int64(f)
	}
	return 0
}
