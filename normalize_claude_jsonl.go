package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Claude Code BYO-subscription capture works by tailing the per-session
// transcript JSONL that Claude Code writes to
// ~/.claude/projects/<munged-cwd>/<session-uuid>.jsonl. The transcript is the
// authoritative record — full assistant messages, tool calls + results, and
// per-API-request token usage with the exact model, which the backend prices
// into an ESTIMATED cost.
//
// Verified line shapes (Claude Code 2.1.x):
//
//	{"type":"user","message":{"role":"user","content":"..."},
//	 "uuid","parentUuid","timestamp","cwd","sessionId","isMeta?","isSidechain?",
//	 "promptId","promptSource","permissionMode",...}
//	{"type":"assistant","message":{"id","model","content":[{type:text|thinking|tool_use}],
//	 "usage":{input_tokens,cache_creation_input_tokens,cache_read_input_tokens,
//	          output_tokens,cache_creation:{ephemeral_5m_input_tokens,ephemeral_1h_input_tokens}}},
//	 "requestId","timestamp",...}
//	{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id",...}]},
//	 "toolUseResult": <the SAME structured object the PostToolUse hook receives>}
//
// plus non-message line types (mode, permission-mode, worktree-state,
// file-history-snapshot, attachment, last-prompt, ai-title, system, ...) that
// carry no candidate-visible signal but DO serve as assistant-message flush
// boundaries.
//
// One API response is split across MULTIPLE assistant lines (one per content
// block), all sharing message.id and carrying identical usage — so usage is
// deduped by message.id and text blocks are accumulated until a boundary.

// claudePendingTool holds a tool_use block awaiting its tool_result line so the
// pair can be fed through normalizePostToolUseByTool — the SAME function the
// hook path uses, which keeps event shapes identical across capture channels.
type claudePendingTool struct {
	name  string
	input map[string]interface{}
}

// claudeMsgAccum accumulates one assistant API message (one message.id) across
// its per-content-block transcript lines.
type claudeMsgAccum struct {
	msgID     string
	ts        string
	model     string
	requestID string
	text      strings.Builder
	usage     map[string]interface{}
	updatedAt time.Time // wall clock of last appended line, for stale flush
}

// claudeTranscriptProcessor converts Claude Code transcript JSONL lines into
// canonical Events. Stateful per transcript file: tool_use blocks are
// correlated with their tool_result lines by id, and assistant text/usage is
// accumulated per message.id.
type claudeTranscriptProcessor struct {
	sessionID string
	// usageOnly puts the processor in sidechain mode (subagents/agent-*.jsonl
	// files): every line is agent-authored, so only per-request token usage is
	// extracted — prompts, responses, and tool events must never enter the
	// candidate's timeline from a sidechain.
	usageOnly     bool
	pendingTools  map[string]claudePendingTool
	emittedMsgIDs map[string]bool
	accum         *claudeMsgAccum
	// lastPromptTs is the TRANSCRIPT timestamp of the previous human prompt,
	// retained only as lane state.
	lastPromptTs time.Time
	// Sidechain attribution (usageOnly mode). A sidechain file is one subagent
	// run, so these are constant per file: agentID comes from the filename
	// (agent-<id>.jsonl) or the rows' agentId field; attributionSkill /
	// attributionAgent are the NAMES of the skill / agent type that spawned the
	// sidechain, which Claude Code stamps directly on sidechain rows. Names
	// only — never skill/agent bodies.
	agentID          string
	attributionSkill string
	attributionAgent string
	// Lane identity of this transcript: one file IS one Claude Code process
	// (the filename is the session uuid), so the first record carrying
	// sessionId/cwd pins both for every event the file produces. Distinct
	// lanes = parallel sessions; the worker's parallelism signals key on
	// meta.ideSessionId / meta.cwd.
	ideSessionID string
	laneCwd      string
}

