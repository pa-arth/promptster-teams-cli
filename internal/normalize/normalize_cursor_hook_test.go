package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// hookPayload builds a payload with the envelope fields every real Cursor hook
// carries, so a test only states what it is actually about.
func hookPayload(step string, extra map[string]interface{}) []byte {
	p := map[string]interface{}{
		"hook_event_name": step,
		"conversation_id": "conv-1",
		"session_id":      "conv-1",
		"generation_id":   "gen-1",
		"cursor_version":  "2026.07.23-e383d2b",
		"user_email":      "engineer@example.com",
		"workspace_roots": []string{"/repo"},
		"transcript_path": "/home/u/.cursor/projects/p/agent-transcripts/conv-1/conv-1.jsonl",
		"model":           "default",
	}
	for k, v := range extra {
		p[k] = v
	}
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return b
}

// --- the privacy boundary ----------------------------------------------------

// afterFileEdit delivers every edit's old_string and new_string. The hiring CLI
// synthesizes a unified diff from exactly these two fields; that is correct
// there and forbidden here. Only counts may leave.
func TestCursorHookFileEdit_CountsOnlyNeverCode(t *testing.T) {
	secret := "const STRIPE_KEY = \"sk_live_should_never_ship\""
	raw := hookPayload("afterFileEdit", map[string]interface{}{
		"file_path": "billing.go",
		"edits": []map[string]string{
			{"old_string": "old line one\nold line two", "new_string": secret + "\nb\nc"},
		},
	})

	res, ok := NormalizeCursorHook(raw)
	if !ok {
		t.Fatal("afterFileEdit produced no events")
	}
	e, found := firstOfKind(res.Events, "file_diff")
	if !found {
		t.Fatal("no file_diff emitted")
	}
	if dataOf(t, e)["path"] != "billing.go" {
		t.Fatalf("path = %v", dataOf(t, e)["path"])
	}
	if dataOf(t, e)["linesAdded"] != 3 || dataOf(t, e)["linesRemoved"] != 2 {
		t.Fatalf("counts = +%v/-%v, want +3/-2", dataOf(t, e)["linesAdded"], dataOf(t, e)["linesRemoved"])
	}

	// The whole event, serialized, must not contain a byte of the edit.
	blob, err := json.Marshal(res.Events)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{secret, "sk_live", "old line one", "old_string", "new_string"} {
		if strings.Contains(string(blob), banned) {
			t.Fatalf("event payload leaked %q:\n%s", banned, blob)
		}
	}
	if e.RawPayload != "" {
		t.Fatal("RawPayload must never be populated — it would carry the edit bodies verbatim")
	}
}

// Every hook payload carries the engineer's email address. For a CLI that sells
// on-device anonymity that must not reach an event, and the default-deny
// projector is the enforcement — this pins that it actually does the job on this
// rail's events rather than assuming it.
func TestCursorHookDropsUserEmailAndOtherUnallowlistedFields(t *testing.T) {
	steps := []struct {
		step  string
		extra map[string]interface{}
	}{
		{"sessionStart", nil},
		{"sessionEnd", map[string]interface{}{"reason": "completed", "duration_ms": 15337}},
		{"beforeSubmitPrompt", map[string]interface{}{"prompt": "fix the bug"}},
		{"afterShellExecution", map[string]interface{}{"command": "ls", "duration": 12.5, "output": "SECRET_STDOUT"}},
		{"afterFileEdit", map[string]interface{}{"file_path": "a.go", "edits": []map[string]string{{"new_string": "x"}}}},
		{"postToolUseFailure", map[string]interface{}{"tool_name": "Write", "failure_type": "permission_denied", "error_message": "/abs/path/leaks.go denied"}},
	}
	for _, s := range steps {
		res, ok := NormalizeCursorHook(hookPayload(s.step, s.extra))
		if !ok {
			t.Fatalf("%s produced no events", s.step)
		}
		for _, e := range res.Events {
			projected := e
			redact.ProjectEvent(&projected, false)
			blob, err := json.Marshal(projected)
			if err != nil {
				t.Fatal(err)
			}
			for _, banned := range []string{"engineer@example.com", "SECRET_STDOUT", "leaks.go", "cursor_version", "generation_id"} {
				if strings.Contains(string(blob), banned) {
					t.Fatalf("%s/%s leaked %q after projection:\n%s", s.step, e.Kind, banned, blob)
				}
			}
		}
	}
}

