package normalize

import (
	"path/filepath"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// Real rollout lines captured from codex-cli 0.137.0 (`codex exec`).
var codexRolloutLines = []string{
	`{"timestamp":"2026-06-06T20:38:45.965Z","type":"session_meta","payload":{"id":"019e9ea8-d5a7-7492-89ec-10c105ee33c3","timestamp":"2026-06-06T20:38:45.466Z","cwd":"/tmp/ws","originator":"codex_exec","cli_version":"0.137.0","source":"exec","model_provider":"openai"}}`,
	`{"timestamp":"2026-06-06T20:38:47.624Z","type":"event_msg","payload":{"type":"user_message","message":"Edit target.py: change line two.","images":[]}}`,
	`{"timestamp":"2026-06-06T20:38:50.766Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_A","name":"apply_patch","input":"*** Begin Patch\n*** Update File: target.py\n@@\n-line two\n+line two EDITED\n*** End Patch\n"}}`,
	`{"timestamp":"2026-06-06T20:38:50.783Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call_A","stdout":"Success","stderr":"","success":true,"changes":{"/tmp/ws/target.py":{"type":"update","unified_diff":"@@ -1,3 +1,3 @@\n line one\n-line two\n+line two EDITED\n line three\n","move_path":null}},"status":"completed"}}`,
	`{"timestamp":"2026-06-06T20:38:51.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"echo done\",\"workdir\":\"/tmp/ws\"}","call_id":"call_B"}}`,
	`{"timestamp":"2026-06-06T20:38:51.100Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_B","output":"Chunk ID: a1\nProcess exited with code 0\nOutput:\ndone\n"}}`,
	`{"timestamp":"2026-06-06T20:38:52.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":13451,"cached_input_tokens":13184,"output_tokens":61,"reasoning_output_tokens":0,"total_tokens":13512}}}}`,
	`{"timestamp":"2026-06-06T20:38:52.500Z","type":"event_msg","payload":{"type":"agent_message","message":"commentary here","phase":"commentary"}}`,
	`{"timestamp":"2026-06-06T20:38:53.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Done.","phase":"final_answer"}}`,
}

func TestCodexRolloutNormalization(t *testing.T) {
	p := NewCodexRolloutProcessor("sess-1")
	var events []event.Event
	for _, line := range codexRolloutLines {
		events = append(events, p.Process([]byte(line))...)
	}

	byKind := map[string]int{}
	for _, e := range events {
		byKind[e.Kind]++
		if e.Source != "codex" {
			t.Errorf("event kind=%s has source=%q, want codex", e.Kind, e.Source)
		}
		if e.SessionID != "sess-1" {
			t.Errorf("event kind=%s has sessionId=%q, want sess-1", e.Kind, e.SessionID)
		}
	}

	want := map[string]int{
		"session_start": 1,
		"prompt":        1,
		"file_diff":     1,
		"command":       1,
		"ai_response":   1, // only the final_answer, not commentary
	}
	for kind, n := range want {
		if byKind[kind] != n {
			t.Errorf("kind %s: got %d events, want %d (all kinds: %v)", kind, byKind[kind], n, byKind)
		}
	}

	// Verify the file_diff carried the ready-made unified diff + line counts.
	for _, e := range events {
		if e.Kind != "file_diff" {
			continue
		}
		d := e.Data.(map[string]interface{})
		if d["path"] != "/tmp/ws/target.py" {
			t.Errorf("file_diff path = %v, want /tmp/ws/target.py", d["path"])
		}
		if d["linesAdded"].(int) != 1 || d["linesRemoved"].(int) != 1 {
			t.Errorf("file_diff lines = +%v/-%v, want +1/-1", d["linesAdded"], d["linesRemoved"])
		}
		if d["changeType"] != "update" {
			t.Errorf("file_diff changeType = %v, want update", d["changeType"])
		}
	}

	// Verify the command carried the parsed exit code + stdout.
	for _, e := range events {
		if e.Kind != "command" {
			continue
		}
		d := e.Data.(map[string]interface{})
		if d["command"] != "echo done" {
			t.Errorf("command = %v, want 'echo done'", d["command"])
		}
		if d["exitCode"].(int) != 0 {
			t.Errorf("command exitCode = %v, want 0", d["exitCode"])
		}
	}
}

// idsByKindCodex processes the canonical rollout through a FRESH processor (a
// fresh processor is exactly what a resumed/forked rollout that copied prior
// history, a re-tail after a watcher restart, or a re-read from a mis-advanced
// offset produces) and returns kind -> [event ids].
func idsByKindCodex(t *testing.T, sessionID string) map[string][]string {
	t.Helper()
	p := NewCodexRolloutProcessor(sessionID)
	var events []event.Event
	for _, l := range codexRolloutLines {
		events = append(events, p.Process([]byte(l))...)
	}
	out := map[string][]string{}
	for _, e := range events {
		if e.ID == "" {
			t.Fatalf("event %s has empty id", e.Kind)
		}
		out[e.Kind] = append(out[e.Kind], e.ID)
	}
	return out
}

// TestCodexTranscriptDeterministicEventIDs is the regression guard for the
// rollout double-emit bug (the Codex analogue of the Claude fix in
// pa-arth/promptster-teams-cli#28): the SAME rollout record must never yield two
// distinct event ids, no matter how the rollout is re-observed. Codex
// resume/fork writes a NEW rollout file that copies prior history verbatim, and
// the watcher runs one processor (its own dedup state) per file — so two
// independent processors over identical input model that fork, plus a re-tail
// after a watcher restart and a re-read from an offset wobble. If ids matched
// only within one processor the backend could not dedupe; they must be
// byte-identical across processors. Reverting stableEventID to event.NewUUID()
// (the pre-fix random id) makes this fail — verified locally.
func TestCodexTranscriptDeterministicEventIDs(t *testing.T) {
	first := idsByKindCodex(t, "sess-idem")
	second := idsByKindCodex(t, "sess-idem")

	wantKinds := []string{"session_start", "prompt", "file_diff", "command", "ai_response"}
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
	other := idsByKindCodex(t, "sess-other")
	if other["prompt"][0] == first["prompt"][0] {
		t.Errorf("prompt id collided across sessions: %q", other["prompt"][0])
	}
}

// codexPromptWorkdir feeds a session_meta (carrying cwd) followed by a
// user_message and returns the workdir stamped on the prompt event (nil if
// absent). session_meta is the only rollout line carrying cwd; it precedes
// every prompt.
func codexPromptWorkdir(t *testing.T, cwd string) interface{} {
	t.Helper()
	p := NewCodexRolloutProcessor("sess-wd")
	lines := []string{
		`{"timestamp":"2026-06-06T20:38:45.965Z","type":"session_meta","payload":{"id":"019e9ea8-d5a7-7492-89ec-10c105ee33c3","cwd":"` + cwd + `","originator":"codex_exec","cli_version":"0.137.0","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-06T20:38:47.624Z","type":"event_msg","payload":{"type":"user_message","message":"do the thing","images":[]}}`,
	}
	var prompt *event.Event
	for _, l := range lines {
		for _, e := range p.Process([]byte(l)) {
			if e.Kind == "prompt" {
				ev := e
				prompt = &ev
			}
		}
	}
	if prompt == nil {
		t.Fatalf("no prompt event emitted for cwd %q", cwd)
	}
	return prompt.Data.(map[string]interface{})["workdir"]
}

// TestCodexPromptWorkdirHomeRelative pins the codex workdir emit: an under-home
// cwd collapses to "~/…" on the prompt event.
func TestCodexPromptWorkdirHomeRelative(t *testing.T) {
	home := filepath.Join(t.TempDir(), "user")
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "repos", "foo")
	if got := codexPromptWorkdir(t, cwd); got != "~/repos/foo" {
		t.Errorf("workdir = %v, want ~/repos/foo", got)
	}
}

