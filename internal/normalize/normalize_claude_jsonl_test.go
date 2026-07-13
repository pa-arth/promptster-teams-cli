package normalize

import (
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

func dm(e event.Event) map[string]interface{} {
	m, _ := e.Data.(map[string]interface{})
	return m
}

func processAll(t *testing.T, p *ClaudeTranscriptProcessor, lines ...string) []event.Event {
	t.Helper()
	var events []event.Event
	for _, l := range lines {
		events = append(events, p.Process([]byte(l))...)
	}
	return events
}

func TestClaudeTranscriptPrompt(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"user","message":{"role":"user","content":"fix the rate limiter off-by-one"},"timestamp":"2026-06-10T10:00:00.000Z","cwd":"/tmp/ws","sessionId":"ide-1","permissionMode":"auto","promptSource":"typed","promptId":"p-1","uuid":"u1"}`,
	)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	if e.Kind != "prompt" {
		t.Fatalf("kind = %s", e.Kind)
	}
	if e.Source != "claude-code" {
		t.Errorf("source = %s", e.Source)
	}
	if e.Actor == nil || e.Actor.Type != "human" {
		t.Errorf("actor = %+v", e.Actor)
	}
	if e.Provenance == nil || e.Provenance.Attribution != "likely_human" || e.Provenance.Methods[0] != "transcript-jsonl" {
		t.Errorf("provenance = %+v", e.Provenance)
	}
	if dm(e)["text"] != "fix the rate limiter off-by-one" {
		t.Errorf("text = %v", dm(e)["text"])
	}
	meta, _ := dm(e)["meta"].(map[string]interface{})
	if meta == nil || meta["ideSessionId"] != "ide-1" || meta["promptSource"] != "typed" {
		t.Errorf("meta = %v", meta)
	}
}

func TestClaudeTranscriptSkipsMetaSidechainAndCommandOutput(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>...</local-command-caveat>"},"timestamp":"2026-06-10T10:00:00Z"}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"summary of earlier work"},"timestamp":"2026-06-10T10:00:01Z"}`,
		`{"type":"user","isSidechain":true,"message":{"role":"user","content":"agent-authored subagent prompt"},"timestamp":"2026-06-10T10:00:02Z"}`,
		`{"type":"user","message":{"role":"user","content":"<local-command-stdout>ok</local-command-stdout>"},"timestamp":"2026-06-10T10:00:04Z"}`,
		// Malformed envelope (no command-name tag) — not a usable invocation.
		`{"type":"user","message":{"role":"user","content":"<command-args>orphaned</command-args>"},"timestamp":"2026-06-10T10:00:05Z"}`,
	)
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d: %+v", len(events), events)
	}
}

func TestClaudeTranscriptSlashCommandBecomesPromptWithCommand(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		// Custom command with args → prompt event carrying the command NAME.
		`{"type":"user","message":{"role":"user","content":"<command-name>/deploy-check</command-name>\n<command-message>deploy-check</command-message>\n<command-args>staging --verbose</command-args>"},"timestamp":"2026-06-10T10:00:00Z"}`,
		// Built-ins are emitted too — the backend keys /clear and /compact
		// reset detection off prompt.command.
		`{"type":"user","message":{"role":"user","content":"<command-name>/compact</command-name>"},"timestamp":"2026-06-10T10:00:01Z"}`,
		// Plain prompts carry no command field.
		`{"type":"user","message":{"role":"user","content":"just a normal prompt"},"timestamp":"2026-06-10T10:00:02Z"}`,
	)
	if len(events) != 3 {
		t.Fatalf("expected 3 prompt events, got %d: %+v", len(events), events)
	}
	e := events[0]
	if e.Kind != "prompt" {
		t.Fatalf("kind = %s", e.Kind)
	}
	if e.Actor == nil || e.Actor.Type != "human" {
		t.Errorf("actor = %+v", e.Actor)
	}
	// Name only — no leading slash, never the args or expanded body.
	if dm(e)["command"] != "deploy-check" {
		t.Errorf("command = %v", dm(e)["command"])
	}
	// text stays as-is (the envelope).
	text, _ := dm(e)["text"].(string)
	if !strings.Contains(text, "<command-name>/deploy-check</command-name>") {
		t.Errorf("text rewritten: %q", text)
	}
	if dm(events[1])["command"] != "compact" {
		t.Errorf("builtin command = %v", dm(events[1])["command"])
	}
	if _, has := dm(events[2])["command"]; has {
		t.Errorf("plain prompt must not carry command: %v", dm(events[2])["command"])
	}
}