// --- the fields the rail exists for ------------------------------------------

// Real model attribution is the headline reason this rail exists. Only
// afterAgentThought carries model_id; every other step reports "default", which
// describes routing rather than a model.
func TestCursorHookModelComesFromModelIDNotTheDefaultLabel(t *testing.T) {
	// A step whose only model signal is "default" must report no model at all —
	// emitting "default" as a model would poison any model-mix metric.
	res, _ := NormalizeCursorHook(hookPayload("sessionStart", nil))
	if _, found := firstOfKind(res.Events, "ai_response"); found {
		t.Fatal(`model "default" must not be reported as a model`)
	}
	if res.Model != "" {
		t.Fatalf("Model = %q, want empty", res.Model)
	}

	raw := hookPayload("afterAgentThought", map[string]interface{}{
		"model_id":     "grok-4.5",
		"model_params": []map[string]string{{"id": "effort", "value": "high"}},
		"text":         "internal reasoning that must never be emitted",
	})
	res, ok := NormalizeCursorHook(raw)
	if !ok {
		t.Fatal("afterAgentThought produced no events")
	}
	e, found := firstOfKind(res.Events, "ai_response")
	if !found {
		t.Fatal("no ai_response carrying the model")
	}
	if dataOf(t, e)["model"] != "grok-4.5" {
		t.Fatalf("model = %v, want grok-4.5", dataOf(t, e)["model"])
	}
	// afterAgentThought is registered for the model ALONE. Its reasoning text is
	// the agent's own prose and has no business leaving the machine.
	if len(res.Events) != 1 {
		t.Fatalf("afterAgentThought emitted %d events, want exactly 1 (the model)", len(res.Events))
	}
	blob, _ := json.Marshal(res.Events)
	if strings.Contains(string(blob), "internal reasoning") {
		t.Fatalf("afterAgentThought leaked its reasoning text:\n%s", blob)
	}
	// Token fields are ABSENT, not zero. Cursor exposes none, and a zero would
	// read downstream as "this turn cost nothing" rather than "unknown".
	for _, k := range []string{"inputTokens", "outputTokens", "cacheReadTokens"} {
		if _, present := dataOf(t, e)[k]; present {
			t.Fatalf("ai_response carries %s — Cursor exposes no token counts, so it must be absent, not zero", k)
		}
	}
}

// `duration` is fractional MILLISECONDS. Read as seconds, a 2-second `go build`
// reported as 33 minutes — caught only by a live run, because nothing downstream
// can tell that a duration is implausible.
func TestCursorHookCommandDurationIsMilliseconds(t *testing.T) {
	raw := hookPayload("afterShellExecution", map[string]interface{}{
		"command":  "go build ./...",
		"duration": 2021.129,
	})
	res, ok := NormalizeCursorHook(raw)
	if !ok {
		t.Fatal("afterShellExecution produced no events")
	}
	e, _ := firstOfKind(res.Events, "command")
	if dataOf(t, e)["durationMs"] != 2021 {
		t.Fatalf("durationMs = %v, want 2021 (a ~2s build, not a 33-minute one)", dataOf(t, e)["durationMs"])
	}
	// No exitCode: afterShellExecution reports no status code, and inventing one
	// would be a fabricated fact.
	if _, present := dataOf(t, e)["exitCode"]; present {
		t.Fatal("command carries an exitCode — Cursor reports none, so it must be absent")
	}
}

