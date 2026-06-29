package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// newUUID generates a random UUID v4 using crypto/rand.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Event is the canonical Promptster event shape sent to /v1/hooks/ingest.
type Event struct {
	ID         string      `json:"id"`
	SessionID  string      `json:"sessionId"`
	Ts         string      `json:"ts"`
	Kind       string      `json:"kind"`
	Source     string      `json:"source"`
	V          int         `json:"v"`
	Data       interface{} `json:"data"`
	Actor      *Actor      `json:"actor,omitempty"`
	Provenance *Provenance `json:"provenance,omitempty"`
	RawPayload string      `json:"rawPayload,omitempty"`
	// Ed25519 signature over the canonical signing message (hex). Added by
	// signAndAppendEvent during buffer append; empty on legacy unsigned events.
	Sig string `json:"sig,omitempty"`
	// Hex of the previous event's `sig` in the session chain; empty for the
	// first event in the chain or for legacy unsigned sessions.
	PrevSig string `json:"prevSig,omitempty"`
}

// Provenance captures who authored a change and how confident we are.
type Provenance struct {
	Attribution   string   `json:"attribution"`
	Confidence    float64  `json:"confidence"`
	Observability string   `json:"observability"`
	Methods       []string `json:"methods"`
}

// Actor identifies who performed the action the event records (as opposed to
// Provenance, which is about who authored a *change*). The grading pipeline
// partitions every signal by actor: only human-attributable behavior drives
// rubric tiers; agent actions are judge context.
type Actor struct {
	Type string `json:"type"`           // ai | human | system | unknown
	Role string `json:"role,omitempty"` // assistant | developer | session
}

func humanActor() *Actor  { return &Actor{Type: "human", Role: "developer"} }
func aiActor() *Actor     { return &Actor{Type: "ai", Role: "assistant"} }
func systemActor() *Actor { return &Actor{Type: "system", Role: "session"} }

func aiProvenance() *Provenance {
	return &Provenance{
		Attribution:   "likely_ai",
		Confidence:    0.9,
		Observability: "high",
		Methods:       []string{"hook"},
	}
}

func humanProvenance() *Provenance {
	return &Provenance{
		Attribution:   "likely_human",
		Confidence:    0.8,
		Observability: "medium",
		Methods:       []string{"hook"},
	}
}

// gitPollProvenance is attribution for diffs observed by the git watcher. The
// watcher only emits content the AI channels did NOT claim (see diff_dedup.go),
// so what remains is human work — either a fresh manual edit (likely_human) or
// a manual edit layered on top of an earlier AI edit to the same file
// (ai_revised_by_human).
func gitPollProvenance(aiTouched bool) *Provenance {
	if aiTouched {
		return &Provenance{
			Attribution:   "ai_revised_by_human",
			Confidence:    0.6,
			Observability: "medium",
			Methods:       []string{"git-poll", "ai-path-ledger"},
		}
	}
	return &Provenance{
		Attribution:   "likely_human",
		Confidence:    0.7,
		Observability: "medium",
		Methods:       []string{"git-poll"},
	}
}

// codexTurnProvenance is attribution for working-tree diffs observed while a
// codex turn was actively writing its rollout: agent work the rollout does
// not itemize per-edit (shell-command writes, mid-turn snapshots of files the
// agent is still editing).
func codexTurnProvenance() *Provenance {
	return &Provenance{
		Attribution:   "likely_ai",
		Confidence:    0.6,
		Observability: "medium",
		Methods:       []string{"git-poll", "codex-turn-window"},
	}
}

func newEvent(kind, sessionID string) Event {
	if sessionID == "" {
		sessionID = "unknown"
	}
	return Event{
		ID:        newUUID(),
		SessionID: sessionID,
		Ts:        time.Now().UTC().Format(time.RFC3339Nano),
		Kind:      kind,
		Source:    "hook",
		V:         1,
	}
}

// lastPromptStatePath returns the file used to persist the last UserPromptSubmit
// timestamp across process invocations.
func lastPromptStatePath() string {
	if p := os.Getenv("PROMPTSTER_BUFFER_PATH"); p != "" {
		return filepath.Join(filepath.Dir(p), "last_prompt.ts")
	}
	return filepath.Join(stateDir(), "last_prompt.ts")
}

// saveLastPromptTs writes the current Unix-millisecond timestamp to the state file.
func saveLastPromptTs() {
	p := lastPromptStatePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(fmt.Sprintf("%d", time.Now().UnixMilli())), 0o644)
}

// loadLastPromptTs reads the persisted prompt timestamp, returning zero if unavailable.
func loadLastPromptTs() time.Time {
	p := lastPromptStatePath()
	data, err := os.ReadFile(p)
	if err != nil {
		return time.Time{}
	}
	var ms int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &ms); err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// rawPayloadString marshals payload to JSON and returns the first 500 chars.