func TestLeadingCommandName(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"<command-name>/deploy-check</command-name>\n<command-args>x</command-args>", "deploy-check"},
		{"<command-name>/compact</command-name>", "compact"},
		{"  <command-name>hygiene</command-name>", "hygiene"}, // no slash in marker
		{"<command-name>/ns:cmd</command-name>", "ns:cmd"},
		{"fix the bug in <command-name>/foo</command-name>", ""}, // not leading
		{"<command-args>orphan</command-args>", ""},              // no name tag
		{"<command-name>/unterminated", ""},
		{"plain prompt", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := leadingCommandName(c.text); got != c.want {
			t.Errorf("leadingCommandName(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestClaudeTranscriptCompactBoundary(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","isMeta":false,"timestamp":"2026-06-10T10:09:00Z","compactMetadata":{"trigger":"auto","preTokens":180000}}`,
		`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","timestamp":"2026-06-10T10:10:00Z","compactMetadata":{"trigger":"manual","preTokens":42000}}`,
		// Non-boundary system lines stay silent.
		`{"type":"system","subtype":"other","timestamp":"2026-06-10T10:11:00Z"}`,
	)
	if len(events) != 2 {
		t.Fatalf("expected 2 context_compact events, got %d: %+v", len(events), events)
	}
	if events[0].Kind != "context_compact" || dm(events[0])["trigger"] != "auto" {
		t.Errorf("first = %s trigger=%v", events[0].Kind, dm(events[0])["trigger"])
	}
	if dm(events[1])["trigger"] != "manual" {
		t.Errorf("second trigger = %v", dm(events[1])["trigger"])
	}
	if events[0].Actor == nil || events[0].Actor.Type != "system" {
		t.Errorf("actor = %+v", events[0].Actor)
	}
}

func TestClaudeTranscriptAssistantAccumulation(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	// One API message split across two lines (text + tool_use), same id and
	// identical usage — must produce ONE ai_response with usage counted once.
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-1","message":{"id":"msg-1","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"Let me look at the config."}],"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":2000,"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":300}}},"timestamp":"2026-06-10T10:00:05Z"}`,
		`{"type":"assistant","requestId":"req-1","message":{"id":"msg-1","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}}],"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":2000,"cache_creation_input_tokens":300}},"timestamp":"2026-06-10T10:00:05Z"}`,
	)
	if len(events) != 0 {
		t.Fatalf("no flush expected before a boundary, got %d events", len(events))
	}
	// Tool result arrives on a user line — flushes the message AND resolves
	// the Bash call.
	events = processAll(t, p,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},"toolUseResult":{"stdout":"ok\n","stderr":"","interrupted":false},"timestamp":"2026-06-10T10:00:09Z"}`,
	)
	if len(events) != 2 {
		t.Fatalf("expected ai_response + command, got %d: %+v", len(events), events)
	}
	ar := events[0]
	if ar.Kind != "ai_response" {
		t.Fatalf("first event kind = %s", ar.Kind)
	}
	if dm(ar)["lastAssistantMessage"] != "Let me look at the config." {
		t.Errorf("text = %v", dm(ar)["lastAssistantMessage"])
	}
	if dm(ar)["model"] != "claude-sonnet-4-6" || dm(ar)["usageScope"] != "request" {
		t.Errorf("model/usageScope = %v / %v", dm(ar)["model"], dm(ar)["usageScope"])
	}
	// The exact camelCase field names are the ingest contract — the teams
	// projector drops anything else.
	if dm(ar)["inputTokens"] != int64(100) || dm(ar)["outputTokens"] != int64(50) {
		t.Errorf("input/output = %v / %v", dm(ar)["inputTokens"], dm(ar)["outputTokens"])
	}
	if dm(ar)["cacheReadTokens"] != int64(2000) || dm(ar)["cacheWriteTokens"] != int64(300) {
		t.Errorf("cacheRead/cacheWrite = %v / %v", dm(ar)["cacheReadTokens"], dm(ar)["cacheWriteTokens"])
	}
	if dm(ar)["cacheWrite5mTokens"] != int64(0) || dm(ar)["cacheWrite1hTokens"] != int64(300) {
		t.Errorf("cacheWrite5m/1h = %v / %v", dm(ar)["cacheWrite5mTokens"], dm(ar)["cacheWrite1hTokens"])
	}
	// Assistant text rides under `text` (the field the teams projector keeps)
	// as well as the legacy lastAssistantMessage.
	if dm(ar)["text"] != "Let me look at the config." {
		t.Errorf("text = %v", dm(ar)["text"])
	}
	cmdEv := events[1]
	if cmdEv.Kind != "command" {
		t.Fatalf("second event kind = %s", cmdEv.Kind)
	}
	if dm(cmdEv)["command"] != "go test ./..." {
		t.Errorf("command = %v", dm(cmdEv)["command"])
	}
	if dm(cmdEv)["exitCode"] != 0 {
		t.Errorf("exitCode = %v", dm(cmdEv)["exitCode"])
	}
	if cmdEv.Provenance == nil || cmdEv.Provenance.Methods[0] != "transcript-jsonl" {
		t.Errorf("command provenance = %+v", cmdEv.Provenance)
	}

	// A third line with the SAME message id after flush must not re-emit usage.
	events = processAll(t, p,
		`{"type":"assistant","requestId":"req-1","message":{"id":"msg-1","model":"claude-sonnet-4-6","content":[{"type":"text","text":"straggler"}],"usage":{"input_tokens":100,"output_tokens":50}},"timestamp":"2026-06-10T10:00:10Z"}`,
		`{"type":"mode","mode":"normal"}`,
	)
	if len(events) != 0 {
		t.Fatalf("duplicate message id re-emitted: %+v", events)
	}
}

func TestClaudeTranscriptEditToolResult(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","message":{"id":"msg-2","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_2","name":"Edit","input":{"file_path":"/tmp/ws/limiter.go","old_string":"n > limit","new_string":"n >= limit"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:01:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"edited"}]},"toolUseResult":{"filePath":"/tmp/ws/limiter.go","structuredPatch":[{"oldStart":10,"oldLines":1,"newStart":10,"newLines":1,"lines":["-if n > limit {","+if n >= limit {"]}],"userModified":false},"timestamp":"2026-06-10T10:01:02Z"}`,
	)
	var diff *event.Event
	for i := range events {
		if events[i].Kind == "file_diff" {
			diff = &events[i]
		}
	}
	if diff == nil {
		t.Fatalf("no file_diff in %+v", events)
	}
	if dm(*diff)["path"] != "/tmp/ws/limiter.go" {
		t.Errorf("path = %v", dm(*diff)["path"])
	}
	d, _ := dm(*diff)["diff"].(string)
	if d == "" || dm(*diff)["linesAdded"] != 1 || dm(*diff)["linesRemoved"] != 1 {
		t.Errorf("diff/lines = %q %v %v", d, dm(*diff)["linesAdded"], dm(*diff)["linesRemoved"])
	}
	if dm(*diff)["attribution"] != "likely_ai" {
		t.Errorf("attribution = %v", dm(*diff)["attribution"])
	}
}