// TestCodexPromptWorkdirOutsideHome is the privacy guard: an outside-home cwd
// (which may carry the OS username) must produce NO workdir on the prompt.
func TestCodexPromptWorkdirOutsideHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "user")
	t.Setenv("HOME", home)
	if got := codexPromptWorkdir(t, "/mnt/users/alice/repo"); got != nil {
		t.Errorf("workdir = %v, want absent for an outside-home cwd (would leak absolute path)", got)
	}
}

func TestCodexPatchLineRanges(t *testing.T) {
	p := NewCodexRolloutProcessor("sess-1")
	// A multi-hunk unified diff: an update hunk (+3,2), an insertion hunk
	// (+10,3), and a pure-deletion hunk (+20,0 — must be skipped).
	line := `{"timestamp":"2026-06-06T20:38:50.783Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call_A","success":true,"changes":{"/tmp/ws/target.py":{"type":"update","unified_diff":"@@ -3,2 +3,2 @@\n-old\n+new\n line\n@@ -9,0 +10,3 @@\n+a\n+b\n+c\n@@ -20,2 +20,0 @@\n-x\n-y\n"}},"status":"completed"}}`
	var events []event.Event
	events = append(events, p.Process([]byte(line))...)

	var diff *event.Event
	for i := range events {
		if events[i].Kind == "file_diff" {
			diff = &events[i]
		}
	}
	if diff == nil {
		t.Fatalf("no file_diff in %+v", events)
	}
	raw, ok := diff.Data.(map[string]interface{})["lineRanges"].([]interface{})
	if !ok {
		t.Fatalf("lineRanges = %T, want []interface{}", diff.Data.(map[string]interface{})["lineRanges"])
	}
	if len(raw) != 2 {
		t.Fatalf("lineRanges len = %d, want 2 (deletion-only hunk skipped): %+v", len(raw), raw)
	}
	want := []struct{ start, end int }{{3, 4}, {10, 12}}
	for i, r := range raw {
		m := r.(map[string]interface{})
		if m["start"] != want[i].start || m["end"] != want[i].end {
			t.Errorf("range[%d] = {start:%v,end:%v}, want {%d,%d}", i, m["start"], m["end"], want[i].start, want[i].end)
		}
		if m["attribution"] != "likely_ai" {
			t.Errorf("range[%d] attribution = %v, want likely_ai", i, m["attribution"])
		}
	}
}