func newClaudeTranscriptProcessor(sessionID string) *claudeTranscriptProcessor {
	return &claudeTranscriptProcessor{
		sessionID:     sessionID,
		pendingTools:  map[string]claudePendingTool{},
		emittedMsgIDs: map[string]bool{},
	}
}

// transcriptHumanProvenance / transcriptAiProvenance mirror the hook
// equivalents but record the capture method, so the worker can tell which
// channel observed an event.
func transcriptHumanProvenance() *Provenance {
	return &Provenance{
		Attribution:   "likely_human",
		Confidence:    0.8,
		Observability: "medium",
		Methods:       []string{"transcript-jsonl"},
	}
}

func transcriptAiProvenance() *Provenance {
	return &Provenance{
		Attribution:   "likely_ai",
		Confidence:    0.9,
		Observability: "high",
		Methods:       []string{"transcript-jsonl"},
	}
}

func (p *claudeTranscriptProcessor) newTranscriptEvent(kind, ts string) Event {
	e := newEvent(kind, p.sessionID)
	e.Source = "claude-code"
	switch kind {
	case "prompt":
		e.Actor = humanActor()
		e.Provenance = transcriptHumanProvenance()
	default:
		e.Actor = aiActor()
	}
	if t := parseCodexTs(ts); t != "" {
		e.Ts = t
	}
	return e
}

// process parses one transcript line and returns zero or more canonical events.
// attachLane stamps the transcript's lane identity (meta.ideSessionId /
// meta.cwd) onto every outgoing event whose data is a map, never overwriting
// keys a normalizer already set.
func (p *claudeTranscriptProcessor) attachLane(events []Event) []Event {
	if p.ideSessionID == "" && p.laneCwd == "" {
		return events
	}
	for i := range events {
		data, ok := events[i].Data.(map[string]interface{})
		if !ok || data == nil {
			continue
		}
		meta, _ := data["meta"].(map[string]interface{})
		if meta == nil {
			meta = map[string]interface{}{}
		}
		if p.ideSessionID != "" {
			if _, exists := meta["ideSessionId"]; !exists {
				meta["ideSessionId"] = p.ideSessionID
			}
		}
		if p.laneCwd != "" {
			if _, exists := meta["cwd"]; !exists {
				meta["cwd"] = p.laneCwd
			}
		}
		data["meta"] = meta
	}
	return events
}

func (p *claudeTranscriptProcessor) process(line []byte) []Event {
	return p.attachLane(p.processLine(line))
}

func (p *claudeTranscriptProcessor) processLine(line []byte) []Event {
	var rec map[string]interface{}
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	if p.ideSessionID == "" {
		p.ideSessionID = stringField(rec, "sessionId")
	}
	if p.laneCwd == "" {
		p.laneCwd = stringField(rec, "cwd")
	}
	// Sidechain attribution hints are constant per file — pin the first value
	// each of them shows up with.
	if p.agentID == "" {
		p.agentID = stringField(rec, "agentId")
	}
	if p.attributionSkill == "" {
		p.attributionSkill = stringField(rec, "attributionSkill")
	}
	if p.attributionAgent == "" {
		p.attributionAgent = stringField(rec, "attributionAgent")
	}
	// Sidechain lines are subagent traffic: their "user" prompts are authored
	// by the AGENT, not the candidate, so they must never become prompt events
	// or pollute the main-context token series. Their token usage is real
	// spend though — extract it as subagent_usage so estimated cost doesn't
	// undercount subagent-heavy sessions. (Subagent transcripts live in
	// <session>/subagents/agent-*.jsonl, which the watcher tails in usageOnly
	// mode — the inline check is belt-and-braces for older layouts.)
	sidechain, _ := rec["isSidechain"].(bool)
	if p.usageOnly || sidechain {
		return p.sidechainUsage(rec)
	}

	typ, _ := rec["type"].(string)
	switch typ {
	case "assistant":
		return p.handleAssistant(rec, line)
	case "user":
		// A user line ends the in-flight assistant message (the API response
		// is complete before tool results / next prompts get written).
		events := p.flushAccum()
		return append(events, p.handleUser(rec, line)...)
	case "system":
		// System lines are flush boundaries; the compact_boundary subtype is
		// also the authoritative compaction marker, carrying whether Claude
		// Code hit the context wall (trigger=auto) or the user ran /compact
		// (trigger=manual) — the context-hygiene ground truth.
		events := p.flushAccum()
		if stringField(rec, "subtype") == "compact_boundary" {
			events = append(events, p.compactBoundaryEvent(rec))
		}
		return events
	default:
		// mode / worktree-state / file-history-snapshot / ... carry no signal
		// of their own but are reliable flush boundaries.
		return p.flushAccum()
	}
}

