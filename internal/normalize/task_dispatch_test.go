package normalize

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// task_dispatch was dead twice over, and each half hid the other.
//
//  1. Claude Code renamed the delegation tool `Task` -> `Agent`. The switch still
//     matched only "Task", so spawns fell to `default` and shipped as a generic
//     tool_use. Prod 2026-07-22: task_dispatch had ZERO rows for all time, against
//     658 tool_use rows named `Agent` across 104 sessions.
//  2. The emitted field was `taskPreview`, which has NEVER appeared in either
//     default-deny allowlist. Fixing only (1) would have shipped an event whose
//     data is stripped to {} on-device — no error, no telemetry, just silence.
//
// So the projection assertion below is not a nicety; it is the half that a
// name-only fix would have left broken.

// A tool_use block is buffered until its tool_result line arrives — only then is
// the pair fed through normalizePostToolUseByTool. Feeding the assistant line
// alone yields nothing, which is why these cases always ship both lines.
func processLines(lines ...string) []event.Event {
	p := NewClaudeTranscriptProcessor("sess-1")
	var out []event.Event
	for _, l := range lines {
		out = append(out, p.Process([]byte(l))...)
	}
	return out
}

func taskDispatchOf(t *testing.T, lines ...string) event.Event {
	t.Helper()
	for _, e := range processLines(lines...) {
		if e.Kind == "task_dispatch" {
			return e
		}
	}
	t.Fatalf("no task_dispatch event emitted")
	return event.Event{}
}