func TestCursorHookActorsAndSource(t *testing.T) {
	cases := []struct {
		step, kind, actor string
		extra             map[string]interface{}
	}{
		{"beforeSubmitPrompt", "prompt", "human", map[string]interface{}{"prompt": "hi"}},
		{"afterFileEdit", "file_diff", "ai", map[string]interface{}{"file_path": "a.go", "edits": []map[string]string{{"new_string": "x"}}}},
		{"afterShellExecution", "command", "ai", map[string]interface{}{"command": "ls"}},
		{"sessionEnd", "session_end", "system", map[string]interface{}{"reason": "completed"}},
	}
	for _, c := range cases {
		res, ok := NormalizeCursorHook(hookPayload(c.step, c.extra))
		if !ok {
			t.Fatalf("%s produced no events", c.step)
		}
		e, found := firstOfKind(res.Events, c.kind)
		if !found {
			t.Fatalf("%s emitted no %s", c.step, c.kind)
		}
		if e.Actor == nil || e.Actor.Type != c.actor {
			t.Fatalf("%s actor = %v, want %s", c.kind, e.Actor, c.actor)
		}
		// The string that puts "cursor" into source_service. Both Cursor rails
		// must agree on it or one session would appear under two tools.
		if e.Source != "cursor" {
			t.Fatalf("%s source = %q, want cursor", c.kind, e.Source)
		}
	}
}

// Ids must be stable across invocations: Cursor may retry a hook, and the same
// payload replayed must not double-count.
func TestCursorHookEventIDsAreDeterministic(t *testing.T) {
	raw := hookPayload("afterShellExecution", map[string]interface{}{"command": "make test"})
	a, _ := NormalizeCursorHook(raw)
	b, _ := NormalizeCursorHook(raw)
	if len(a.Events) == 0 || len(a.Events) != len(b.Events) {
		t.Fatalf("event counts differ: %d vs %d", len(a.Events), len(b.Events))
	}
	for i := range a.Events {
		if a.Events[i].ID != b.Events[i].ID {
			t.Fatalf("id %d not deterministic: %s vs %s", i, a.Events[i].ID, b.Events[i].ID)
		}
	}

	// The model id must NOT vary with the hook step, so every afterAgentThought
	// in a turn collapses to one event rather than one per thought.
	m1, _ := NormalizeCursorHook(hookPayload("afterAgentThought", map[string]interface{}{"model_id": "grok-4.5", "text": "a"}))
	m2, _ := NormalizeCursorHook(hookPayload("afterAgentThought", map[string]interface{}{"model_id": "grok-4.5", "text": "completely different"}))
	if m1.Events[0].ID != m2.Events[0].ID {
		t.Fatal("two thoughts reporting the same model minted different ids — they would not collapse")
	}
}

// A step we did not register must emit nothing. Its payload has not been
// inspected for source, so the safe answer is silence.
func TestCursorHookUnregisteredStepsEmitNothing(t *testing.T) {
	for _, step := range []string{"preToolUse", "postToolUse", "preCompact", "subagentStart", "afterAgentResponse", "stop"} {
		res, ok := NormalizeCursorHook(hookPayload(step, map[string]interface{}{
			"tool_input":  map[string]string{"contents": "FILE BODY"},
			"tool_output": "COMMAND OUTPUT",
		}))
		if ok || len(res.Events) != 0 {
			t.Fatalf("%s emitted %d event(s); unregistered steps must emit nothing", step, len(res.Events))
		}
	}
}

// Garbage in must not panic or invent a session.
func TestCursorHookRejectsUnusablePayloads(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(""),
		[]byte("{{{not json"),
		[]byte(`{"hook_event_name":"sessionStart"}`),                         // no session id
		[]byte(`{"conversation_id":"c1"}`),                                   // no step
		[]byte(`{"hook_event_name":"afterFileEdit","conversation_id":"c1"}`), // no file_path
	} {
		if _, ok := NormalizeCursorHook(raw); ok {
			t.Fatalf("accepted an unusable payload: %s", raw)
		}
	}
}
