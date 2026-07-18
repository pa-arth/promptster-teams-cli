package normalize

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
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
type ClaudeTranscriptProcessor struct {
	sessionID string
	// UsageOnly puts the processor in sidechain mode (subagents/agent-*.jsonl
	// files): every line is agent-authored, so only per-request token usage is
	// extracted — prompts, responses, and tool events must never enter the
	// candidate's timeline from a sidechain.
	UsageOnly     bool
	pendingTools  map[string]claudePendingTool
	emittedMsgIDs map[string]bool
	accum         *claudeMsgAccum
	// lastPromptTs is the TRANSCRIPT timestamp of the previous human prompt,
	// retained only as lane state.
	lastPromptTs time.Time
	// Sidechain attribution (UsageOnly mode). A sidechain file is one subagent
	// run, so these are constant per file: AgentID comes from the filename
	// (agent-<id>.jsonl) or the rows' agentId field; attributionSkill /
	// attributionAgent are the NAMES of the skill / agent type that spawned the
	// sidechain, which Claude Code stamps directly on sidechain rows. Names
	// only — never skill/agent bodies.
	AgentID          string
	attributionSkill string
	attributionAgent string
	// Interrupt tracking. Claude Code writes a synthetic user line
	// ([Request interrupted by user] / ...for tool use) when the developer hits
	// ESC/Ctrl+C mid-response. We classify what was cut POSITIONALLY from the
	// most-recent assistant RECORD — interruptedMessageId is null ~1/3 of the
	// time, so it can't be relied on. lastAssistantHadTool/lastCutToolName are
	// re-derived from each assistant record's own content (NOT sticky across
	// records sharing a message.id), so a text-only record following a tool_use
	// record for the same message clears the cut-tool state.
	//
	// SOURCE EXCLUSION: only the tool NAME is tracked here (same class as a
	// slash-command name), never the tool INPUT. A Bash command body or file
	// path is source-adjacent, so — unlike the hiring CLI — no cutToolInput is
	// derived, kept, or emitted. If in doubt it stays out.
	lastAssistantHadTool bool
	lastCutToolName      string
	// pendingInterruptID is the ID of an emitted interrupt awaiting the redirect
	// prompt that follows it. The next prompt (with no assistant record between)
	// back-links to it; an intervening assistant record or a second interrupt
	// clears it.
	pendingInterruptID string
}

func NewClaudeTranscriptProcessor(sessionID string) *ClaudeTranscriptProcessor {
	return &ClaudeTranscriptProcessor{
		sessionID:     sessionID,
		pendingTools:  map[string]claudePendingTool{},
		emittedMsgIDs: map[string]bool{},
	}
}

// transcriptHumanProvenance / transcriptAiProvenance mirror the hook
// equivalents but record the capture method, so the worker can tell which
// channel observed an event.
func transcriptHumanProvenance() *event.Provenance {
	return &event.Provenance{
		Attribution:   "likely_human",
		Confidence:    0.8,
		Observability: "medium",
		Methods:       []string{"transcript-jsonl"},
	}
}

func transcriptAiProvenance() *event.Provenance {
	return &event.Provenance{
		Attribution:   "likely_ai",
		Confidence:    0.9,
		Observability: "high",
		Methods:       []string{"transcript-jsonl"},
	}
}

// stableEventID derives a deterministic event id from a STABLE per-source key
// (a transcript line's own `uuid`, an assistant message.id, or a tool_use_id)
// scoped by session and kind. The same logical source therefore always yields
// the same id — so a line re-read after an offset wobble, re-tailed from a
// resumed/forked transcript that copied prior history, or an event resent after
// a transport error all collapse to ONE row on the backend instead of landing
// as byte-identical duplicates with fresh random ids. When sourceKey is empty
// (a malformed line missing its uuid) it falls back to a random id — never
// worse than the previous always-random behavior.
func (p *ClaudeTranscriptProcessor) stableEventID(sourceKey, kind string) string {
	if sourceKey == "" {
		return event.NewUUID()
	}
	return event.DeterministicUUID(p.sessionID + "\x1f" + kind + "\x1f" + sourceKey)
}

