package normalize

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
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

// codexRolloutProcessor converts codex rollout JSONL lines into canonical
// Events. It is stateful: function-call lines are correlated with their
// *_output lines by call_id, and the latest token usage is attached to the next
// final assistant message.
type CodexRolloutProcessor struct {
	sessionID      string
	pending        map[string]codexPendingCall
	lastTokenUsage map[string]interface{}
}

func NewCodexRolloutProcessor(sessionID string) *CodexRolloutProcessor {
	return &CodexRolloutProcessor{
		sessionID: sessionID,
		pending:   map[string]codexPendingCall{},
	}
}

// newCodexEvent builds a canonical Event stamped with the rollout line's own
// timestamp (so the replay timeline reflects when things actually happened, not
// when the watcher observed them) and source="codex". Actor is derived from
// kind: prompts are the candidate, session lifecycle is the system, and every
// tool/output event is the agent acting.
func (p *CodexRolloutProcessor) newCodexEvent(kind, ts string) event.Event {
	e := event.NewEvent(kind, p.sessionID)
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
	default:
		// turn_context and unknown wrappers carry no candidate-visible signal.
		return nil
	}
}

func (p *CodexRolloutProcessor) sessionMeta(payload map[string]interface{}, ts, raw string) []event.Event {
	e := p.newCodexEvent("session_start", ts)
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
		e := p.newCodexEvent("prompt", ts)
		e.Provenance = event.HumanProvenance()
		data := map[string]interface{}{"text": text}
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
		e := p.newCodexEvent("ai_response", ts)
		data := map[string]interface{}{
			"lastAssistantMessage": stringField(payload, "message"),
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
	var events []event.Event
	for path, rawChange := range changes {
		change, _ := rawChange.(map[string]interface{})
		if change == nil {
			continue
		}
		diff := stringField(change, "unified_diff")
		added, removed := countDiffLines(diff)
		e := p.newCodexEvent("file_diff", ts)
		e.Provenance = event.AIProvenance()
		data := map[string]interface{}{
			"path":         path,
			"diff":         diff,
			"linesAdded":   added,
			"linesRemoved": removed,
			"attribution":  "likely_ai",
			"changeType":   stringField(change, "type"),
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
		// apply_patch is reported via the richer event_msg/patch_apply_end; skip
		// the call line so we don't double-count file edits.
		if name == "apply_patch" {
			return nil
		}
		callID := stringField(payload, "call_id")
		args := parseCodexArgs(payload)
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
		output := stringField(payload, "output")
		return p.emitToolEvent(call, output, ts, raw)

	default:
		// message / reasoning context items duplicate event_msg signal — skip.
		return nil
	}
}

// emitToolEvent converts a completed tool call (call + output) into the right
// canonical event, branching on the codex tool name.
func (p *CodexRolloutProcessor) emitToolEvent(call codexPendingCall, output, ts, raw string) []event.Event {
	switch {
	case isCodexShellTool(call.name):
		cmd := codexCommandString(call.args)
		exitCode, stdout := parseCodexExecOutput(output)
		e := p.newCodexEvent("command", ts)
		e.Provenance = event.AIProvenance()
		e.Data = map[string]interface{}{
			"command":  cmd,
			"exitCode": exitCode,
			"stdout":   stdout,
		}
		e.RawPayload = raw
		return []event.Event{e}

	case call.name == "update_plan":
		e := p.newCodexEvent("planning", ts)
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
		e := p.newCodexEvent("mcp_call", ts)
		e.Data = map[string]interface{}{
			"tool":        call.name,
			"argsPreview": jsonPreview(call.args, 100),
		}
		e.RawPayload = raw
		return []event.Event{e}

	default:
		e := p.newCodexEvent("tool_use", ts)
		e.Data = map[string]interface{}{
			"toolName":     call.name,
			"inputPreview": jsonPreview(call.args, 100),
			"ok":           true,
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
		// Note: this is an OpenAI-priced estimate placeholder, not an
		// authoritative cost.
		data["reasoningTokens"] = intField(u, "reasoning_output_tokens")
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
		fmt.Sscanf(m[1], "%d", &exitCode)
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