// compactBoundaryEvent converts a system/compact_boundary transcript line into
// a context_compact event. compactMetadata.trigger distinguishes auto-compact
// (context window exhausted) from a typed /compact.
func (p *claudeTranscriptProcessor) compactBoundaryEvent(rec map[string]interface{}) Event {
	ts, _ := rec["timestamp"].(string)
	e := p.newTranscriptEvent("context_compact", ts)
	e.Actor = systemActor()
	data := map[string]interface{}{}
	if cm, ok := rec["compactMetadata"].(map[string]interface{}); ok {
		if trigger := stringField(cm, "trigger"); trigger != "" {
			data["trigger"] = trigger
		}
	}
	e.Data = data
	e.RawPayload = strPreview(fmt.Sprintf("compact boundary trigger=%v", data["trigger"]), 100)
	return e
}

// sidechainUsage extracts ONLY token usage from a sidechain (subagent) line:
// one subagent_usage event per assistant message.id, carrying the request's
// usage + model and nothing else. No accumulation is needed — every line of a
// message repeats the same usage.
func (p *claudeTranscriptProcessor) sidechainUsage(rec map[string]interface{}) []Event {
	typ, _ := rec["type"].(string)
	if typ != "assistant" {
		return nil
	}
	msg, _ := rec["message"].(map[string]interface{})
	if msg == nil {
		return nil
	}
	msgID := stringField(msg, "id")
	if msgID == "" || p.emittedMsgIDs[msgID] {
		return nil
	}
	usage, _ := msg["usage"].(map[string]interface{})
	if usage == nil {
		return nil
	}
	p.emittedMsgIDs[msgID] = true

	ts, _ := rec["timestamp"].(string)
	e := p.newTranscriptEvent("subagent_usage", ts)
	e.Provenance = transcriptAiProvenance()
	data := map[string]interface{}{
		"usageScope":       "request",
		"sidechain":        true,
		"inputTokens":      intField(usage, "input_tokens"),
		"outputTokens":     intField(usage, "output_tokens"),
		"cacheReadTokens":  intField(usage, "cache_read_input_tokens"),
		"cacheWriteTokens": intField(usage, "cache_creation_input_tokens"),
	}
	if model := stringField(msg, "model"); model != "" {
		data["model"] = model
	}
	if reqID := stringField(rec, "requestId"); reqID != "" {
		data["requestId"] = reqID
	}
	if cc, ok := usage["cache_creation"].(map[string]interface{}); ok {
		data["cacheWrite5mTokens"] = intField(cc, "ephemeral_5m_input_tokens")
		data["cacheWrite1hTokens"] = intField(cc, "ephemeral_1h_input_tokens")
	}
	// Attribution: which skill/agent spawned this sidechain (names only) and
	// the sidechain's agent id, so per-skill / per-agent spend can be rolled up.
	if p.agentID != "" {
		data["agentId"] = p.agentID
	}
	if p.attributionSkill != "" {
		data["attributionSkill"] = p.attributionSkill
	}
	if p.attributionAgent != "" {
		data["attributionAgent"] = p.attributionAgent
	}
	e.Data = data
	e.RawPayload = strPreview(fmt.Sprintf("subagent usage msg=%s", msgID), 100)
	return []Event{e}
}