func TestClaudeTranscriptErrorToolResult(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","message":{"id":"msg-3","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_3","name":"Bash","input":{"command":"go test ./pkg"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:02:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","is_error":true,"content":"FAIL: TestX"}]},"toolUseResult":"FAIL: TestX","timestamp":"2026-06-10T10:02:05Z"}`,
	)
	var cmd *event.Event
	for i := range events {
		if events[i].Kind == "command" {
			cmd = &events[i]
		}
	}
	if cmd == nil {
		t.Fatalf("no command in %+v", events)
	}
	if dm(*cmd)["exitCode"] != 1 {
		t.Errorf("exitCode = %v (failed command must read as failure)", dm(*cmd)["exitCode"])
	}
}

func TestClaudeTranscriptFlushStale(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-9","message":{"id":"msg-9","model":"claude-sonnet-4-6","content":[{"type":"text","text":"All done — tests pass."}],"usage":{"input_tokens":40,"output_tokens":20}},"timestamp":"2026-06-10T10:03:00Z"}`,
	)
	if len(events) != 0 {
		t.Fatalf("unexpected immediate flush")
	}
	if got := p.FlushStale(time.Minute); len(got) != 0 {
		t.Fatalf("flushed before maxAge: %+v", got)
	}
	p.accum.updatedAt = time.Now().Add(-2 * time.Minute)
	got := p.FlushStale(time.Minute)
	if len(got) != 1 || got[0].Kind != "ai_response" {
		t.Fatalf("stale flush failed: %+v", got)
	}
	if dm(got[0])["lastAssistantMessage"] != "All done — tests pass." {
		t.Errorf("text = %v", dm(got[0])["lastAssistantMessage"])
	}
}

func TestClaudeTranscriptSidechainUsage(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	// Inline sidechain assistant line: usage extracted as subagent_usage, no
	// ai_response, no text.
	events := processAll(t, p,
		`{"type":"assistant","isSidechain":true,"requestId":"req-s1","message":{"id":"msg-s1","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"subagent narration"}],"usage":{"input_tokens":3,"output_tokens":7,"cache_read_input_tokens":0,"cache_creation_input_tokens":10002}},"timestamp":"2026-06-10T10:05:00Z"}`,
		// Same message id on a second line — usage must not double-count.
		`{"type":"assistant","isSidechain":true,"requestId":"req-s1","message":{"id":"msg-s1","model":"claude-haiku-4-5-20251001","content":[{"type":"tool_use","id":"toolu_s1","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":3,"output_tokens":7,"cache_read_input_tokens":0,"cache_creation_input_tokens":10002}},"timestamp":"2026-06-10T10:05:01Z"}`,
		// Sidechain user prompt: agent-authored, must emit NOTHING.
		`{"type":"user","isSidechain":true,"message":{"role":"user","content":"agent-authored prompt"},"timestamp":"2026-06-10T10:05:02Z"}`,
	)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 subagent_usage, got %d: %+v", len(events), events)
	}
	e := events[0]
	if e.Kind != "subagent_usage" {
		t.Fatalf("kind = %s", e.Kind)
	}
	if dm(e)["sidechain"] != true || dm(e)["usageScope"] != "request" {
		t.Errorf("sidechain/usageScope = %v / %v", dm(e)["sidechain"], dm(e)["usageScope"])
	}
	if dm(e)["model"] != "claude-haiku-4-5-20251001" || dm(e)["cacheWriteTokens"] != int64(10002) {
		t.Errorf("model/cacheWrite = %v / %v", dm(e)["model"], dm(e)["cacheWriteTokens"])
	}
	if _, has := dm(e)["lastAssistantMessage"]; has {
		t.Error("sidechain usage must not carry assistant text")
	}
}

