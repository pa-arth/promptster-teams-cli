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

// intFromJSON extracts an int from a JSON number (float64).
func intFromJSON(v interface{}) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
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
		e := event.NewEvent("file_read", sessionID)
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
		e := event.NewEvent("file_search", sessionID)
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
		e := event.NewEvent("planning", sessionID)
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

	case toolName == "TodoRead":
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

	case toolName == "Task":
		taskDesc, _ := toolInput["description"].(string)
		e := event.NewEvent("task_dispatch", sessionID)
		e.Data = map[string]interface{}{
			"taskPreview": strPreview(taskDesc, 100),
		}
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