func (p *claudeTranscriptProcessor) handleAssistant(rec map[string]interface{}, line []byte) []Event {
	msg, _ := rec["message"].(map[string]interface{})
	if msg == nil {
		return p.flushAccum()
	}
	msgID := stringField(msg, "id")
	ts, _ := rec["timestamp"].(string)

	var events []Event
	if p.accum != nil && p.accum.msgID != msgID {
		events = p.flushAccum()
	}
	// Start an accumulator on the first line of a not-yet-emitted message.
	if p.accum == nil && msgID != "" && !p.emittedMsgIDs[msgID] {
		p.accum = &claudeMsgAccum{
			msgID:     msgID,
			ts:        ts,
			model:     stringField(msg, "model"),
			requestID: stringField(rec, "requestId"),
		}
		if u, ok := msg["usage"].(map[string]interface{}); ok {
			p.accum.usage = u
		}
	}
	if p.accum != nil && p.accum.msgID == msgID {
		p.accum.updatedAt = time.Now()
	}

	content, _ := msg["content"].([]interface{})
	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]interface{})
		if block == nil {
			continue
		}
		switch stringField(block, "type") {
		case "text":
			if p.accum != nil && p.accum.msgID == msgID {
				if p.accum.text.Len() > 0 {
					p.accum.text.WriteString("\n")
				}
				p.accum.text.WriteString(stringField(block, "text"))
			}
		case "tool_use":
			// Register even when the message was already flushed — the
			// tool_result may arrive on a later poll.
			id := stringField(block, "id")
			input, _ := block["input"].(map[string]interface{})
			if input == nil {
				input = map[string]interface{}{}
			}
			if id != "" {
				p.pendingTools[id] = claudePendingTool{name: stringField(block, "name"), input: input}
			}
		}
		// thinking / other block types carry no event of their own.
	}
	return events
}

// flushAccum emits the accumulated assistant message as ONE ai_response event
// carrying the request's token usage and model — the per-request usage series
// the worker needs for context-trend signals and estimated pricing.
func (p *claudeTranscriptProcessor) flushAccum() []Event {
	if p.accum == nil {
		return nil
	}
	a := p.accum
	p.accum = nil
	if a.msgID == "" || p.emittedMsgIDs[a.msgID] {
		return nil
	}
	p.emittedMsgIDs[a.msgID] = true

	e := p.newTranscriptEvent("ai_response", a.ts)
	e.Provenance = transcriptAiProvenance()
	data := map[string]interface{}{
		"lastAssistantMessage": a.text.String(),
		// The teams projector persists assistant text under `text` (it drops
		// unknown fields, so lastAssistantMessage alone never lands there).
		"text":       a.text.String(),
		"usageScope": "request",
	}
	if a.model != "" {
		data["model"] = a.model
	}
	if a.requestID != "" {
		data["requestId"] = a.requestID
	}
	if u := a.usage; u != nil {
		data["inputTokens"] = intField(u, "input_tokens")
		data["outputTokens"] = intField(u, "output_tokens")
		data["cacheReadTokens"] = intField(u, "cache_read_input_tokens")
		data["cacheWriteTokens"] = intField(u, "cache_creation_input_tokens")
		// 5m vs 1h cache writes price differently (1.25x vs 2x input) — pass
		// the split so the worker's estimate doesn't systematically undercount
		// (Claude Code defaults to the 1h tier).
		if cc, ok := u["cache_creation"].(map[string]interface{}); ok {
			data["cacheWrite5mTokens"] = intField(cc, "ephemeral_5m_input_tokens")
			data["cacheWrite1hTokens"] = intField(cc, "ephemeral_1h_input_tokens")
		}
	}
	e.Data = data
	e.RawPayload = strPreview(a.text.String(), 500)
	return []Event{e}
}