func TestClaudeTranscriptSidechainAttribution(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	p.UsageOnly = true
	// Filename-derived agent id is the floor — rows may repeat it later.
	p.AgentID = "ab276e3606abc0ce2"
	events := processAll(t, p,
		// First user line carries agentId + attributionSkill (skill-spawned
		// sidechain), no usage.
		`{"type":"user","isSidechain":true,"agentId":"ab276e3606abc0ce2","attributionSkill":"commit-push-pr","message":{"role":"user","content":"agent task"},"timestamp":"2026-06-10T10:05:00Z"}`,
		// Assistant line carries attributionAgent (agent type) + usage.
		`{"type":"assistant","isSidechain":true,"agentId":"ab276e3606abc0ce2","attributionAgent":"general-purpose","requestId":"req-a1","message":{"id":"msg-a1","model":"claude-sonnet-4-6","content":[{"type":"text","text":"working"}],"usage":{"input_tokens":11,"output_tokens":22,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}},"timestamp":"2026-06-10T10:05:01Z"}`,
		// Second request in the same sidechain inherits the pinned attribution.
		`{"type":"assistant","isSidechain":true,"requestId":"req-a2","message":{"id":"msg-a2","model":"claude-sonnet-4-6","content":[{"type":"text","text":"more"}],"usage":{"input_tokens":5,"output_tokens":6}},"timestamp":"2026-06-10T10:05:02Z"}`,
	)
	if len(events) != 2 {
		t.Fatalf("expected 2 subagent_usage events, got %d: %+v", len(events), events)
	}
	for i, e := range events {
		if e.Kind != "subagent_usage" {
			t.Fatalf("event %d kind = %s", i, e.Kind)
		}
		if dm(e)["agentId"] != "ab276e3606abc0ce2" {
			t.Errorf("event %d agentId = %v", i, dm(e)["agentId"])
		}
		if dm(e)["attributionSkill"] != "commit-push-pr" {
			t.Errorf("event %d attributionSkill = %v", i, dm(e)["attributionSkill"])
		}
		if dm(e)["attributionAgent"] != "general-purpose" {
			t.Errorf("event %d attributionAgent = %v", i, dm(e)["attributionAgent"])
		}
	}
}

func TestClaudeTranscriptSkillToolCarriesSkillName(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","message":{"id":"msg-sk1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_sk1","name":"Skill","input":{"skill":"commit-push-pr","args":"--no-verify"}}],"usage":{"input_tokens":1,"output_tokens":1}},"timestamp":"2026-06-10T10:08:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_sk1","content":"ok"}]},"toolUseResult":{"content":"ok"},"timestamp":"2026-06-10T10:08:05Z"}`,
	)
	var tu *event.Event
	for i := range events {
		if events[i].Kind == "tool_use" {
			tu = &events[i]
		}
	}
	if tu == nil {
		t.Fatalf("no tool_use in %+v", events)
	}
	if dm(*tu)["skill"] != "commit-push-pr" {
		t.Errorf("skill = %v", dm(*tu)["skill"])
	}
	if dm(*tu)["tool"] != "Skill" {
		t.Errorf("tool = %v", dm(*tu)["tool"])
	}
}

