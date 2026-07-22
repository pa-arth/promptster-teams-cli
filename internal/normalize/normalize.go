package normalize

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// lastPromptStatePath returns the file used to persist the last UserPromptSubmit
// timestamp across process invocations.
func lastPromptStatePath() string {
	if p := os.Getenv("PROMPTSTER_BUFFER_PATH"); p != "" {
		return filepath.Join(filepath.Dir(p), "last_prompt.ts")
	}
	return filepath.Join(state.StateDir(), "last_prompt.ts")
}

// saveLastPromptTs writes the current Unix-millisecond timestamp to the state file.
func saveLastPromptTs() {
	p := lastPromptStatePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(fmt.Sprintf("%d", time.Now().UnixMilli())), 0o600)
}

// loadLastPromptTs reads the persisted prompt timestamp, returning zero if unavailable.
func loadLastPromptTs() time.Time {
	p := lastPromptStatePath()
	// #nosec G304 -- p is lastPromptStatePath(), derived from state.StateDir(), not user input.
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

// lineRangesFromStructuredPatch derives the new-file-side line span of each
// structuredPatch hunk as content-free {start,end,attribution} triples. This is
// the WHICH-lines counterpart to countLinesFromPatches' how-many: it carries
// only integers + the event's provenance enum, never any diff bytes or file
// content (the projector's element allowlist structurally enforces that).
//
// The result is []interface{} of map[string]interface{} — the ONLY array shape
// redact.projectArrayElements can walk (a []map[string]any would be stripped to
// empty because it does not assert to []interface{}, and this path never
// round-trips through JSON before projection).
//
// A pure-deletion hunk (newLines == 0) has no new-file lines to attribute and
// is skipped.
func lineRangesFromStructuredPatch(patches []interface{}, attribution string) []interface{} {
	ranges := make([]interface{}, 0, len(patches))
	for _, p := range patches {
		patch, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		newStart := intFromJSON(patch["newStart"])
		newLines := intFromJSON(patch["newLines"])
		if newLines == 0 {
			continue
		}
		ranges = append(ranges, map[string]interface{}{
			"start":       newStart,
			"end":         newStart + newLines - 1,
			"attribution": attribution,
		})
	}
	return ranges
}

// intFromJSON extracts an int from a JSON number (float64).
func intFromJSON(v interface{}) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// taskStatusFromResponse returns the status TaskUpdate actually resolved to,
// from {"statusChange":{"from":"pending","to":"in_progress"}}. A short
// fixed-vocabulary token (pending/in_progress/completed), never prose.
// taskCallSucceeded reports whether a Task* response is positive evidence the
// call actually did what it was asked. Verified response shapes:
//
//	TaskCreate → {"task": {"id": "1", "subject": "..."}}
//	TaskUpdate → {"success": true, "taskId": "1", "statusChange": {"from","to"}}
//
// Only TaskCreate/TaskUpdate are judged. TodoWrite predates this shape and is
// always treated as succeeded — it has no known success marker, and inventing a
// check for one would silently drop every legacy client's planning events.
func taskCallSucceeded(toolName string, toolResponse map[string]interface{}) bool {
	switch toolName {
	case "TaskCreate":
		task, ok := toolResponse["task"].(map[string]interface{})
		return ok && len(task) > 0
	case "TaskUpdate":
		if ok, isBool := toolResponse["success"].(bool); isBool {
			return ok
		}
		// No explicit success flag: a resolved transition is proof enough.
		_, ok := taskStatusFromResponse(toolResponse)
		return ok
	default:
		return true
	}
}

func taskStatusFromResponse(toolResponse map[string]interface{}) (string, bool) {
	change, ok := toolResponse["statusChange"].(map[string]interface{})
	if !ok {
		return "", false
	}
	to, ok := change["to"].(string)
	if !ok || to == "" {
		return "", false
	}
	return to, true
}

// normalizePostToolUseByTool converts a PostToolUse/postToolUse payload into a
// rich, tool-specific Event. Shared between Claude Code and Cursor normalizers.
func normalizePostToolUseByTool(toolName string, toolInput, toolResponse map[string]interface{}, sessionID, raw string) (event.Event, bool) {
	switch {
	case strings.Contains(toolName, "Edit") || toolName == "Write" || toolName == "StrReplace":
		filePath, _ := toolInput["file_path"].(string)
		if filePath == "" {
			filePath, _ = toolInput["path"].(string)
		}
		e := event.NewEvent("file_diff", sessionID)
		e.Provenance = event.AIProvenance()
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
				if ranges := lineRangesFromStructuredPatch(patches, e.Provenance.Attribution); len(ranges) > 0 {
					data["lineRanges"] = ranges
				}
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
				// A Write replaces the whole file, so every new line is AI.
				// Mirror lineRangesFromStructuredPatch's exact shape:
				// []interface{} of map[string]interface{}. This is the
				// non-round-tripped normalizer path -- a []map[string]any would
				// be silently stripped by the projector's .([]interface{}) assert.
				if lines > 0 {
					data["lineRanges"] = []interface{}{
						map[string]interface{}{"start": 1, "end": lines, "attribution": e.Provenance.Attribution},
					}
				}
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
		e := event.NewEvent("command", sessionID)
		e.Provenance = event.AIProvenance()
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
		// The file body, held only for the length count and the credential
		// key-NAME harvest below. It is never assigned into e.Data — the
		// no-source contract means a file_read carries a path and counts, and
		// `content` is not in the field allowlist at either end of the wire.
		body := ""
		// Claude Code sends tool_response as {type: "text", file: {filePath, content, numLines, ...}}
		if fileObj, ok := toolResponse["file"].(map[string]interface{}); ok {
			if content, ok := fileObj["content"].(string); ok {
				contentLen = len(content)
				body = content
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
			body = content
		}
		e := event.NewEvent("file_read", sessionID)
		e.Data = map[string]interface{}{
			"path":          path,
			"contentLength": contentLen,
			"numLines":      numLines,
		}
		// Credential KEY NAMES (never values) for dotenv-class files. Harvested
		// here rather than by re-reading the file: the body is already in hand,
		// so there is no second read and no window for it to change underneath
		// us. Runs AFTER the filePath fixup above, because the response's path is
		// the authoritative one and the classifier keys on it.
		//
		// Set only when non-empty. An absent field means "this CLI did not
		// harvest"; an empty array would mean "we looked inside and the file held
		// no keys", which is a different and much more dangerous claim.
		if keys := HarvestCredentialKeyNames(path, body); len(keys) > 0 {
			e.Data.(map[string]interface{})["credentialKeys"] = keys
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
		e := event.NewEvent("file_search", sessionID)
		e.Data = map[string]interface{}{
			"toolName":     toolName,
			"pattern":      pattern,
			"cwd":          cwd,
			"resultsCount": resultsCount,
		}
		e.RawPayload = raw
		return e, true

	// Claude Code renamed its todo tool: TodoWrite/TodoRead became
	// TaskCreate/TaskUpdate/TaskList. A rename doesn't throw, so `planning` went
	// to ZERO rows product-wide without a single error — while plan_decision
	// stayed alive purely because ExitPlanMode wasn't renamed. The old names are
	// kept for older clients still in the field.
	//
	// The new tools are NOT shape-compatible with the old one. There is no
	// `todos` key and no array anywhere in them (checked against ~450 real
	// TaskCreate calls: every todos/tasks-array variant in the wild is a model
	// mis-call that came back InputValidationError). TodoWrite rewrote the WHOLE
	// list each call; TaskCreate creates exactly ONE task per call and TaskUpdate
	// flips ONE status.
	case toolName == "TodoWrite" || toolName == "TaskCreate" || toolName == "TaskUpdate":
		// A REJECTED call is not planning. The tool_input carries what the agent
		// ASKED for, and it is populated identically whether the call succeeded or
		// failed — so recording off the input alone turns an InputValidationError
		// into a plan that never existed, and a failed status flip into progress
		// that never happened.
		//
		// Gated on POSITIVE proof of success (the response's own `task` /
		// `statusChange`), not on the absence of an error: error shapes for these
		// tools aren't pinned down, and "no error field I recognise" is not
		// evidence a thing worked. Same trap as the debug-arc resolver that marked
		// arcs fixed because nothing said otherwise.
		//
		// A wholly ABSENT response stays permissive — legacy clients and TodoWrite
		// predate this shape, and dropping their events would trade a small
		// fabrication for a large blind spot.
		if len(toolResponse) > 0 && !taskCallSucceeded(toolName, toolResponse) {
			return event.Event{}, false
		}
		e := event.NewEvent("planning", sessionID)
		data := map[string]interface{}{}
		if todos, ok := toolInput["todos"].([]interface{}); ok {
			// Legacy TodoWrite: the whole list arrives as one array. Kept verbatim
			// for shape-compatibility with older clients (redaction drops the
			// bodies — see projectFieldAllowlist).
			data["todos"] = todos
		}
		// `subject` rides as `title` deliberately: it is the same class of short
		// engineer-facing label the allowlist already keeps for this kind, so it
		// survives projection on BOTH sides today. A new `subject` key would be
		// stripped until the backend allowlist landed too — a field that never
		// survives is a fake fix.
		if subject, ok := toolInput["subject"].(string); ok && subject != "" {
			data["title"] = subject
		}
		// NO item/plan-size count is emitted, deliberately. TaskCreate's response
		// counter ("Task #3 created successfully") is the SESSION's cumulative task
		// ordinal, not the size of the current plan: a second plan in the same
		// session starts at the first plan's high-water mark, so a fresh 1-task plan
		// would report 6. Deleted tasks overcount the same way. Recovering a true
		// plan size needs a plan-boundary tracker — state this normalizer
		// deliberately doesn't keep — and nothing in this repo consumes a count at
		// all. Emitting the ordinal under a plan-size name would be exactly the bug
		// class this case exists to fix: a number that looks measured and isn't.
		if toolName == "TaskUpdate" {
			// A status flip EXECUTES a plan, it doesn't define one. Prefer the
			// response's RESOLVED transition; fall back to the requested input
			// status only for the ~19-in-786 real responses that come back
			// {success:true, updatedFields:[...]} with no statusChange.
			//
			// That fallback is only sound because the success gate above already
			// dropped rejected calls: toolInput["status"] is what was ASKED for, so
			// on a failure it would report the wished-for status as if it had been
			// applied. Reached here, the call is known to have succeeded, which
			// makes the requested status also the applied one. Do not move this
			// above the gate.
			if status, ok := taskStatusFromResponse(toolResponse); ok {
				data["status"] = status
			} else if status, ok := toolInput["status"].(string); ok && status != "" {
				data["status"] = status
			}
		}
		e.Data = data
		e.RawPayload = raw
		return e, true

	case toolName == "ExitPlanMode":
		// PostToolUse on ExitPlanMode marks the moment the candidate reviewed the
		// agent's plan. The approve/reject choice is the HUMAN's action even though
		// the tool call is the agent's — actor reflects that. The response preview
		// carries whatever approval state Claude Code reports; the worker
		// interprets it best-effort rather than us guessing here.
		e := event.NewEvent("plan_decision", sessionID)
		e.Actor = event.HumanActor()
		e.Provenance = &event.Provenance{
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

	// TaskList is the rename of TodoRead: a pure READ of the current plan. It
	// belongs here and not in `planning` above — routing observation into
	// `planning` would inflate planning volume with calls that changed nothing.
	case toolName == "TodoRead" || toolName == "TaskList":
		e := event.NewEvent("planning_read", sessionID)
		e.Data = map[string]interface{}{}
		e.RawPayload = raw
		return e, true

	case toolName == "WebSearch":
		query, _ := toolInput["query"].(string)
		e := event.NewEvent("web_lookup", sessionID)
		e.Data = map[string]interface{}{
			"toolName": toolName,
			"query":    query,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "WebFetch":
		url, _ := toolInput["url"].(string)
		e := event.NewEvent("web_lookup", sessionID)
		e.Data = map[string]interface{}{
			"toolName": toolName,
			"url":      url,
		}
		e.RawPayload = raw
		return e, true

	// Claude Code renamed the delegation tool: `Task` became `Agent`. Same rename
	// class as TodoWrite -> TaskCreate above, and it cost the same thing — a rename
	// doesn't throw, it just stops matching, so every spawn fell to `default` and
	// shipped as a generic tool_use. `task_dispatch` went to ZERO rows product-wide
	// and stayed there (verified in prod 2026-07-22: 0 for all time, against 658
	// tool_use rows named `Agent` across 104 sessions). The backend's
	// subagentDispatchCount maxed three channels and this was the only one of the
	// three anything had ever emitted, so team_harness_v1 reported a fleet that
	// never delegates. `Task` is kept for older clients still in the field.
	//
	// The DATA half was independently broken, and would have bitten the moment the
	// name was fixed: this emitted `taskPreview`, which has never been in either
	// default-deny allowlist (on-device internal/redact/project.go OR the backend's
	// eventFieldProjection.ts — both say {name, status, summary}). So the event
	// would have arrived with an EMPTY data object and the description — the whole
	// reason to prefer task_dispatch over the bare Agent tool_use — would have been
	// stripped on-device with no error and no telemetry. Emit under the keys the
	// allowlists actually pass. This widens nothing: `summary` was already
	// sanctioned, it just had no producer.
	case toolName == "Task" || toolName == "Agent":
		taskDesc, _ := toolInput["description"].(string)
		e := event.NewEvent("task_dispatch", sessionID)
		data := map[string]interface{}{
			// Description only — never `prompt`, which is the full delegated
			// instruction and can carry anything the human typed.
			"summary": strPreview(taskDesc, 100),
		}
		// WHICH agent was spun up ("Explore", "general-purpose", ...). A type name
		// chosen from a registry, not free text, and the dimension any
		// "what does this team delegate" surface is actually asking about.
		if st, _ := toolInput["subagent_type"].(string); st != "" {
			data["name"] = strings.TrimSpace(st)
		}
		e.Data = data
		e.RawPayload = raw
		return e, true

	case toolName == "LS":
		path, _ := toolInput["path"].(string)
		e := event.NewEvent("dir_list", sessionID)
		e.Data = map[string]interface{}{
			"path": path,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "Delete":
		path, _ := toolInput["path"].(string)
		e := event.NewEvent("file_delete", sessionID)
		e.Data = map[string]interface{}{
			"path": path,
		}
		e.RawPayload = raw
		return e, true

	case toolName == "Skill":
		// Skill invocations carry the skill NAME in the args ({skill, args}).
		// Surface it as `skill` — name only, never the skill body or args —
		// so per-skill usage/ROI can be rolled up. `tool` mirrors toolName
		// under the field name the teams projector persists.
		e := event.NewEvent("tool_use", sessionID)
		data := map[string]interface{}{
			"toolName":     toolName,
			"tool":         toolName,
			"inputPreview": jsonPreview(toolInput, 100),
			"ok":           true,
		}
		if skill := skillNameFromInput(toolInput); skill != "" {
			data["skill"] = skill
		}
		e.Data = data
		e.RawPayload = raw
		return e, true

	default:
		e := event.NewEvent("tool_use", sessionID)
		e.Data = map[string]interface{}{
			"toolName":     toolName,
			"tool":         toolName,
			"inputPreview": jsonPreview(toolInput, 100),
			"ok":           true,
		}
		e.RawPayload = raw
		return e, true
	}
}

// skillNameFromInput extracts the skill NAME from a Skill tool call's input.
// Current Claude Code sends {skill: "name", args: "..."}; older shapes carried
// the invocation as {command: "/name args"}. Name only — args are never taken.
func skillNameFromInput(toolInput map[string]interface{}) string {
	if s, _ := toolInput["skill"].(string); s != "" {
		return strings.TrimPrefix(strings.TrimSpace(s), "/")
	}
	if c, _ := toolInput["command"].(string); c != "" {
		name := strings.Fields(strings.TrimSpace(c))
		if len(name) > 0 {
			return strings.TrimPrefix(name[0], "/")
		}
	}
	return ""
}

// relativizeEventPaths rewrites any absolute file path inside event.Data
// to be relative to taskRoot, when the path actually lives inside it.
// This unifies editor-hook events (absolute paths like
// `/Users/x/workspace/foo.go`) with git-watcher events (relative paths
// like `foo.go`) so the replay can collapse them into one file entry.
//
// Best-effort only: paths outside taskRoot, paths that are already
// relative, or relativize failures fall through unchanged.
func RelativizeEventPaths(ev *event.Event, taskRoot string) {
	if taskRoot == "" || ev == nil {
		return
	}
	data, ok := ev.Data.(map[string]interface{})
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