// TestCodexAttachesModelFromTurnContext (§10.1): the per-turn model id lives on
// the turn_context rollout line — session_meta carries only model_provider (the
// vendor, e.g. "openai"). Capture the latest turn_context model as processor
// state and stamp it on the ai_response so the backend can price the turn against
// the real model. turn_context precedes the turn's agent messages, so the state
// capture is order-correct.
func TestCodexAttachesModelFromTurnContext(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-06-06T20:38:45.965Z","type":"session_meta","payload":{"id":"s","cwd":"/tmp/ws","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-06T20:38:46.000Z","type":"turn_context","payload":{"model":"gpt-5.5","cwd":"/tmp/ws"}}`,
		`{"timestamp":"2026-06-06T20:38:47.000Z","type":"event_msg","payload":{"type":"user_message","message":"go"}}`,
		`{"timestamp":"2026-06-06T20:38:53.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Done.","phase":"final_answer"}}`,
	}
	p := NewCodexRolloutProcessor("sess-m")
	var events []event.Event
	for _, l := range lines {
		events = append(events, p.Process([]byte(l))...)
	}
	var got string
	for _, e := range events {
		if e.Kind == "ai_response" {
			got, _ = e.Data.(map[string]interface{})["model"].(string)
		}
	}
	if got != "gpt-5.5" {
		t.Fatalf("ai_response model = %q, want gpt-5.5 (from turn_context)", got)
	}
}