func TestSkillNameFromInput(t *testing.T) {
	cases := []struct {
		input map[string]interface{}
		want  string
	}{
		{map[string]interface{}{"skill": "commit-push-pr", "args": "x"}, "commit-push-pr"},
		{map[string]interface{}{"skill": "/hygiene"}, "hygiene"},
		{map[string]interface{}{"command": "/deploy-check staging"}, "deploy-check"},
		{map[string]interface{}{"args": "no name"}, ""},
		{map[string]interface{}{}, ""},
	}
	for _, c := range cases {
		if got := skillNameFromInput(c.input); got != c.want {
			t.Errorf("skillNameFromInput(%v) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestClaudeTranscriptUsageOnlyMode(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	p.UsageOnly = true // agent-*.jsonl file — lines need not carry isSidechain
	events := processAll(t, p,
		`{"type":"user","message":{"role":"user","content":"agent-authored prompt without sidechain flag"},"timestamp":"2026-06-10T10:06:00Z"}`,
		`{"type":"assistant","message":{"id":"msg-u1","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":9}},"timestamp":"2026-06-10T10:06:01Z"}`,
	)
	if len(events) != 1 || events[0].Kind != "subagent_usage" {
		t.Fatalf("UsageOnly mode: expected 1 subagent_usage, got %+v", events)
	}
}

// idempotencyLines is a self-contained turn that exercises every watcher-owned
// kind that was observed duplicated in the wild (prompt, ai_response, command)
// plus context_compact. Each line carries the stable identity Claude Code
// writes (line `uuid`, message.id, tool_use_id) that the fix keys event ids off.
var idempotencyLines = []string{
	`{"type":"user","message":{"role":"user","content":"add retry to the ingest client"},"timestamp":"2026-06-10T09:00:00.000Z","cwd":"/tmp/ws","sessionId":"ide-9","uuid":"line-prompt-1"}`,
	`{"type":"assistant","requestId":"req-9","message":{"id":"msg-9","model":"claude-sonnet-4-6","content":[{"type":"text","text":"On it."},{"type":"tool_use","id":"toolu_9","name":"Bash","input":{"command":"go build ./..."}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T09:00:05.000Z","uuid":"line-asst-1"}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","content":"ok"}]},"toolUseResult":{"stdout":"ok\n","stderr":"","interrupted":false},"timestamp":"2026-06-10T09:00:09.000Z","uuid":"line-toolres-1"}`,
	`{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto"},"timestamp":"2026-06-10T09:00:12.000Z","uuid":"line-compact-1"}`,
}

// idsByKind processes the canonical turn through a FRESH processor (a fresh
// processor is exactly what a resumed/forked transcript, a re-tail after a
// watcher restart, or re-reading a mis-advanced offset produces) and returns
// kind -> [event ids]. FlushStale drains the final accumulated message.
func idsByKind(t *testing.T, sessionID string) map[string][]string {
	t.Helper()
	p := NewClaudeTranscriptProcessor(sessionID)
	events := processAll(t, p, idempotencyLines...)
	events = append(events, p.FlushStale(0)...)
	out := map[string][]string{}
	for _, e := range events {
		if e.ID == "" {
			t.Fatalf("event %s has empty id", e.Kind)
		}
		out[e.Kind] = append(out[e.Kind], e.ID)
	}
	return out
}

// TestClaudeTranscriptDeterministicEventIDs is the regression guard for the
// timeline double-emit bug: the SAME source line/message must never yield two
// distinct event ids, no matter how the transcript is re-observed. Two
// independent processors over identical input model all three duplicate
// mechanisms at once — a forked/resumed transcript that copied prior history
// (two files, two processors), a re-tail after a watcher restart, and a re-read
// from an offset wobble. If ids matched only within one processor the backend
// could not dedupe; they must be byte-identical across processors.
func TestClaudeTranscriptDeterministicEventIDs(t *testing.T) {
	first := idsByKind(t, "sess-idem")
	second := idsByKind(t, "sess-idem")

	wantKinds := []string{"prompt", "ai_response", "command", "context_compact"}
	for _, k := range wantKinds {
		ids := first[k]
		if len(ids) != 1 {
			t.Fatalf("expected exactly one %s event, got %d (%v)", k, len(ids), ids)
		}
		if len(second[k]) != 1 {
			t.Fatalf("expected exactly one %s event in second run, got %d (%v)", k, len(second[k]), second[k])
		}
		if second[k][0] != ids[0] {
			t.Errorf("%s id not idempotent across re-observation: %q (first) vs %q (second)",
				k, ids[0], second[k][0])
		}
	}

	// Every emitted id must be distinct across kinds — determinism must not
	// collapse different logical events onto the same id.
	seen := map[string]string{}
	for kind, ids := range first {
		for _, id := range ids {
			if prev, dup := seen[id]; dup {
				t.Errorf("id %q reused across kinds %s and %s", id, prev, kind)
			}
			seen[id] = kind
		}
	}

	// A different session must NOT collide with this one — ids are session-scoped.
	other := idsByKind(t, "sess-other")
	if other["prompt"][0] == first["prompt"][0] {
		t.Errorf("prompt id collided across sessions: %q", other["prompt"][0])
	}
}

// TestDeterministicUUIDStableAndVersioned pins the primitive the fix relies on:
// same input -> same valid v5 UUID, different input -> different id.
func TestDeterministicUUIDStableAndVersioned(t *testing.T) {
	a := event.DeterministicUUID("sess-1\x1fprompt\x1fline-1")
	if a != event.DeterministicUUID("sess-1\x1fprompt\x1fline-1") {
		t.Fatalf("DeterministicUUID not stable for identical input")
	}
	if a == event.DeterministicUUID("sess-1\x1fprompt\x1fline-2") {
		t.Fatalf("DeterministicUUID collided for distinct input")
	}
	if len(a) != 36 || a[14] != '5' {
		t.Fatalf("not a v5 UUID: %q", a)
	}
}

func TestParseClaudeExitCodeFromErrorText(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","message":{"id":"msg-e1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_e1","name":"Bash","input":{"command":"npm test"}}],"usage":{"input_tokens":1,"output_tokens":1}},"timestamp":"2026-06-10T10:07:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_e1","is_error":true,"content":"Error: Exit code 2"}]},"toolUseResult":"Error: Exit code 2","timestamp":"2026-06-10T10:07:05Z"}`,
	)
	var cmd *event.Event
	for i := range events {
		if events[i].Kind == "command" {
			cmd = &events[i]
		}
	}
	if cmd == nil {
		t.Fatalf("no command in %+v", events)
	}
	if dm(*cmd)["exitCode"] != 2 {
		t.Errorf("exitCode = %v, want the parsed 2 (not the is_error fallback 1)", dm(*cmd)["exitCode"])
	}
}

func TestClaudeTranscriptInterruptActionNotPrompt(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	// Assistant fires a Bash tool_use, then the developer hits ESC — Claude Code
	// writes a plain-string user line. It must become ONE interrupt event
	// (subtype=action, cutTool from the preceding record), NOT a prompt.
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-i1","message":{"id":"msg-i1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_i1","name":"Bash","input":{"command":"rm -rf /tmp/scratch"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:10:00Z"}`,
		`{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-06-10T10:10:03Z","uuid":"line-int-1"}`,
	)
	var interrupt *event.Event
	for i := range events {
		if events[i].Kind == "prompt" {
			t.Fatalf("interrupt sentinel must NOT become a prompt: %+v", events[i])
		}
		if events[i].Kind == "interrupt" {
			interrupt = &events[i]
		}
	}
	if interrupt == nil {
		t.Fatalf("expected an interrupt event, got %+v", events)
	}
	if interrupt.Source != "claude-code" {
		t.Errorf("source = %s", interrupt.Source)
	}
	if interrupt.Actor == nil || interrupt.Actor.Type != "human" {
		t.Errorf("actor = %+v", interrupt.Actor)
	}
	if interrupt.Provenance == nil || interrupt.Provenance.Methods[0] != "transcript-jsonl" {
		t.Errorf("provenance = %+v", interrupt.Provenance)
	}
	d := dm(*interrupt)
	if d["subtype"] != "action" {
		t.Errorf("subtype = %v, want action", d["subtype"])
	}
	if d["cutTool"] != "Bash" {
		t.Errorf("cutTool = %v", d["cutTool"])
	}
	if d["variant"] != "generation" {
		t.Errorf("variant = %v, want generation (the plain sentinel)", d["variant"])
	}
	// SOURCE-EXCLUSION REGRESSION GUARD: the teams CLI must NEVER emit a
	// cutToolInput (a Bash command body / file path is source-adjacent). The
	// hiring CLI carries it; teams deliberately omits it.
	if _, has := d["cutToolInput"]; has {
		t.Errorf("teams interrupt must NOT carry cutToolInput (source exclusion): %v", d["cutToolInput"])
	}
}

func TestClaudeTranscriptInterruptGenerationSubtype(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	// Assistant emits only text (no tool_use), then interrupted → subtype
	// generation, no cutTool.
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-g1","message":{"id":"msg-g1","model":"claude-sonnet-4-6","content":[{"type":"text","text":"Here is a long explanation that got cut off"}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:11:00Z"}`,
		`{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-06-10T10:11:02Z","uuid":"line-int-g1"}`,
	)
	var interrupt, aiResp *event.Event
	for i := range events {
		switch events[i].Kind {
		case "interrupt":
			interrupt = &events[i]
		case "ai_response":
			aiResp = &events[i]
		}
	}
	if aiResp == nil {
		t.Errorf("the interrupted assistant text should still flush as ai_response")
	}
	if interrupt == nil {
		t.Fatalf("expected interrupt, got %+v", events)
	}
	d := dm(*interrupt)
	if d["subtype"] != "generation" {
		t.Errorf("subtype = %v, want generation", d["subtype"])
	}
	if _, has := d["cutTool"]; has {
		t.Errorf("generation interrupt must not carry cutTool: %v", d["cutTool"])
	}
	if _, has := d["cutToolInput"]; has {
		t.Errorf("teams interrupt must NOT carry cutToolInput (source exclusion)")
	}
}