const (
	agentCall   = `{"type":"assistant","message":{"id":"msg-a","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_a","name":"Agent","input":{"description":"audit the rate limiter for off-by-one","prompt":"SECRET full instruction that must never ship","subagent_type":"Explore"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:01:00Z"}`
	agentResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"done"}]},"toolUseResult":{"content":"done"},"timestamp":"2026-06-10T10:01:30Z"}`
)

func TestTaskDispatchFromAgentTool(t *testing.T) {
	e := taskDispatchOf(t, agentCall, agentResult)
	if got := dm(e)["summary"]; got != "audit the rate limiter for off-by-one" {
		t.Errorf("summary = %v", got)
	}
	if got := dm(e)["name"]; got != "Explore" {
		t.Errorf("name = %v", got)
	}
}

// The pre-rename name still has to work: CLIs in the field are not all current,
// and a fix that trades one dead name for another is not a fix.
func TestTaskDispatchFromLegacyTaskTool(t *testing.T) {
	call := `{"type":"assistant","message":{"id":"msg-t","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_t","name":"Task","input":{"description":"summarize the migration","subagent_type":"general-purpose"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:01:00Z"}`
	res := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_t","content":"done"}]},"toolUseResult":{"content":"done"},"timestamp":"2026-06-10T10:01:30Z"}`
	e := taskDispatchOf(t, call, res)
	if got := dm(e)["summary"]; got != "summarize the migration" {
		t.Errorf("summary = %v", got)
	}
	if got := dm(e)["name"]; got != "general-purpose" {
		t.Errorf("name = %v", got)
	}
}

// THE test. Emitting under a key the allowlist doesn't carry is indistinguishable
// from not emitting at all, and that is exactly how this field died the first time.
func TestTaskDispatchSurvivesProjection(t *testing.T) {
	e := taskDispatchOf(t, agentCall, agentResult)
	redact.ProjectEvent(&e, false)
	data, _ := e.Data.(map[string]interface{})
	if data["summary"] != "audit the rate limiter for off-by-one" {
		t.Errorf("summary stripped by projection: %v", data["summary"])
	}
	if data["name"] != "Explore" {
		t.Errorf("name stripped by projection: %v", data["name"])
	}
}

// The full delegated instruction is not a description. It can carry anything the
// human typed, so it must never reach the event — before OR after projection.
func TestTaskDispatchNeverCarriesThePrompt(t *testing.T) {
	e := taskDispatchOf(t, agentCall, agentResult)
	for k, v := range dm(e) {
		if s, ok := v.(string); ok && s == "SECRET full instruction that must never ship" {
			t.Errorf("prompt leaked into data[%q]", k)
		}
		if k == "prompt" {
			t.Errorf("data carries a `prompt` key: %v", v)
		}
	}
}

// A spawn with no subagent_type is still a spawn. Omit the key rather than
// emitting an empty string, so ABSENT stays distinguishable from EMPTY.
func TestTaskDispatchOmitsMissingSubagentType(t *testing.T) {
	call := `{"type":"assistant","message":{"id":"msg-n","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_n","name":"Agent","input":{"description":"do the thing"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:01:00Z"}`
	res := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_n","content":"done"}]},"toolUseResult":{"content":"done"},"timestamp":"2026-06-10T10:01:30Z"}`
	e := taskDispatchOf(t, call, res)
	if got := dm(e)["summary"]; got != "do the thing" {
		t.Errorf("summary = %v", got)
	}
	if _, present := dm(e)["name"]; present {
		t.Errorf("name present on a spawn with no subagent_type: %v", dm(e)["name"])
	}
}

// A whitespace-only subagent_type must be treated as ABSENT, not as an empty
// name. Testing `!= ""` before trimming lets "   " through and emits `name: ""`,
// which collapses the very distinction the case above exists to preserve and
// opens a nameless agent-type bucket downstream.
func TestTaskDispatchTreatsWhitespaceSubagentTypeAsAbsent(t *testing.T) {
	call := `{"type":"assistant","message":{"id":"msg-ws","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_ws","name":"Agent","input":{"description":"do the thing","subagent_type":"   "}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:01:00Z"}`
	res := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_ws","content":"done"}]},"toolUseResult":{"content":"done"},"timestamp":"2026-06-10T10:01:30Z"}`
	e := taskDispatchOf(t, call, res)
	if v, present := dm(e)["name"]; present {
		t.Errorf("whitespace-only subagent_type emitted name=%q; want the key absent", v)
	}
}

// strPreview slices BYTES. Landing mid-rune leaves dangling continuation bytes,
// and Go's json encoder silently swaps them for U+FFFD — so the failure is a
// corrupted preview, never an error. Descriptions are human free text, so
// non-ASCII is a live case, not a hypothetical.
func TestTaskDispatchSummaryStaysValidUTF8(t *testing.T) {
	// 60 CJK runes = 180 bytes, so the 100-byte cap lands inside rune 34.
	desc := strings.Repeat("設", 60)
	call := `{"type":"assistant","message":{"id":"msg-u","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_u","name":"Agent","input":{"description":"` + desc + `","subagent_type":"Explore"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:01:00Z"}`
	res := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_u","content":"done"}]},"toolUseResult":{"content":"done"},"timestamp":"2026-06-10T10:01:30Z"}`
	e := taskDispatchOf(t, call, res)

	summary, _ := dm(e)["summary"].(string)
	if !utf8.ValidString(summary) {
		t.Errorf("summary is not valid UTF-8: %q", summary)
	}
	if strings.ContainsRune(summary, utf8.RuneError) {
		t.Errorf("summary carries U+FFFD, i.e. a rune was sliced apart: %q", summary)
	}
	// The cap must still bind — a fix that stopped truncating would pass the two
	// assertions above while defeating the point of a preview.
	if !strings.HasSuffix(summary, "...") {
		t.Errorf("summary was not truncated: %q", summary)
	}
	if len(summary) > 100+len("...") {
		t.Errorf("summary is %d bytes, over the 100-byte cap: %q", len(summary), summary)
	}
}

// The Agent call must stop arriving as a generic tool_use, or the backend's
// max-of-channels sees the same spawn twice under two shapes.
func TestAgentToolNoLongerFallsThroughToToolUse(t *testing.T) {
	var kinds []string
	for _, e := range processLines(agentCall, agentResult) {
		kinds = append(kinds, e.Kind)
		if e.Kind == "tool_use" {
			t.Errorf("Agent still emitted a generic tool_use: %+v", dm(e))
		}
	}
	// Guard against the assertion above passing because nothing was emitted at
	// all — the exact way the first draft of this file lied.
	var sawDispatch bool
	for _, k := range kinds {
		if k == "task_dispatch" {
			sawDispatch = true
		}
	}
	if !sawDispatch {
		t.Fatalf("no task_dispatch among %v", kinds)
	}
}
