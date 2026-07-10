package normalize

import (
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