func TestClaudeTranscriptInterruptForToolUseVariant(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	// The "...for tool use" sentinel arriving inside a content array.
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-t1","message":{"id":"msg-t1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_t1","name":"Edit","input":{"file_path":"/tmp/ws/app.go","old_string":"a","new_string":"b"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:12:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]},"timestamp":"2026-06-10T10:12:01Z","uuid":"line-int-t1"}`,
	)
	var interrupt *event.Event
	for i := range events {
		if events[i].Kind == "prompt" {
			t.Fatalf("array-form sentinel must NOT become a prompt: %+v", events[i])
		}
		if events[i].Kind == "interrupt" {
			interrupt = &events[i]
		}
	}
	if interrupt == nil {
		t.Fatalf("expected interrupt, got %+v", events)
	}
	d := dm(*interrupt)
	if d["variant"] != "tool_use" {
		t.Errorf("variant = %v, want tool_use", d["variant"])
	}
	if d["subtype"] != "action" || d["cutTool"] != "Edit" {
		t.Errorf("subtype/cutTool = %v / %v", d["subtype"], d["cutTool"])
	}
	// The cut tool's file_path must NOT leak as cutToolInput (source exclusion).
	if _, has := d["cutToolInput"]; has {
		t.Errorf("teams interrupt must NOT carry cutToolInput (would leak the file_path)")
	}
}