// flushStale force-flushes an accumulated assistant message that has not seen
// a new line in maxAge — covers the final message of a turn when Claude Code
// writes no further boundary line for a while.
func (p *claudeTranscriptProcessor) flushStale(maxAge time.Duration) []Event {
	if p.accum == nil || time.Since(p.accum.updatedAt) < maxAge {
		return nil
	}
	return p.attachLane(p.flushAccum())
}

func (p *claudeTranscriptProcessor) handleUser(rec map[string]interface{}, line []byte) []Event {
	msg, _ := rec["message"].(map[string]interface{})
	if msg == nil {
		return nil
	}
	// Meta lines (local-command caveats, command wrappers) and compact
	// summaries are Claude-Code-internal, not candidate prompts.
	if isMeta, _ := rec["isMeta"].(bool); isMeta {
		return nil
	}
	if isCompact, _ := rec["isCompactSummary"].(bool); isCompact {
		return nil
	}
	ts, _ := rec["timestamp"].(string)

	switch content := msg["content"].(type) {
	case string:
		return p.promptEvent(content, rec, ts, line)
	case []interface{}:
		var events []Event
		var textParts []string
		hasToolResult := false
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]interface{})
			if block == nil {
				continue
			}
			switch stringField(block, "type") {
			case "tool_result":
				hasToolResult = true
				if ev, ok := p.resolveToolResult(rec, block, ts, line); ok {
					events = append(events, ev)
				}
			case "text":
				textParts = append(textParts, stringField(block, "text"))
			}
		}
		// A content array with text blocks and no tool_result is a human
		// prompt (e.g. prompt with attachments).
		if !hasToolResult && len(textParts) > 0 {
			events = append(events, p.promptEvent(strings.Join(textParts, "\n"), rec, ts, line)...)
		}
		return events
	default:
		return nil
	}
}

func (p *claudeTranscriptProcessor) promptEvent(text string, rec map[string]interface{}, ts string, line []byte) []Event {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	// Local command OUTPUT is not an invocation — drop it.
	if strings.HasPrefix(trimmed, "<local-command") {
		return nil
	}
	// Slash-command envelopes (<command-name>/foo</command-name>…) are typed
	// invocations. They stay in the prompt stream with a `command` field — the
	// slash-command NAME only, never the expanded command body (which lives on
	// a separate isMeta line and never gets here). The backend keys reset
	// detection (/clear, /compact) and per-command spend attribution off this
	// field, so built-ins are emitted too.
	command := ""
	if strings.HasPrefix(trimmed, "<command-") {
		command = leadingCommandName(trimmed)
		if command == "" {
			// Malformed/unterminated envelope — not a usable invocation.
			return nil
		}
	}

	e := p.newTranscriptEvent("prompt", ts)
	data := map[string]interface{}{"text": text}
	if command != "" {
		data["command"] = command
	}

	meta := map[string]interface{}{}
	if v := stringField(rec, "sessionId"); v != "" {
		meta["ideSessionId"] = v
	}
	if v := stringField(rec, "permissionMode"); v != "" {
		meta["permissionMode"] = v
	}
	if v := stringField(rec, "promptSource"); v != "" {
		meta["promptSource"] = v
	}
	if v := stringField(rec, "promptId"); v != "" {
		meta["promptId"] = v
	}
	if v := stringField(rec, "cwd"); v != "" {
		meta["cwd"] = v
	}
	if len(meta) > 0 {
		data["meta"] = meta
	}

	// Track the previous human-prompt time as lane state only. No timing,
	// length, or paste signals are emitted — teams capture is content, not
	// behavioral analysis of the developer.
	if lineTime, err := time.Parse(time.RFC3339, ts); err == nil {
		p.lastPromptTs = lineTime
	}

	e.Data = data
	e.RawPayload = strPreview(string(line), 500)
	return []Event{e}
}