// newTranscriptEvent builds a canonical event for a transcript-derived kind.
// sourceKey is the stable identity of the source (line uuid / message.id /
// tool_use_id); pass "" only when no stable key exists.
func (p *ClaudeTranscriptProcessor) newTranscriptEvent(kind, ts, sourceKey string) event.Event {
	e := event.NewEvent(kind, p.sessionID)
	// Overwrite NewEvent's random id with the stable, source-derived one (the
	// discarded random read is negligible next to this event's Ed25519 signing
	// + POST; keeping NewEvent as the single source of Ts/Source/V defaults).
	e.ID = p.stableEventID(sourceKey, kind)
	e.Source = "claude-code"
	switch kind {
	case "prompt":
		e.Actor = event.HumanActor()
		e.Provenance = transcriptHumanProvenance()
	default:
		e.Actor = event.AIActor()
	}
	if t := parseCodexTs(ts); t != "" {
		e.Ts = t
	}
	return e
}

// Process parses one transcript line and returns zero or more canonical events.
//
// Lane identity was twice stamped onto data.meta (ideSessionId / permissionMode
// / promptId / cwd) and twice removed — it grew back after the first removal,
// which is why this note is a prohibition and not a changelog. It is dead by
// construction: the redaction projector allowlists no `meta` key for any kind,
// so it was stripped before the buffer, the signature, and the wire — nothing
// ever received it. Do not reassemble it. `cwd` is an absolute filesystem path,
// and a map is not separable at the projector (it allowlists KEYS, so keeping
// any field inside means allowlisting the map whole) — so the only thing
// standing between cwd and the wire would be the absence of one allowlist line.
// Anything genuinely needed downstream goes on the envelope (where the
// projector cannot reach it) or gets hoisted to its own top-level, individually
// allowlistable data key — see promptSource in promptEvent.
func (p *ClaudeTranscriptProcessor) Process(line []byte) []event.Event {
	return p.processLine(line)
}