func TestClaudeTranscriptInterruptRedirectLinkage(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-r1","message":{"id":"msg-r1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_r1","name":"Bash","input":{"command":"sleep 999"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:13:00Z"}`,
		`{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-06-10T10:13:02Z","uuid":"line-int-r1"}`,
		`{"type":"user","message":{"role":"user","content":"actually run the tests instead"},"timestamp":"2026-06-10T10:13:05Z","uuid":"line-redir-r1"}`,
	)
	var interrupt, prompt *event.Event
	for i := range events {
		switch events[i].Kind {
		case "interrupt":
			interrupt = &events[i]
		case "prompt":
			prompt = &events[i]
		}
	}
	if interrupt == nil || prompt == nil {
		t.Fatalf("expected an interrupt AND a redirect prompt, got %+v", events)
	}
	if dm(*prompt)["followsInterrupt"] != true {
		t.Errorf("redirect prompt missing followsInterrupt: %v", dm(*prompt))
	}
	if len(prompt.RelatedEventIDs) != 1 || prompt.RelatedEventIDs[0] != interrupt.ID {
		t.Errorf("relatedEventIds = %v, want [%s]", prompt.RelatedEventIDs, interrupt.ID)
	}
}

func TestClaudeTranscriptConsecutiveInterruptsCollapse(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	// ESC ESC in a burst → exactly one interrupt event; the redirect still
	// links to that single interrupt.
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-c1","message":{"id":"msg-c1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_c1","name":"Bash","input":{"command":"top"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:14:00Z"}`,
		`{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-06-10T10:14:01Z","uuid":"line-int-c1"}`,
		`{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-06-10T10:14:01Z","uuid":"line-int-c2"}`,
		`{"type":"user","message":{"role":"user","content":"stop and summarize"},"timestamp":"2026-06-10T10:14:03Z","uuid":"line-redir-c1"}`,
	)
	interrupts := 0
	var interrupt, prompt *event.Event
	for i := range events {
		switch events[i].Kind {
		case "interrupt":
			interrupts++
			interrupt = &events[i]
		case "prompt":
			prompt = &events[i]
		}
	}
	if interrupts != 1 {
		t.Fatalf("ESC ESC burst must collapse to 1 interrupt, got %d", interrupts)
	}
	if prompt == nil || interrupt == nil {
		t.Fatalf("expected interrupt + prompt, got %+v", events)
	}
	if len(prompt.RelatedEventIDs) != 1 || prompt.RelatedEventIDs[0] != interrupt.ID {
		t.Errorf("redirect must link the single interrupt: %v", prompt.RelatedEventIDs)
	}
}

func TestClaudeTranscriptInterruptAbortNoRedirect(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	// Interrupt followed by a NEW assistant record (no redirect prompt) → the
	// pending link is cleared; a later prompt does NOT back-link.
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-a1","message":{"id":"msg-a1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_a1","name":"Bash","input":{"command":"make"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:15:00Z"}`,
		`{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-06-10T10:15:01Z","uuid":"line-int-a1"}`,
		`{"type":"assistant","requestId":"req-a2","message":{"id":"msg-a2","model":"claude-sonnet-4-6","content":[{"type":"text","text":"resuming on my own"}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:15:02Z"}`,
		`{"type":"user","message":{"role":"user","content":"unrelated follow-up"},"timestamp":"2026-06-10T10:15:06Z","uuid":"line-prompt-a1"}`,
	)
	var prompt *event.Event
	for i := range events {
		if events[i].Kind == "prompt" {
			prompt = &events[i]
		}
	}
	if prompt == nil {
		t.Fatalf("expected a prompt, got %+v", events)
	}
	if _, has := dm(*prompt)["followsInterrupt"]; has {
		t.Errorf("prompt after an intervening assistant must NOT follow the interrupt")
	}
	if len(prompt.RelatedEventIDs) != 0 {
		t.Errorf("relatedEventIds = %v, want empty", prompt.RelatedEventIDs)
	}
}