func rawPayloadString(payload map[string]interface{}) string {
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

// strPreview returns the first n chars of s (appending "..." if truncated).
func strPreview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// jsonPreview marshals v to JSON and returns the first n chars.
func jsonPreview(v interface{}, n int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return strPreview(string(b), n)
}

// estimateCost returns an approximate USD cost for Sonnet-class models.
// Pricing used: input=$3/MTok, output=$15/MTok, cacheRead=$0.30/MTok, cacheWrite=$3.75/MTok.
func estimateCost(input, output, cacheRead, cacheWrite int64) float64 {
	return float64(input)*3.0/1_000_000 +
		float64(output)*15.0/1_000_000 +
		float64(cacheRead)*0.30/1_000_000 +
		float64(cacheWrite)*3.75/1_000_000
}

// normalizeClaudeCode converts a raw Claude Code hook payload into a canonical Event.
// Returns (event, true) on success, (zero, false) if the payload is unrecognised.
func normalizeClaudeCode(payload map[string]interface{}, sessionID string) (Event, bool) {
	hookName, _ := payload["hook_event_name"].(string)
	raw := rawPayloadString(payload)

	switch hookName {
	case "UserPromptSubmit":
		e := newEvent("prompt", sessionID)
		e.Actor = humanActor()
		e.Provenance = humanProvenance()
		// Claude Code sends the prompt text as "prompt" (not "transcript")
		promptText, _ := payload["prompt"].(string)
		if promptText == "" {
			// Legacy fallback
			promptText, _ = payload["transcript"].(string)
		}

		meta := map[string]interface{}{}
		if payloadSessionID, ok := payload["session_id"].(string); ok && payloadSessionID != "" {
			meta["ideSessionId"] = payloadSessionID
		}
		if model, ok := payload["model"].(string); ok && model != "" {
			meta["model"] = model
		}
		if permMode, ok := payload["permission_mode"].(string); ok && permMode != "" {
			meta["permissionMode"] = permMode
		}
		if transcriptPath, ok := payload["transcript_path"].(string); ok && transcriptPath != "" {
			meta["transcriptPath"] = transcriptPath
		}
		if cwd, ok := payload["cwd"].(string); ok && cwd != "" {
			meta["cwd"] = cwd
		}

		data := map[string]interface{}{"text": promptText}
		if len(meta) > 0 {
			data["meta"] = meta
		}

		e.Data = data
		e.RawPayload = raw

		// Persist timestamp so the Stop handler can compute turn duration.
		saveLastPromptTs()
		return e, true

	case "Stop":
		e := newEvent("ai_response", sessionID)
		e.Actor = aiActor()

		// Current Claude Code Stop payload fields (per docs):
		// last_assistant_message, stop_hook_active, session_id, cwd, permission_mode
		lastMsg, _ := payload["last_assistant_message"].(string)
		stopHookActive, _ := payload["stop_hook_active"].(bool)

		// Legacy fields (older Claude Code versions may still send these)
		stopReason, _ := payload["stop_reason"].(string)
		var inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64
		if usage, ok := payload["usage"].(map[string]interface{}); ok {
			if v, ok := usage["input_tokens"].(float64); ok {
				inputTokens = int64(v)
			}
			if v, ok := usage["output_tokens"].(float64); ok {
				outputTokens = int64(v)
			}
			if v, ok := usage["cache_read_tokens"].(float64); ok {
				cacheReadTokens = int64(v)
			}
			if v, ok := usage["cache_write_tokens"].(float64); ok {
				cacheWriteTokens = int64(v)
			}
		}

		data := map[string]interface{}{}
		if lastMsg != "" {
			data["lastAssistantMessage"] = lastMsg
		}
		data["stopHookActive"] = stopHookActive
		if stopReason != "" {
			data["stopReason"] = stopReason
		}
		if inputTokens > 0 || outputTokens > 0 {
			data["inputTokens"] = inputTokens
			data["outputTokens"] = outputTokens
			data["cacheReadTokens"] = cacheReadTokens
			data["cacheWriteTokens"] = cacheWriteTokens
			data["totalCost"] = estimateCost(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens)
		}
		if last := loadLastPromptTs(); !last.IsZero() {
			data["turnDurationMs"] = time.Since(last).Milliseconds()
		}

		e.Data = data
		e.RawPayload = raw
		return e, true

	case "SessionStart":
		e := newEvent("session_start", sessionID)
		e.Actor = systemActor()
		payloadSessionID, _ := payload["session_id"].(string)
		model, _ := payload["model"].(string)
		source, _ := payload["source"].(string) // startup|resume|clear|compact
		cwd, _ := payload["cwd"].(string)
		// Legacy fallback
		if cwd == "" {
			cwd, _ = payload["working_directory"].(string)
		}
		data := map[string]interface{}{
			"ideSessionId": payloadSessionID,
			"model":        model,
			"source":       source,
			"cwd":          cwd,
		}
		e.Data = data
		e.RawPayload = raw
		return e, true

	case "SessionEnd":
		e := newEvent("session_end", sessionID)
		e.Actor = systemActor()
		payloadSessionID, _ := payload["session_id"].(string)
		reason, _ := payload["reason"].(string) // clear|resume|logout|prompt_input_exit|other
		// Legacy fallback
		if reason == "" {
			reason, _ = payload["stop_reason"].(string)
		}
		e.Data = map[string]interface{}{
			"ideSessionId": payloadSessionID,
			"reason":       reason,
		}
		e.RawPayload = raw
		return e, true

	case "SubagentStart":
		e := newEvent("subagent_start", sessionID)
		e.Actor = aiActor()
		subagentID, _ := payload["subagent_id"].(string)
		parentSessionID, _ := payload["parent_session_id"].(string)
		taskDesc, _ := payload["task_description"].(string)
		e.Data = map[string]interface{}{
			"subagentId":      subagentID,
			"parentSessionId": parentSessionID,
			"taskDescription": taskDesc,
		}
		e.RawPayload = raw
		return e, true

	case "SubagentStop":
		e := newEvent("subagent_stop", sessionID)
		e.Actor = aiActor()
		subagentID, _ := payload["subagent_id"].(string)
		stopReason, _ := payload["stop_reason"].(string)
		totalTurns := 0
		if v, ok := payload["total_turns"].(float64); ok {
			totalTurns = int(v)
		}
		e.Data = map[string]interface{}{
			"subagentId": subagentID,
			"stopReason": stopReason,
			"totalTurns": totalTurns,
		}
		e.RawPayload = raw
		return e, true

	case "PreCompact":
		e := newEvent("context_compact", sessionID)
		e.Actor = systemActor()
		var contextPct float64
		if v, ok := payload["context_window_used_pct"].(float64); ok {
			contextPct = v
		} else if v, ok := payload["context_pct"].(float64); ok {
			contextPct = v
		}
		e.Data = map[string]interface{}{
			"contextPct": contextPct,
		}
		e.RawPayload = raw
		return e, true

	case "PreToolUse":
		e := newEvent("tool_intent", sessionID)
		e.Actor = aiActor()
		toolName, _ := payload["tool_name"].(string)
		inputPreview := jsonPreview(payload["tool_input"], 100)
		e.Data = map[string]interface{}{
			"toolName":     toolName,
			"inputPreview": inputPreview,
		}
		e.RawPayload = raw
		return e, true

	case "PostToolUse":
		toolName, _ := payload["tool_name"].(string)
		toolInput, _ := payload["tool_input"].(map[string]interface{})
		if toolInput == nil {
			toolInput = map[string]interface{}{}
		}
		toolResponse, _ := payload["tool_response"].(map[string]interface{})
		if toolResponse == nil {
			toolResponse = map[string]interface{}{}
			// Claude Code may send tool_response as a plain string (e.g. Read tool
			// returns the file content directly). Wrap it so downstream handlers
			// can access it uniformly via toolResponse["content"].
			if s, ok := payload["tool_response"].(string); ok {
				toolResponse["content"] = s
			}
		}
		ev, ok := normalizePostToolUseByTool(toolName, toolInput, toolResponse, sessionID, raw)
		if ok && ev.Actor == nil {
			// Every tool-driven event is the agent acting unless the branch said
			// otherwise (plan_decision is the human's call).
			ev.Actor = aiActor()
		}
		return ev, ok

	case "PostToolUseFailure":
		toolName, _ := payload["tool_name"].(string)
		errMsg, _ := payload["error"].(string)
		if errMsg == "" {
			errMsg = fmt.Sprintf("tool %s failed", toolName)
		}
		e := newEvent("tool_call", sessionID)
		e.Actor = aiActor()
		e.Data = map[string]interface{}{
			"tool":  toolName,
			"ok":    false,
			"error": errMsg,
		}
		e.RawPayload = raw
		return e, true

	default:
		return Event{}, false
	}
}

// buildUnifiedDiff produces a minimal unified diff string from old/new content.
func buildUnifiedDiff(path, oldStr, newStr string) string {
	oldLines := strings.Split(oldStr, "\n")
	newLines := strings.Split(newStr, "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", filepath.Base(path), filepath.Base(path))
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", 1, len(oldLines), 1, len(newLines))
	for _, l := range oldLines {
		fmt.Fprintf(&b, "-%s\n", l)
	}
	for _, l := range newLines {
		fmt.Fprintf(&b, "+%s\n", l)
	}
	return b.String()
}

// buildNewFileDiff produces a unified diff for a newly created file.
func buildNewFileDiff(path, content string) string {
	lines := strings.Split(content, "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "--- /dev/null\n+++ b/%s\n", filepath.Base(path))
	fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
	for _, l := range lines {
		fmt.Fprintf(&b, "+%s\n", l)
	}
	return b.String()
}

// buildDiffFromStructuredPatch builds a unified diff string from Claude Code's
// structuredPatch array: [{oldStart, oldLines, newStart, newLines, lines: ["-old", "+new", " ctx"]}]
func buildDiffFromStructuredPatch(path string, patches []interface{}) string {
	var b strings.Builder
	baseName := filepath.Base(path)
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", baseName, baseName)
	for _, p := range patches {
		patch, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		oldStart := intFromJSON(patch["oldStart"])
		oldLines := intFromJSON(patch["oldLines"])
		newStart := intFromJSON(patch["newStart"])
		newLines := intFromJSON(patch["newLines"])
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldLines, newStart, newLines)
		if lines, ok := patch["lines"].([]interface{}); ok {
			for _, l := range lines {
				if s, ok := l.(string); ok {
					b.WriteString(s)
					b.WriteByte('\n')
				}
			}
		}
	}
	return b.String()
}

// countLinesFromPatches counts added/removed lines from structuredPatch.
func countLinesFromPatches(patches []interface{}) (added, removed int) {
	for _, p := range patches {
		patch, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if lines, ok := patch["lines"].([]interface{}); ok {
			for _, l := range lines {
				if s, ok := l.(string); ok && len(s) > 0 {
					switch s[0] {
					case '+':
						added++
					case '-':
						removed++
					}
				}
			}
		}
	}
	return
}

// intFromJSON extracts an int from a JSON number (float64).
func intFromJSON(v interface{}) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// normalizePostToolUseByTool converts a PostToolUse/postToolUse payload into a
// rich, tool-specific Event. Shared between Claude Code and Cursor normalizers.
func normalizePostToolUseByTool(toolName string, toolInput, toolResponse map[string]interface{}, sessionID, raw string) (Event, bool) {
	switch {
	case strings.Contains(toolName, "Edit") || toolName == "Write" || toolName == "StrReplace":
		filePath, _ := toolInput["file_path"].(string)
		if filePath == "" {
			filePath, _ = toolInput["path"].(string)
		}
		e := newEvent("file_diff", sessionID)
		e.Provenance = aiProvenance()
		data := map[string]interface{}{
			"path":        filePath,
			"attribution": "likely_ai",
		}
		if toolName != "Write" {
			oldStr, _ := toolInput["old_string"].(string)
			newStr, _ := toolInput["new_string"].(string)
			if oldStr != "" || newStr != "" {
				data["oldString"] = oldStr
				data["newString"] = newStr
			}

			// Claude Code sends structuredPatch in tool_response with precise hunks.
			// Use it to build a proper unified diff and accurate line counts.
			if patches, ok := toolResponse["structuredPatch"].([]interface{}); ok && len(patches) > 0 {
				data["diff"] = buildDiffFromStructuredPatch(filePath, patches)
				added, removed := countLinesFromPatches(patches)
				data["linesAdded"] = added
				data["linesRemoved"] = removed
			} else if oldStr != "" || newStr != "" {
				// Fallback: compute from old/new strings
				oldLines := strings.Count(oldStr, "\n")
				newLines := strings.Count(newStr, "\n")
				if oldStr != "" && !strings.HasSuffix(oldStr, "\n") {
					oldLines++
				}
				if newStr != "" && !strings.HasSuffix(newStr, "\n") {
					newLines++
				}
				data["linesRemoved"] = oldLines
				data["linesAdded"] = newLines
				data["diff"] = buildUnifiedDiff(filePath, oldStr, newStr)
			}
		} else {
			if content, ok := toolInput["content"].(string); ok && content != "" {
				data["content"] = content
				data["contentLength"] = len(content)
				// Count lines added (entire file is new)
				lines := strings.Count(content, "\n")
				if content != "" && !strings.HasSuffix(content, "\n") {
					lines++
				}
				data["linesAdded"] = lines
				data["linesRemoved"] = 0
				// Build synthetic diff for new file
				data["diff"] = buildNewFileDiff(filePath, content)
			}
		}
		e.Data = data
		e.RawPayload = raw
		return e, true

	case toolName == "Bash" || toolName == "Shell":
		command, _ := toolInput["command"].(string)
		exitCodeRaw, _ := toolResponse["exit_code"].(float64)
		stdout, _ := toolResponse["stdout"].(string)
		stderr, _ := toolResponse["stderr"].(string)
		e := newEvent("command", sessionID)
		e.Provenance = aiProvenance()
		e.Data = map[string]interface{}{
			"command":  command,
			"exitCode": int(exitCodeRaw),
			"stdout":   stdout,
			"stderr":   stderr,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "Read":
		path, _ := toolInput["file_path"].(string)
		if path == "" {
			path, _ = toolInput["path"].(string)
		}
		contentLen := 0
		numLines := 0
		// Claude Code sends tool_response as {type: "text", file: {filePath, content, numLines, ...}}
		if fileObj, ok := toolResponse["file"].(map[string]interface{}); ok {
			if content, ok := fileObj["content"].(string); ok {
				contentLen = len(content)
			}
			if n, ok := fileObj["numLines"].(float64); ok {
				numLines = int(n)
			}
			// Use the file path from the response if available (more reliable)
			if fp, ok := fileObj["filePath"].(string); ok && fp != "" {
				path = fp
			}
		} else if content, ok := toolResponse["content"].(string); ok {
			// Fallback: direct content field
			contentLen = len(content)
		}
		e := newEvent("file_read", sessionID)
		e.Data = map[string]interface{}{
			"path":          path,
			"contentLength": contentLen,
			"numLines":      numLines,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "Glob" || toolName == "Grep":
		pattern, _ := toolInput["pattern"].(string)
		if pattern == "" {
			pattern, _ = toolInput["glob_pattern"].(string)
		}
		cwd, _ := toolInput["path"].(string)
		if cwd == "" {
			cwd, _ = toolInput["target_directory"].(string)
		}
		resultsCount := 0
		if results, ok := toolResponse["filenames"].([]interface{}); ok {
			resultsCount = len(results)
		} else if results, ok := toolResponse["results"].([]interface{}); ok {
			resultsCount = len(results)
		}
		e := newEvent("file_search", sessionID)
		e.Data = map[string]interface{}{
			"toolName":     toolName,
			"pattern":      pattern,
			"cwd":          cwd,
			"resultsCount": resultsCount,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "TodoWrite":
		todos, _ := toolInput["todos"].([]interface{})
		e := newEvent("planning", sessionID)
		e.Data = map[string]interface{}{
			"todos": todos,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "ExitPlanMode":
		// PostToolUse on ExitPlanMode marks the moment the candidate reviewed the
		// agent's plan. The approve/reject choice is the HUMAN's action even though
		// the tool call is the agent's — actor reflects that. The response preview
		// carries whatever approval state Claude Code reports; the worker
		// interprets it best-effort rather than us guessing here.
		e := newEvent("plan_decision", sessionID)
		e.Actor = humanActor()
		e.Provenance = &Provenance{
			Attribution:   "likely_human",
			Confidence:    0.6,
			Observability: "medium",
			Methods:       []string{"hook", "plan-mode"},
		}
		planText, _ := toolInput["plan"].(string)
		e.Data = map[string]interface{}{
			"planPreview":     strPreview(planText, 1000),
			"responsePreview": jsonPreview(toolResponse, 300),
		}
		e.RawPayload = raw
		return e, true

	case toolName == "TodoRead":
		e := newEvent("planning_read", sessionID)
		e.Data = map[string]interface{}{}
		e.RawPayload = raw
		return e, true

	case toolName == "WebSearch":
		query, _ := toolInput["query"].(string)
		e := newEvent("web_lookup", sessionID)
		e.Data = map[string]interface{}{
			"toolName": toolName,
			"query":    query,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "WebFetch":
		url, _ := toolInput["url"].(string)
		e := newEvent("web_lookup", sessionID)
		e.Data = map[string]interface{}{
			"toolName": toolName,
			"url":      url,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "Task":
		taskDesc, _ := toolInput["description"].(string)
		e := newEvent("task_dispatch", sessionID)
		e.Data = map[string]interface{}{
			"taskPreview": strPreview(taskDesc, 100),
		}
		e.RawPayload = raw
		return e, true

	case toolName == "LS":
		path, _ := toolInput["path"].(string)
		e := newEvent("dir_list", sessionID)
		e.Data = map[string]interface{}{
			"path": path,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "Delete":
		path, _ := toolInput["path"].(string)
		e := newEvent("file_delete", sessionID)
		e.Data = map[string]interface{}{
			"path": path,
		}
		e.RawPayload = raw
		return e, true

	default:
		e := newEvent("tool_use", sessionID)
		e.Data = map[string]interface{}{
			"toolName":     toolName,
			"inputPreview": jsonPreview(toolInput, 100),
			"ok":           true,
		}
		e.RawPayload = raw
		return e, true
	}
}

// normalizeCursor converts a raw Cursor hook payload into a canonical Event.
// Supports both the current schema (hook_event_name, snake_case fields, Cursor 2.6+)
// and the legacy schema ("event" field, camelCase fields).
// Returns (event, true) on success, (zero, false) if unrecognised.
func normalizeCursor(payload map[string]interface{}, sessionID string) (Event, bool) {
	// New Cursor 2.6+ schema uses hook_event_name; old schema used "event"
	hookName, _ := payload["hook_event_name"].(string)
	if hookName == "" {
		hookName, _ = payload["event"].(string)
	}
	raw := rawPayloadString(payload)

	switch hookName {
	case "beforeSubmitPrompt":
		prompt, _ := payload["prompt"].(string)
		// New schema uses snake_case; fall back to old camelCase.
		conversationID, _ := payload["conversation_id"].(string)
		if conversationID == "" {
			conversationID, _ = payload["conversationId"].(string)
		}
		generationID, _ := payload["generation_id"].(string)
		if generationID == "" {
			generationID, _ = payload["generationId"].(string)
		}
		model, _ := payload["model"].(string)
		transcriptPath, _ := payload["transcript_path"].(string)
		e := newEvent("prompt", sessionID)
		e.Actor = humanActor()
		e.Provenance = humanProvenance()
		data := map[string]interface{}{
			"text":           prompt,
			"conversationId": conversationID,
			"generationId":   generationID,
		}
		if model != "" {
			data["model"] = model
		}
		if transcriptPath != "" {
			data["transcriptPath"] = transcriptPath
		}
		e.Data = data
		e.RawPayload = raw
		saveLastPromptTs()
		return e, true

	case "afterFileEdit":
		// Real Cursor schema: file_path + edits[]{old_string,new_string}. (The
		// legacy speculative normalizer read path/diff, which never matched a live
		// payload.) Synthesize a unified diff from the edit hunks.
		filePath, _ := payload["file_path"].(string)
		if filePath == "" {
			filePath, _ = payload["path"].(string)
		}
		edits, _ := payload["edits"].([]interface{})
		diff := buildDiffFromCursorEdits(filePath, edits)
		if diff == "" {
			// Back-compat: use a ready-made diff string if a payload provides one.
			diff, _ = payload["diff"].(string)
		}
		added, removed := countDiffLines(diff)
		e := newEvent("file_diff", sessionID)
		e.Actor = aiActor()
		e.Provenance = aiProvenance()
		e.Data = map[string]interface{}{
			"path":         filePath,
			"diff":         diff,
			"linesAdded":   added,
			"linesRemoved": removed,
			"attribution":  "likely_ai",
		}
		e.RawPayload = raw
		return e, true

	case "afterShellExecution":
		// Real Cursor schema carries output + duration (beforeShellExecution does
		// not), so this is the channel we register for shell commands.
		command, _ := payload["command"].(string)
		output, _ := payload["output"].(string)
		e := newEvent("command", sessionID)
		e.Actor = aiActor()
		e.Provenance = aiProvenance()
		data := map[string]interface{}{
			"command": command,
			"stdout":  output,
		}
		// Cursor's afterShellExecution exposes no explicit exit code — leave it
		// unset rather than fabricating a 0 (which would misreport failures).
		// durationMs must be an integer (CommandEventData.durationMs = int); Cursor
		// sends a float, so truncate.
		if d, ok := payload["duration"].(float64); ok && d >= 0 {
			data["durationMs"] = int64(d)
		}
		e.Data = data
		e.RawPayload = raw
		return e, true

	case "beforeShellExecution":
		// Not registered by Promptster (afterShellExecution is richer); kept for
		// back-compat if a candidate already wired this event.
		command, _ := payload["command"].(string)
		cwd, _ := payload["cwd"].(string)
		e := newEvent("command", sessionID)
		e.Actor = aiActor()
		e.Data = map[string]interface{}{
			"command": command,
			"cwd":     cwd,
		}
		e.RawPayload = raw
		return e, true

	case "afterToolCall":
		// Legacy Cursor hook name; kept for backward compatibility.
		toolName, _ := payload["toolName"].(string)
		resultPreview := strPreview(jsonPreview(payload["result"], 200), 200)
		e := newEvent("tool_use", sessionID)
		e.Actor = aiActor()
		e.Data = map[string]interface{}{
			"toolName":      toolName,
			"resultPreview": resultPreview,
		}
		e.RawPayload = raw
		return e, true

	case "postToolUse":
		toolName, _ := payload["tool_name"].(string)
		if toolName == "" {
			toolName, _ = payload["toolName"].(string)
		}
		toolInput, _ := payload["tool_input"].(map[string]interface{})
		if toolInput == nil {
			toolInput = map[string]interface{}{}
		}
		// Cursor sends tool_output as a JSON string; parse it into a map.
		toolResponse := map[string]interface{}{}
		if outStr, ok := payload["tool_output"].(string); ok && outStr != "" {
			_ = json.Unmarshal([]byte(outStr), &toolResponse)
		} else if outMap, ok := payload["tool_output"].(map[string]interface{}); ok {
			toolResponse = outMap
		}
		ev, ok := normalizePostToolUseByTool(toolName, toolInput, toolResponse, sessionID, raw)
		if !ok {
			return Event{}, false
		}
		if ev.Actor == nil {
			ev.Actor = aiActor()
		}
		// De-dup across Cursor hook channels: file edits, shell commands, and MCP
		// calls are captured by their dedicated observer hooks (afterFileEdit,
		// afterShellExecution, afterMCPExecution), which carry richer payloads.
		// Drop the postToolUse twin for those kinds so nothing is double-counted.
		// (If a live session shows Cursor routes these ONLY through postToolUse,
		// flip this — the cursor-CLI e2e validates which hooks actually fire.)
		switch ev.Kind {
		case "file_diff", "command", "mcp_call":
			return Event{}, false
		}
		return ev, true

	case "postToolUseFailure":
		toolName, _ := payload["tool_name"].(string)
		if toolName == "" {
			toolName, _ = payload["toolName"].(string)
		}
		// Real Cursor schema uses error_message; fall back to the legacy "error".
		errMsg, _ := payload["error_message"].(string)
		if errMsg == "" {
			errMsg, _ = payload["error"].(string)
		}
		if errMsg == "" {
			errMsg = fmt.Sprintf("tool %s failed", toolName)
		}
		e := newEvent("tool_call", sessionID)
		e.Actor = aiActor()
		e.Data = map[string]interface{}{
			"tool":  toolName,
			"ok":    false,
			"error": errMsg,
		}
		e.RawPayload = raw
		return e, true

	case "beforeReadFile":
		path, _ := payload["path"].(string)
		if path == "" {
			path, _ = payload["file_path"].(string)
		}
		if path == "" {
			if ti, ok := payload["tool_input"].(map[string]interface{}); ok {
				path, _ = ti["file_path"].(string)
				if path == "" {
					path, _ = ti["path"].(string)
				}
			}
		}
		e := newEvent("file_read", sessionID)
		e.Actor = aiActor()
		e.Data = map[string]interface{}{
			"path": path,
		}
		e.RawPayload = raw
		return e, true

	case "stop":
		e := newEvent("session_end", sessionID)
		e.Actor = systemActor()
		status, _ := payload["status"].(string)
		loopCount := 0
		if v, ok := payload["loop_count"].(float64); ok {
			loopCount = int(v)
		}
		data := map[string]interface{}{
			"status":    status,
			"loopCount": loopCount,
		}
		if last := loadLastPromptTs(); !last.IsZero() {
			data["turnDurationMs"] = time.Since(last).Milliseconds()
		}
		e.Data = data
		e.RawPayload = raw
		return e, true

	case "afterMCPExecution":
		// Real Cursor schema: tool_name, tool_input, result_json, duration.
		toolName, _ := payload["tool_name"].(string)
		e := newEvent("mcp_call", sessionID)
		e.Actor = aiActor()
		data := map[string]interface{}{
			"tool":        toolName,
			"argsPreview": jsonPreview(payload["tool_input"], 100),
		}
		if d, ok := payload["duration"].(float64); ok && d >= 0 {
			data["durationMs"] = int64(d)
		}
		e.Data = data
		e.RawPayload = raw
		return e, true

	case "beforeMCPExecution":
		// Not registered by Promptster (afterMCPExecution carries the result);
		// kept for back-compat.
		server, _ := payload["server"].(string)
		tool, _ := payload["tool"].(string)
		argsPreview := jsonPreview(payload["args"], 100)
		e := newEvent("mcp_call", sessionID)
		e.Actor = aiActor()
		e.Data = map[string]interface{}{
			"server":      server,
			"tool":        tool,
			"argsPreview": argsPreview,
		}
		e.RawPayload = raw
		return e, true

	case "subagentStop":
		subagentType, _ := payload["subagent_type"].(string)
		status, _ := payload["status"].(string)
		summary, _ := payload["summary"].(string)
		e := newEvent("subagent_stop", sessionID)
		e.Actor = aiActor()
		e.Data = map[string]interface{}{
			"subagentType": subagentType,
			"status":       status,
			"summary":      strPreview(summary, 200),
		}
		e.RawPayload = raw
		return e, true

	case "sessionStart":
		e := newEvent("session_start", sessionID)
		e.Actor = systemActor()
		e.Data = map[string]interface{}{
			"cursorSessionId": stringField(payload, "session_id"),
			"composerMode":    stringField(payload, "composer_mode"),
		}
		e.RawPayload = raw
		return e, true

	case "sessionEnd":
		e := newEvent("session_end", sessionID)
		e.Actor = systemActor()
		e.Data = map[string]interface{}{
			"cursorSessionId": stringField(payload, "session_id"),
			"reason":          stringField(payload, "reason"),
			"finalStatus":     stringField(payload, "final_status"),
		}
		e.RawPayload = raw
		return e, true

	default:
		return Event{}, false
	}
}

// buildDiffFromCursorEdits synthesizes a unified diff from Cursor's
// afterFileEdit edits[] array (each entry has old_string/new_string). Cursor
// does not provide line positions, so each edit becomes a hunk anchored at
// line 1 — fine for display and for +/- line counting; not a positionally
// accurate patch.
func buildDiffFromCursorEdits(path string, edits []interface{}) string {
	if len(edits) == 0 {
		return ""
	}
	base := filepath.Base(path)
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", base, base)
	wrote := false
	for _, raw := range edits {
		edit, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		oldLines := splitDiffLines(stringField(edit, "old_string"))
		newLines := splitDiffLines(stringField(edit, "new_string"))
		if len(oldLines) == 0 && len(newLines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "@@ -1,%d +1,%d @@\n", len(oldLines), len(newLines))
		for _, l := range oldLines {
			fmt.Fprintf(&b, "-%s\n", l)
		}
		for _, l := range newLines {
			fmt.Fprintf(&b, "+%s\n", l)
		}
		wrote = true
	}
	if !wrote {
		return ""
	}
	return b.String()
}

// splitDiffLines splits a string into lines for diff rendering, returning nil
// for the empty string (a pure insertion/deletion has no lines on that side)
// and trimming a single trailing newline so "foo\n" yields ["foo"], not
// ["foo", ""].
func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// detectSource attempts to infer the hook source from the payload shape when
// PROMPTSTER_HOOK_SOURCE is not set.
//
// Detection priority:
//  1. PROMPTSTER_HOOK_SOURCE env var (explicit override)
//  2. cursor_version field -> Cursor (only present in Cursor payloads)
//  3. conversation_id field (snake_case) -> Cursor (Claude Code uses session_id)
//  4. Old "event" field -> Cursor (legacy Cursor schema)
//  5. hook_event_name starting lowercase -> Cursor (e.g. "beforeSubmitPrompt", "stop")
//  6. hook_event_name starting uppercase -> Claude Code (e.g. "UserPromptSubmit", "Stop")
func detectSource(payload map[string]interface{}) string {
	if src := os.Getenv("PROMPTSTER_HOOK_SOURCE"); src != "" {
		return src
	}
	// cursor_version is only present in Cursor payloads.
	if _, ok := payload["cursor_version"]; ok {
		return "cursor"
	}
	// conversation_id (snake_case) is Cursor-specific; Claude Code uses session_id.
	if _, ok := payload["conversation_id"]; ok {
		return "cursor"
	}
	// Old Cursor schema used an "event" field instead of hook_event_name.
	if _, ok := payload["event"]; ok {
		return "cursor"
	}
	// Both Claude Code and new Cursor (2.6+) use hook_event_name.
	// Claude Code uses PascalCase values ("UserPromptSubmit", "Stop");
	// Cursor uses camelCase starting lowercase ("beforeSubmitPrompt", "stop").
	if name, ok := payload["hook_event_name"].(string); ok && len(name) > 0 {
		if name[0] >= 'a' && name[0] <= 'z' {
			return "cursor"
		}
		return "claude-code"
	}
	return "unknown"
}

// relativizeEventPaths rewrites any absolute file path inside event.Data
// to be relative to taskRoot, when the path actually lives inside it.
// This unifies editor-hook events (absolute paths like
// `/Users/x/workspace/foo.go`) with git-watcher events (relative paths
// like `foo.go`) so the replay can collapse them into one file entry.
//
// Best-effort only: paths outside taskRoot, paths that are already
// relative, or relativize failures fall through unchanged.
func relativizeEventPaths(event *Event, taskRoot string) {
	if taskRoot == "" || event == nil {
		return
	}
	data, ok := event.Data.(map[string]interface{})
	if !ok {
		return
	}
	cleanRoot := filepath.Clean(taskRoot)
	rewrite := func(key string) {
		s, ok := data[key].(string)
		if !ok || s == "" || !filepath.IsAbs(s) {
			return
		}
		rel, err := filepath.Rel(cleanRoot, filepath.Clean(s))
		if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
			return
		}
		data[key] = filepath.ToSlash(rel)
	}
	rewrite("path")
	rewrite("file_path")
}

// attachClaudeLaneMeta threads the per-process lane identifiers — Claude
// Code's session_id and the hook payload's cwd — into data.meta on EVERY
// hook event, not just prompts. Distinct session_ids are distinct concurrent
// `claude` processes (parallel sessions in the same workspace); distinct
// cwds are worktree/checkout isolation. The worker's parallelism signals and
// cross-lane file-collision detection key on these. Existing meta keys are
// never overwritten.
func attachClaudeLaneMeta(e *Event, payload map[string]interface{}) {
	sid, _ := payload["session_id"].(string)
	cwd, _ := payload["cwd"].(string)
	if sid == "" && cwd == "" {
		return
	}
	data, ok := e.Data.(map[string]interface{})
	if !ok || data == nil {
		return
	}
	meta, _ := data["meta"].(map[string]interface{})
	if meta == nil {
		meta = map[string]interface{}{}
	}
	if sid != "" {
		if _, exists := meta["ideSessionId"]; !exists {
			meta["ideSessionId"] = sid
		}
	}
	if cwd != "" {
		if _, exists := meta["cwd"]; !exists {
			meta["cwd"] = cwd
		}
	}
	data["meta"] = meta
}

// normalize dispatches to the right normalizer based on detected source.
func normalize(payload map[string]interface{}, sessionID string) (Event, bool) {
	src := detectSource(payload)
	switch src {
	case "claude-code":
		e, ok := normalizeClaudeCode(payload, sessionID)
		if ok {
			attachClaudeLaneMeta(&e, payload)
		}
		e.Source = src
		return e, ok
	case "cursor":
		e, ok := normalizeCursor(payload, sessionID)
		e.Source = src
		return e, ok
	default:
		return Event{}, false
	}
}