func (p *ClaudeTranscriptProcessor) processLine(line []byte) []event.Event {
	var rec map[string]interface{}
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	// The transcript path is the primary source of session identity (the file IS
	// the session), but fall back to the transcript's own sessionId if the path
	// could not be parsed. On a subagent transcript this field is the PARENT
	// session — which is what rolls that work up rather than fragmenting it.
	if p.sessionID == "" {
		p.sessionID = stringField(rec, "sessionId")
	}
	// Sidechain attribution hints are constant per file — pin the first value
	// each of them shows up with.
	if p.AgentID == "" {
		p.AgentID = stringField(rec, "agentId")
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
	// <session>/subagents/agent-*.jsonl, which the watcher tails in UsageOnly
	// mode — the inline check is belt-and-braces for older layouts.)
	sidechain, _ := rec["isSidechain"].(bool)
	if p.UsageOnly || sidechain {
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
func (p *ClaudeTranscriptProcessor) compactBoundaryEvent(rec map[string]interface{}) event.Event {
	ts, _ := rec["timestamp"].(string)
	e := p.newTranscriptEvent("context_compact", ts, stringField(rec, "uuid"))
	e.Actor = event.SystemActor()
	data := map[string]interface{}{}
	if cm, ok := rec["compactMetadata"].(map[string]interface{}); ok {
		if trigger := stringField(cm, "trigger"); trigger != "" {
			data["trigger"] = trigger
		}
	}
	e.Data = data
	trigger, _ := data["trigger"].(string)
	e.RawPayload = strPreview(fmt.Sprintf("compact boundary trigger=%s", trigger), 100)
	return e
}

// sidechainUsage extracts ONLY token usage from a sidechain (subagent) line:
// one subagent_usage event per assistant message.id, carrying the request's
// usage + model and nothing else. No accumulation is needed — every line of a
// message repeats the same usage.
func (p *ClaudeTranscriptProcessor) sidechainUsage(rec map[string]interface{}) []event.Event {
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
	// message.id is stable per subagent API response across re-reads/forks.
	e := p.newTranscriptEvent("subagent_usage", ts, msgID)
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
	if p.AgentID != "" {
		data["agentId"] = p.AgentID
	}
	if p.attributionSkill != "" {
		data["attributionSkill"] = p.attributionSkill
	}
	if p.attributionAgent != "" {
		data["attributionAgent"] = p.attributionAgent
	}
	e.Data = data
	e.RawPayload = strPreview(fmt.Sprintf("subagent usage msg=%s", msgID), 100)
	return []event.Event{e}
}

func (p *ClaudeTranscriptProcessor) handleAssistant(rec map[string]interface{}, line []byte) []event.Event {
	msg, _ := rec["message"].(map[string]interface{})
	if msg == nil {
		return p.flushAccum()
	}
	msgID := stringField(msg, "id")
	ts, _ := rec["timestamp"].(string)

	// Any assistant activity clears a pending interrupt back-link: an assistant
	// record standing between an interrupt and the next prompt means the
	// interrupt was an abort with no redirect prompt following it.
	p.pendingInterruptID = ""

	var events []event.Event
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

	// Re-derive the positional interrupt context from THIS record's own content.
	// One API message is chunked across multiple records that share message.id;
	// a tool_use record followed by a text-only record for the same id must leave
	// the cut-tool state CLEARED, so an interrupt after the text record is
	// classified "generation", not a stale "action". So track the most-recent
	// RECORD's content, not sticky state keyed on message.id.
	recordHadTool := false
	var recordToolName string

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
			// This record contains a tool call, so an interrupt arriving next cut
			// an ACTION. Record the tool NAME (never its input) so the interrupt
			// event can name what was killed without carrying source.
			recordHadTool = true
			recordToolName = stringField(block, "name")
		}
		// thinking / other block types carry no event of their own.
	}

	// Reflect the most-recent assistant record: a tool_use record sets the
	// cut-tool state; a text-only (or thinking-only) record clears it.
	p.lastAssistantHadTool = recordHadTool
	p.lastCutToolName = recordToolName

	return events
}

// flushAccum emits the accumulated assistant message as ONE ai_response event
// carrying the request's token usage and model — the per-request usage series
// the worker needs for context-trend signals and estimated pricing.
func (p *ClaudeTranscriptProcessor) flushAccum() []event.Event {
	if p.accum == nil {
		return nil
	}
	a := p.accum
	p.accum = nil
	if a.msgID == "" || p.emittedMsgIDs[a.msgID] {
		return nil
	}
	p.emittedMsgIDs[a.msgID] = true

	// message.id keys the whole accumulated response; it is stable across a
	// re-read of the transcript or a resumed/forked copy of these lines, so the
	// ai_response id is deterministic even though it spans multiple lines.
	e := p.newTranscriptEvent("ai_response", a.ts, a.msgID)
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
	return []event.Event{e}
}

// flushStale force-flushes an accumulated assistant message that has not seen
// a new line in maxAge — covers the final message of a turn when Claude Code
// writes no further boundary line for a while.
func (p *ClaudeTranscriptProcessor) FlushStale(maxAge time.Duration) []event.Event {
	if p.accum == nil || time.Since(p.accum.updatedAt) < maxAge {
		return nil
	}
	return p.flushAccum()
}

func (p *ClaudeTranscriptProcessor) handleUser(rec map[string]interface{}, line []byte) []event.Event {
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

	uuid := stringField(rec, "uuid")

	switch content := msg["content"].(type) {
	case string:
		// An ESC/Ctrl+C interrupt arrives here as a plain-string user message —
		// intercept it before it becomes a spurious prompt.
		if variant, ok := interruptVariant(content); ok {
			return p.interruptEvent(variant, ts, uuid, line)
		}
		return p.promptEvent(content, rec, ts, line)
	case []interface{}:
		var events []event.Event
		var textParts []string
		hasToolResult := false
		interruptVar := ""
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]interface{})
			if block == nil {
				continue
			}
			switch stringField(block, "type") {
			case "tool_result":
				// A tool_result marks this as a synthetic (non-prompt) record —
				// set the flag FIRST so the "...for tool use" sentinel path below
				// can't fall through and also emit a stray prompt from a sibling
				// text block. The sentinel can land inside a tool_result block when
				// a tool call was cut — catch it before pairing.
				hasToolResult = true
				if variant, ok := interruptVariant(toolResultText(block)); ok {
					interruptVar = variant
					continue
				}
				if ev, ok := p.resolveToolResult(rec, block, ts, line); ok {
					events = append(events, ev)
				}
			case "text":
				if variant, ok := interruptVariant(stringField(block, "text")); ok {
					interruptVar = variant
					continue
				}
				textParts = append(textParts, stringField(block, "text"))
			}
		}
		if interruptVar != "" {
			events = append(events, p.interruptEvent(interruptVar, ts, uuid, line)...)
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

// interruptSentinels maps the exact synthetic user text Claude Code writes on an
// ESC/Ctrl+C interrupt to the variant recorded on the interrupt event. Match is
// on the trimmed, whole string only.
var interruptSentinels = map[string]string{
	"[Request interrupted by user]":              "generation",
	"[Request interrupted by user for tool use]": "tool_use",
}

// interruptVariant reports whether text is an interrupt sentinel and, if so, the
// variant ("generation" | "tool_use") to stamp on the event.
func interruptVariant(text string) (string, bool) {
	v, ok := interruptSentinels[strings.TrimSpace(text)]
	return v, ok
}

// toolResultText returns the textual content of a tool_result block. Content is
// either a plain string or an array of blocks (Claude Code writes the interrupt
// sentinel in both shapes); for the array form the text of every type=="text"
// block is concatenated. Returns "" for any other shape.
func toolResultText(block map[string]interface{}) string {
	switch content := block["content"].(type) {
	case string:
		return content
	case []interface{}:
		var parts []string
		for _, rawInner := range content {
			inner, _ := rawInner.(map[string]interface{})
			if inner == nil {
				continue
			}
			if stringField(inner, "type") == "text" {
				parts = append(parts, stringField(inner, "text"))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// interruptEvent emits an `interrupt` event describing what the developer cut
// mid-response. subtype is classified positionally from the most-recent
// assistant record: "action" if it contained a tool_use block (naming the cut
// tool), else "generation". variant records which sentinel matched.
//
// SOURCE EXCLUSION: unlike the hiring CLI this carries NO cutToolInput — a Bash
// command body or file path is source-adjacent and would violate the teams
// "never store your code" guarantee. Only the tool NAME (safe, same class as a
// slash-command name), subtype, and variant are emitted; the redact projector's
// interrupt allowlist enforces the same restriction as defense-in-depth.
//
// Consecutive interrupts (ESC ESC) collapse: a second interrupt while one is
// still pending is skipped so a burst counts once. sourceKey is the synthetic
// user line's own uuid, so the emitted interrupt gets a deterministic id.
func (p *ClaudeTranscriptProcessor) interruptEvent(variant, ts, sourceKey string, line []byte) []event.Event {
	if p.pendingInterruptID != "" {
		return nil
	}
	subtype := "generation"
	data := map[string]interface{}{
		"variant": variant,
	}
	if p.lastAssistantHadTool {
		subtype = "action"
		if p.lastCutToolName != "" {
			data["cutTool"] = p.lastCutToolName
		}
	}
	data["subtype"] = subtype

	e := p.newTranscriptEvent("interrupt", ts, sourceKey)
	e.Actor = event.HumanActor()
	e.Provenance = transcriptHumanProvenance()
	e.Data = data
	e.RawPayload = strPreview(string(line), 500)
	p.pendingInterruptID = e.ID
	return []event.Event{e}
}

func (p *ClaudeTranscriptProcessor) promptEvent(text string, rec map[string]interface{}, ts string, line []byte) []event.Event {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	// Local command OUTPUT is not an invocation — drop it.
	//
	// The <bash-*> family is the `!`-prefixed bash mode: Claude Code writes the
	// invocation AND its captured stdout/stderr into the transcript as user
	// lines, so without this they ship as prompt.text — shell commands, absolute
	// paths, infra hostnames and raw stdout, i.e. exactly what the redact
	// projector exists to prevent ("never stdout/stderr"). None of its three
	// layers catches them: the projector allowlists prompt.text (it is the
	// product), scrubInlineCommand only runs on shellCommandKinds, and the DB
	// CHECK guards a `stdout` KEY, not stdout INSIDE text.
	//
	// Unlike task-notifications below, dropping at capture is the correct
	// boundary: source exclusion is a capture-side GUARANTEE ("source never
	// leaves the engineer's machine"), not a filtering preference the backend
	// may revisit. A source-bearing line that reaches the buffer has already
	// broken the promise, so it must never be created.
	if strings.HasPrefix(trimmed, "<local-command") ||
		strings.HasPrefix(trimmed, "<bash-input") ||
		strings.HasPrefix(trimmed, "<bash-stdout") ||
		strings.HasPrefix(trimmed, "<bash-stderr") {
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

	e := p.newTranscriptEvent("prompt", ts, stringField(rec, "uuid"))
	data := map[string]interface{}{"text": text}
	if command != "" {
		data["command"] = command
	}

	// promptSource is Claude Code's own provenance marker for the turn: "typed"
	// (a human at the keyboard) vs harness-injected values like "system" — which
	// is what <task-notification> background-task blobs carry. The backend needs
	// it to keep pseudo-prompts out of the fluency judge and the reps miner,
	// where they score as an engineer's bad prompting.
	//
	// TOP-LEVEL, never nested on a map: the projector allowlists KEYS, so a map
	// can only be kept whole — which is how `cwd` would ride along. Its own key
	// is the only shape that is individually allowlistable (see the Process
	// header above, and the "promptSource" entry in redact.projectFieldAllowlist
	// — this key persists NOTHING until it is listed there and in the backend's
	// eventFieldProjection.ts).
	//
	// Do NOT drop task-notifications here. Deliberate: a client-side drop is
	// irreversible and bakes into every installed CLI forever (old CLIs stay
	// installed), while the backend filters on read and can change its mind. Ship
	// the signal, not the verdict. Contrast the <bash-*> drop above, which is a
	// source-exclusion guarantee rather than a filtering preference.
	if v := clampPromptSource(stringField(rec, "promptSource")); v != "" {
		data["promptSource"] = v
	}

	// workdir — WHERE this session ran, home-collapsed to "~/…" so it names the
	// repo/worktree without leaking the OS username the absolute path carries. It
	// rides on its own individually-allowlisted key (redact.projectFieldAllowlist
	// "prompt" → "workdir"); the raw absolute `cwd` stays DROPPED. HomeRelativeStrict
	// emits ONLY a provably home-relative ("~"-prefixed) value — an outside-home cwd
	// or a home-lookup failure returns "", so the field is omitted rather than
	// leaking an absolute path that may carry the OS username.
	if wd := state.HomeRelativeStrict(stringField(rec, "cwd")); wd != "" {
		data["workdir"] = wd
	}

	// Track the previous human-prompt time as lane state only. No timing,
	// length, or paste signals are emitted — teams capture is content, not
	// behavioral analysis of the developer.
	if lineTime, err := time.Parse(time.RFC3339, ts); err == nil {
		p.lastPromptTs = lineTime
	}

	// This is the redirect prompt following an interrupt (no assistant record
	// intervened, or it would have cleared pendingInterruptID) — back-link it.
	if p.pendingInterruptID != "" {
		e.RelatedEventIDs = append(e.RelatedEventIDs, p.pendingInterruptID)
		data["followsInterrupt"] = true
		p.pendingInterruptID = ""
	}

	e.Data = data
	e.RawPayload = strPreview(string(line), 500)
	return []event.Event{e}
}

// promptSourceMax / promptSourceRe SHAPE-clamp the vendor's promptSource token:
// a lower-snake identifier, nothing else. Compiled at package level — promptEvent
// is the event hot path.
const promptSourceMax = 32

var promptSourceRe = regexp.MustCompile(`^[a-z][a-z_]*$`)

// clampPromptSource returns v when it is a plausible vendor enum token, else "".
//
// Shape, deliberately NOT a value enum. An enum set would have to name every
// value Claude Code might ship (today: typed / queued / system /
// suggestion_accepted) and would silently drop the next one — `resumed`,
// `hook_injected` — until a CLI release adopted it, and the CLI is the
// slow-propagating side of this contract (old builds stay installed for
// months). The shape adopts unknown future values for free.
//
// It is still a real boundary, not a formality: a lower-snake token capped at 32
// chars structurally cannot carry a path (no `/`), a URL, JSON, or prose (no
// spaces, no digits, no punctuation) — so this field cannot become a source leak
// however the vendor's shape drifts. That is what earns it its allowlist entry.
func clampPromptSource(v string) string {
	if len(v) == 0 || len(v) > promptSourceMax || !promptSourceRe.MatchString(v) {
		return ""
	}
	return v
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
func (p *ClaudeTranscriptProcessor) resolveToolResult(rec map[string]interface{}, block map[string]interface{}, ts string, line []byte) (event.Event, bool) {
	id := stringField(block, "tool_use_id")
	call, ok := p.pendingTools[id]
	if !ok {
		return event.Event{}, false
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
		return event.Event{}, false
	}
	ev.Source = "claude-code"
	// tool_use_id is unique per tool call and stable across re-reads / forked
	// transcripts, so derive a deterministic id from it (the normalizer minted a
	// random one). One tool_use resolves to exactly one event.
	ev.ID = p.stableEventID(id, ev.Kind)
	if t := parseCodexTs(ts); t != "" {
		ev.Ts = t
	}
	if ev.Actor == nil {
		ev.Actor = event.AIActor()
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