// TestCodexTurnContextWithoutModelClearsStale (§10.1, honesty guard): a later
// turn_context that declares NO model must not leave the previous turn's model
// attributed to the new turn's ai_response — we omit model rather than price
// against a stale one. (A turn with no turn_context at all still retains the
// last known model; this only covers a turn_context that is present but empty.)
func TestCodexTurnContextWithoutModelClearsStale(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-06-06T20:38:46.000Z","type":"turn_context","payload":{"model":"gpt-5.5"}}`,
		`{"timestamp":"2026-06-06T20:38:47.000Z","type":"turn_context","payload":{"cwd":"/tmp/ws"}}`,
		`{"timestamp":"2026-06-06T20:38:53.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Done.","phase":"final_answer"}}`,
	}
	p := NewCodexRolloutProcessor("sess-clear")
	var events []event.Event
	for _, l := range lines {
		events = append(events, p.Process([]byte(l))...)
	}
	for _, e := range events {
		if e.Kind != "ai_response" {
			continue
		}
		if got, present := e.Data.(map[string]interface{})["model"]; present {
			t.Fatalf("ai_response model = %v, want omitted (later turn_context declared no model)", got)
		}
		return
	}
	t.Fatal("no ai_response event")
}

// TestCodexAiResponseCarriesReasoningTokens (§10.2): the ai_response usage payload
// includes reasoningTokens (OpenAI's reasoning_output_tokens), a content-free
// count the backend uses for reasoning-model pricing. The normalizer already
// extracts it; this pins it onto the ai_response event.
func TestCodexAiResponseCarriesReasoningTokens(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-06-06T20:38:52.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":20,"reasoning_output_tokens":37,"total_tokens":157}}}}`,
		`{"timestamp":"2026-06-06T20:38:53.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Done.","phase":"final_answer"}}`,
	}
	p := NewCodexRolloutProcessor("sess-r")
	var events []event.Event
	for _, l := range lines {
		events = append(events, p.Process([]byte(l))...)
	}
	for _, e := range events {
		if e.Kind != "ai_response" {
			continue
		}
		if got := e.Data.(map[string]interface{})["reasoningTokens"]; got != int64(37) {
			t.Fatalf("ai_response reasoningTokens = %v (%T), want int64(37)", got, got)
		}
		return
	}
	t.Fatal("no ai_response event")
}

// TestCodexReasoningTokensOmittedWhenUnreported (§10.2): a usage payload that
// reports input/output tokens but NO reasoning_output_tokens must OMIT
// reasoningTokens entirely rather than emit 0. Emitting 0 would conflate an
// unreported count with a genuine zero — the same "never fabricate a value for
// something we don't know" invariant the attribution buckets hold.
func TestCodexReasoningTokensOmittedWhenUnreported(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-06-06T20:38:52.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}}}`,
		`{"timestamp":"2026-06-06T20:38:53.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Done.","phase":"final_answer"}}`,
	}
	p := NewCodexRolloutProcessor("sess-nr")
	var events []event.Event
	for _, l := range lines {
		events = append(events, p.Process([]byte(l))...)
	}
	for _, e := range events {
		if e.Kind != "ai_response" {
			continue
		}
		data := e.Data.(map[string]interface{})
		if _, present := data["reasoningTokens"]; present {
			t.Fatalf("reasoningTokens present (%v) when the usage payload reported none; want omitted", data["reasoningTokens"])
		}
		// Ordinary counts still flow.
		if got := data["outputTokens"]; got != int64(20) {
			t.Fatalf("outputTokens = %v, want int64(20)", got)
		}
		return
	}
	t.Fatal("no ai_response event")
}

func TestParseCodexExecOutput(t *testing.T) {
	code, stdout := parseCodexExecOutput("Chunk ID: a1\nProcess exited with code 3\nOutput:\nhello\nworld\n")
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if stdout != "hello\nworld\n" {
		t.Errorf("stdout = %q, want 'hello\\nworld\\n'", stdout)
	}
}

func TestCodexCommandStringForms(t *testing.T) {
	if got := codexCommandString(map[string]interface{}{"cmd": "ls -la"}); got != "ls -la" {
		t.Errorf("cmd form = %q", got)
	}
	if got := codexCommandString(map[string]interface{}{"command": "echo hi"}); got != "echo hi" {
		t.Errorf("command string form = %q", got)
	}
	arr := map[string]interface{}{"command": []interface{}{"bash", "-lc", "echo hi"}}
	if got := codexCommandString(arr); got != "bash -lc echo hi" {
		t.Errorf("command array form = %q", got)
	}
}