func TestClaudeTranscriptChunkedToolThenTextInterruptIsGeneration(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	// One API message chunked across two records sharing message.id: a tool_use
	// record THEN a text-only record. The trailing text record is the most recent
	// content, so an interrupt after it must classify as "generation" with NO
	// stale cutTool — not "action" carried over from the earlier tool_use record.
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-ch1","message":{"id":"msg-ch1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_ch1","name":"Bash","input":{"command":"npm run build"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:20:00Z"}`,
		`{"type":"assistant","requestId":"req-ch1","message":{"id":"msg-ch1","model":"claude-sonnet-4-6","content":[{"type":"text","text":"now let me explain the plan before running anything"}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:20:01Z"}`,
		`{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-06-10T10:20:03Z","uuid":"line-int-ch1"}`,
	)
	var interrupt *event.Event
	for i := range events {
		if events[i].Kind == "prompt" {
			t.Fatalf("interrupt sentinel must NOT become a prompt: %+v", events[i])
		}
		if events[i].Kind == "interrupt" {
			interrupt = &events[i]
		}
	}
	if interrupt == nil {
		t.Fatalf("expected an interrupt event, got %+v", events)
	}
	d := dm(*interrupt)
	if d["subtype"] != "generation" {
		t.Errorf("subtype = %v, want generation (text record cleared the tool state)", d["subtype"])
	}
	if _, has := d["cutTool"]; has {
		t.Errorf("stale cutTool leaked from the earlier tool_use record: %v", d["cutTool"])
	}
}

func TestClaudeTranscriptSentinelToolResultEmitsOnlyInterrupt(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	// A cut tool call: the synthetic user record's content array carries the
	// sentinel in a tool_result block AND a sibling text block. Exactly ONE
	// interrupt must be emitted, and NO prompt (the sibling text is not a real
	// prompt, and must not be back-linked as a redirect either).
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-sr1","message":{"id":"msg-sr1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_sr1","name":"Bash","input":{"command":"sleep 999"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:21:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_sr1","content":"[Request interrupted by user for tool use]"},{"type":"text","text":"stray sibling text"}]},"timestamp":"2026-06-10T10:21:01Z","uuid":"line-int-sr1"}`,
	)
	interrupts := 0
	var interrupt *event.Event
	for i := range events {
		if events[i].Kind == "prompt" {
			t.Fatalf("sentinel tool_result must NOT also emit a prompt: %+v", events[i])
		}
		if events[i].Kind == "interrupt" {
			interrupts++
			interrupt = &events[i]
		}
	}
	if interrupts != 1 {
		t.Fatalf("expected exactly 1 interrupt, got %d: %+v", interrupts, events)
	}
	d := dm(*interrupt)
	if d["variant"] != "tool_use" {
		t.Errorf("variant = %v, want tool_use", d["variant"])
	}
	if d["subtype"] != "action" || d["cutTool"] != "Bash" {
		t.Errorf("subtype/cutTool = %v / %v", d["subtype"], d["cutTool"])
	}
}

func TestToolResultTextArrayContent(t *testing.T) {
	// Array-shaped tool_result.content carrying the sentinel must be detected.
	block := map[string]interface{}{
		"type":        "tool_result",
		"tool_use_id": "toolu_arr1",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "[Request interrupted by user for tool use]"},
		},
	}
	got := toolResultText(block)
	variant, ok := interruptVariant(got)
	if !ok {
		t.Fatalf("array-shaped tool_result content not detected: %q", got)
	}
	if variant != "tool_use" {
		t.Errorf("variant = %v, want tool_use", variant)
	}
	// Multiple text blocks concatenate with newlines; non-text blocks are ignored.
	multi := map[string]interface{}{
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "line one"},
			map[string]interface{}{"type": "image", "source": "..."},
			map[string]interface{}{"type": "text", "text": "line two"},
		},
	}
	if got := toolResultText(multi); got != "line one\nline two" {
		t.Errorf("concatenation = %q", got)
	}
	// End-to-end through handleUser: array tool_result sentinel yields one
	// interrupt and no prompt.
	p := NewClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-arr","message":{"id":"msg-arr","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_arr2","name":"Edit","input":{"file_path":"/tmp/ws/x.go"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:22:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_arr2","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}]},"timestamp":"2026-06-10T10:22:01Z","uuid":"line-int-arr1"}`,
	)
	interrupts := 0
	for i := range events {
		if events[i].Kind == "prompt" {
			t.Fatalf("array sentinel must NOT become a prompt: %+v", events[i])
		}
		if events[i].Kind == "interrupt" {
			interrupts++
		}
	}
	if interrupts != 1 {
		t.Fatalf("expected 1 interrupt from array-shaped sentinel, got %d: %+v", interrupts, events)
	}
}
