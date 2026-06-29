package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func dm(e Event) map[string]interface{} {
	m, _ := e.Data.(map[string]interface{})
	return m
}

func processAll(t *testing.T, p *claudeTranscriptProcessor, lines ...string) []Event {
	t.Helper()
	var events []Event
	for _, l := range lines {
		events = append(events, p.process([]byte(l))...)
	}
	return events
}

func TestClaudeTranscriptPrompt(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1")
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

func TestClaudeTranscriptSkipsMetaSidechainAndCommandWrappers(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>...</local-command-caveat>"},"timestamp":"2026-06-10T10:00:00Z"}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"summary of earlier work"},"timestamp":"2026-06-10T10:00:01Z"}`,
		`{"type":"user","isSidechain":true,"message":{"role":"user","content":"agent-authored subagent prompt"},"timestamp":"2026-06-10T10:00:02Z"}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"},"timestamp":"2026-06-10T10:00:03Z"}`,
		`{"type":"user","message":{"role":"user","content":"<local-command-stdout>ok</local-command-stdout>"},"timestamp":"2026-06-10T10:00:04Z"}`,
	)
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d: %+v", len(events), events)
	}
}

func TestClaudeTranscriptSlashCommandBecomesToolIntent(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		// Custom command with args → tool_intent, human actor.
		`{"type":"user","message":{"role":"user","content":"<command-name>/deploy-check</command-name>\n<command-message>deploy-check</command-message>\n<command-args>staging --verbose</command-args>"},"timestamp":"2026-06-10T10:00:00Z"}`,
		// Built-in → still dropped.
		`{"type":"user","message":{"role":"user","content":"<command-name>/compact</command-name>"},"timestamp":"2026-06-10T10:00:01Z"}`,
		// Promptster's own explain command → emitted; the WORKER excludes it
		// by inputPreview, so the preview must carry the name.
		`{"type":"user","message":{"role":"user","content":"<command-name>/explain</command-name>\n<command-args>chose retries over backoff</command-args>"},"timestamp":"2026-06-10T10:00:02Z"}`,
	)
	if len(events) != 2 {
		t.Fatalf("expected 2 tool_intent events, got %d: %+v", len(events), events)
	}
	e := events[0]
	if e.Kind != "tool_intent" {
		t.Fatalf("kind = %s", e.Kind)
	}
	if e.Actor == nil || e.Actor.Type != "human" {
		t.Errorf("actor = %+v", e.Actor)
	}
	if dm(e)["toolName"] != "SlashCommand" {
		t.Errorf("toolName = %v", dm(e)["toolName"])
	}
	if dm(e)["inputPreview"] != "/deploy-check staging --verbose" {
		t.Errorf("inputPreview = %v", dm(e)["inputPreview"])
	}
	if dm(events[1])["inputPreview"] != "/explain chose retries over backoff" {
		t.Errorf("explain preview = %v", dm(events[1])["inputPreview"])
	}
}

func TestClaudeTranscriptAssistantAccumulation(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1")
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
	if dm(ar)["inputTokens"] != int64(100) || dm(ar)["cacheWriteTokens"] != int64(300) {
		t.Errorf("tokens = %v / %v", dm(ar)["inputTokens"], dm(ar)["cacheWriteTokens"])
	}
	if dm(ar)["cacheWrite1hTokens"] != int64(300) {
		t.Errorf("cacheWrite1hTokens = %v", dm(ar)["cacheWrite1hTokens"])
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
	p := newClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","message":{"id":"msg-2","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_2","name":"Edit","input":{"file_path":"/tmp/ws/limiter.go","old_string":"n > limit","new_string":"n >= limit"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:01:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"edited"}]},"toolUseResult":{"filePath":"/tmp/ws/limiter.go","structuredPatch":[{"oldStart":10,"oldLines":1,"newStart":10,"newLines":1,"lines":["-if n > limit {","+if n >= limit {"]}],"userModified":false},"timestamp":"2026-06-10T10:01:02Z"}`,
	)
	var diff *Event
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
	p := newClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","message":{"id":"msg-3","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_3","name":"Bash","input":{"command":"go test ./pkg"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:02:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","is_error":true,"content":"FAIL: TestX"}]},"toolUseResult":"FAIL: TestX","timestamp":"2026-06-10T10:02:05Z"}`,
	)
	var cmd *Event
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
	p := newClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","requestId":"req-9","message":{"id":"msg-9","model":"claude-sonnet-4-6","content":[{"type":"text","text":"All done — tests pass."}],"usage":{"input_tokens":40,"output_tokens":20}},"timestamp":"2026-06-10T10:03:00Z"}`,
	)
	if len(events) != 0 {
		t.Fatalf("unexpected immediate flush")
	}
	if got := p.flushStale(time.Minute); len(got) != 0 {
		t.Fatalf("flushed before maxAge: %+v", got)
	}
	p.accum.updatedAt = time.Now().Add(-2 * time.Minute)
	got := p.flushStale(time.Minute)
	if len(got) != 1 || got[0].Kind != "ai_response" {
		t.Fatalf("stale flush failed: %+v", got)
	}
	if dm(got[0])["lastAssistantMessage"] != "All done — tests pass." {
		t.Errorf("text = %v", dm(got[0])["lastAssistantMessage"])
	}
}