// leadingCommandName returns the slash-command name from a leading
// slash-command envelope (<command-name>/foo</command-name>…) — name only,
// without the leading slash, "" when the text doesn't start with an envelope
// or the name tag is missing/unterminated. It never returns the expanded
// command body (that lives in a separate isMeta line).
func leadingCommandName(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<command-") {
		return ""
	}
	return strings.TrimPrefix(extractTagContent(trimmed, "command-name"), "/")
}

// extractTagContent returns the trimmed text between <tag> and </tag>, or ""
// when the tag is absent or unterminated.
func extractTagContent(text, tag string) string {
	open := "<" + tag + ">"
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(text[start:], "</"+tag+">")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

// resolveToolResult pairs a tool_result block with its registered tool_use and
// feeds both through normalizePostToolUseByTool. The outer toolUseResult field
// carries the same structured response the PostToolUse hook receives
// (structuredPatch for edits, stdout/stderr for Bash, file for Read), so the
// resulting events are shape-identical to hook-captured ones.
func (p *claudeTranscriptProcessor) resolveToolResult(rec map[string]interface{}, block map[string]interface{}, ts string, line []byte) (Event, bool) {
	id := stringField(block, "tool_use_id")
	call, ok := p.pendingTools[id]
	if !ok {
		return Event{}, false
	}
	delete(p.pendingTools, id)

	isErr, _ := block["is_error"].(bool)
	toolResponse := map[string]interface{}{}
	switch tur := rec["toolUseResult"].(type) {
	case map[string]interface{}:
		toolResponse = tur
	case string:
		toolResponse["content"] = tur
		if isErr {
			toolResponse["error"] = tur
		}
	}
	// Transcript Bash results carry no structured exit_code. Failed runs DO
	// embed it in the result text ("Error: Exit code 2"), so parse that —
	// verification signals key off the real code — falling back to 1. Only
	// error results are scanned: a successful command's stdout may legitimately
	// TALK about exit codes, and success means 0 by definition (Claude Code
	// flags any nonzero exit as is_error).
	if _, has := toolResponse["exit_code"]; !has && isErr {
		blockText, _ := block["content"].(string)
		if code, ok := parseClaudeExitCode(toolResponse, blockText); ok {
			toolResponse["exit_code"] = float64(code)
		} else {
			toolResponse["exit_code"] = float64(1)
		}
	}

	ev, ok := normalizePostToolUseByTool(call.name, call.input, toolResponse, p.sessionID, strPreview(string(line), 500))
	if !ok {
		return Event{}, false
	}
	ev.Source = "claude-code"
	if t := parseCodexTs(ts); t != "" {
		ev.Ts = t
	}
	if ev.Actor == nil {
		ev.Actor = aiActor()
	}
	// Stamp the capture method while preserving the attribution the
	// normalizer chose (plan_decision is likely_human, edits likely_ai).
	if ev.Provenance != nil {
		ev.Provenance.Methods = []string{"transcript-jsonl"}
	}
	return ev, true
}

// parseClaudeExitCode scans the textual fields of a transcript tool result for
// an embedded exit code ("Error: Exit code 2", "exited with code 130", ...).
// Reuses the codex exit-code pattern — both tools phrase it the same way.
func parseClaudeExitCode(toolResponse map[string]interface{}, extra string) (int, bool) {
	candidates := []string{extra}
	for _, key := range []string{"content", "error", "stderr", "stdout"} {
		if s, ok := toolResponse[key].(string); ok && s != "" {
			candidates = append(candidates, s)
		}
	}
	for _, s := range candidates {
		if m := codexExitCodeRe.FindStringSubmatch(s); m != nil {
			code := 0
			if _, err := fmt.Sscanf(m[1], "%d", &code); err == nil {
				return code, true
			}
		}
	}
	return 0, false
}