func TestClassifyClaudeTranscript(t *testing.T) {
	tmp := t.TempDir()
	cutoff := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	write := func(name string, lines ...string) string {
		p := filepath.Join(tmp, name)
		content := ""
		for _, l := range lines {
			content += l + "\n"
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	ws := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	// Production resolves the workspace through symlinks before matching
	// (macOS /var -> /private/var); mirror that here.
	ws = resolvePath(ws)

	match := write("match.jsonl",
		`{"type":"mode","mode":"normal"}`,
		fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"2026-06-10T10:00:00Z","message":{"content":"hi"}}`, ws),
	)
	if got := classifyClaudeTranscript(match, []string{ws}, cutoff); got != claudeMatchYes {
		t.Errorf("match: got %v", got)
	}

	other := write("other.jsonl",
		fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"2026-06-10T10:00:00Z","message":{"content":"hi"}}`, filepath.Join(tmp, "elsewhere")),
	)
	if got := classifyClaudeTranscript(other, []string{ws}, cutoff); got != claudeMatchNo {
		t.Errorf("other: got %v", got)
	}

	// No cwd yet — file just created, must stay undecided (retry next poll),
	// NOT be cached as a mismatch.
	young := write("young.jsonl", `{"type":"mode","mode":"normal"}`)
	if got := classifyClaudeTranscript(young, []string{ws}, cutoff); got != claudeMatchUndecided {
		t.Errorf("young: got %v", got)
	}

	old := write("old.jsonl",
		fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"2026-06-10T08:00:00Z","message":{"content":"hi"}}`, ws),
	)
	if got := classifyClaudeTranscript(old, []string{ws}, cutoff); got != claudeMatchNo {
		t.Errorf("old session before cutoff: got %v", got)
	}
}

func TestSuppressForTranscriptCapture(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	hookPrompt := Event{Kind: "prompt", Source: "claude-code"}
	lifecycle := Event{Kind: "session_start", Source: "claude-code"}
	cursorPrompt := Event{Kind: "prompt", Source: "cursor"}

	managed := Session{CaptureMode: ""}
	if suppressForTranscriptCapture(managed, &hookPrompt) {
		t.Error("managed mode must never suppress")
	}

	transcript := Session{CaptureMode: "transcript"}
	// No watcher state at all → unhealthy → no suppression, but the takeover
	// marker is recorded so a future watcher skips the backlog.
	if suppressForTranscriptCapture(transcript, &hookPrompt) {
		t.Error("unhealthy watcher must not suppress")
	}
	if _, err := os.Stat(claudeHookTakeoverPath()); err != nil {
		t.Errorf("takeover marker not written: %v", err)
	}

	// Healthy watcher (this process, fresh heartbeat) → suppress prompt but
	// keep lifecycle + non-claude sources.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveClaudeWatcherState(claudeWatcherState{PID: os.Getpid(), StartedAt: now, LastHeartbeat: now}); err != nil {
		t.Fatal(err)
	}
	if !suppressForTranscriptCapture(transcript, &hookPrompt) {
		t.Error("healthy watcher must suppress overlapping kinds")
	}
	if suppressForTranscriptCapture(transcript, &lifecycle) {
		t.Error("lifecycle kinds must never be suppressed")
	}
	if suppressForTranscriptCapture(transcript, &cursorPrompt) {
		t.Error("non-claude sources must never be suppressed")
	}

	// Stale heartbeat → unhealthy again.
	stale := time.Now().Add(-2 * claudeWatcherStaleAfter).UTC().Format(time.RFC3339Nano)
	if err := saveClaudeWatcherState(claudeWatcherState{PID: os.Getpid(), StartedAt: now, LastHeartbeat: stale}); err != nil {
		t.Fatal(err)
	}
	if suppressForTranscriptCapture(transcript, &hookPrompt) {
		t.Error("stale heartbeat must not suppress")
	}

	// Degraded watcher (running, parsing nothing) → unhealthy.
	if err := saveClaudeWatcherState(claudeWatcherState{PID: os.Getpid(), StartedAt: now, LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano), Degraded: true}); err != nil {
		t.Fatal(err)
	}
	if suppressForTranscriptCapture(transcript, &hookPrompt) {
		t.Error("degraded watcher must not suppress")
	}
}

func TestClaudeTranscriptSidechainUsage(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1")
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

func TestClaudeTranscriptUsageOnlyMode(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1")
	p.usageOnly = true // agent-*.jsonl file — lines need not carry isSidechain
	events := processAll(t, p,
		`{"type":"user","message":{"role":"user","content":"agent-authored prompt without sidechain flag"},"timestamp":"2026-06-10T10:06:00Z"}`,
		`{"type":"assistant","message":{"id":"msg-u1","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":9}},"timestamp":"2026-06-10T10:06:01Z"}`,
	)
	if len(events) != 1 || events[0].Kind != "subagent_usage" {
		t.Fatalf("usageOnly mode: expected 1 subagent_usage, got %+v", events)
	}
}

func TestParseClaudeExitCodeFromErrorText(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1")
	events := processAll(t, p,
		`{"type":"assistant","message":{"id":"msg-e1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_e1","name":"Bash","input":{"command":"npm test"}}],"usage":{"input_tokens":1,"output_tokens":1}},"timestamp":"2026-06-10T10:07:00Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_e1","is_error":true,"content":"Error: Exit code 2"}]},"toolUseResult":"Error: Exit code 2","timestamp":"2026-06-10T10:07:05Z"}`,
	)
	var cmd *Event
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

func TestIsClaudeSidechainFile(t *testing.T) {
	if !isClaudeSidechainFile("/x/projects/p/sess-1/subagents/agent-abc.jsonl") {
		t.Error("agent- prefix in subagents dir must be sidechain")
	}
	if !isClaudeSidechainFile("/x/projects/p/sess-1/subagents/deadbeef.jsonl") {
		t.Error("any file under subagents/ must be sidechain")
	}
	if isClaudeSidechainFile("/x/projects/p/2677c24e.jsonl") {
		t.Error("main transcript must not be sidechain")
	}
}

func TestClaudeDegradationStepCatchesMidSessionFailure(t *testing.T) {
	// Healthy stretch: events flowing, counters stay reset.
	degraded, since := claudeDegradationStep(false, 5, 40_000, 0)
	if degraded || since != 0 {
		t.Fatalf("healthy poll: degraded=%v since=%d", degraded, since)
	}
	// Mid-session parser break: bytes accumulate across event-less polls and
	// trip the threshold EVEN THOUGH events were sent earlier in the session
	// (the old eventsSent==0 condition could never fire here).
	for i := 0; i < 3; i++ {
		degraded, since = claudeDegradationStep(degraded, 0, 100_000, since)
	}
	if !degraded {
		t.Fatalf("300KB without a parsed event must degrade (since=%d)", since)
	}
	// Stays degraded on further empty polls.
	if d, _ := claudeDegradationStep(degraded, 0, 10, since); !d {
		t.Error("degraded must persist while nothing parses")
	}
	// Recovery: one parsed event resets everything.
	degraded, since = claudeDegradationStep(degraded, 1, 500, since)
	if degraded || since != 0 {
		t.Errorf("recovery failed: degraded=%v since=%d", degraded, since)
	}
}
