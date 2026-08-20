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

	res, ok := NormalizeCursorHook(raw, CursorHookOptions{})
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
		res, ok := NormalizeCursorHook(hookPayload(s.step, s.extra), CursorHookOptions{})
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
// afterAgentThought carries a real model_id; every other step — and often
// afterAgentThought itself — reports "default", which describes routing rather
// than a model. model_id:"default" must not become a reported model either.
func TestCursorHookModelComesFromModelIDNotTheDefaultLabel(t *testing.T) {
	// A step whose only model signal is "default" must report no model at all —
	// emitting "default" as a model would poison any model-mix metric.
	res, _ := NormalizeCursorHook(hookPayload("sessionStart", nil), CursorHookOptions{})
	if res.Model != "" {
		t.Fatalf("Model = %q, want empty", res.Model)
	}

	// model_id:"default" is the IDE sentinel (measured 2026-08-03). Same rule.
	res, ok := NormalizeCursorHook(hookPayload("afterAgentThought", map[string]interface{}{
		"model_id": "default",
		"text":     "reasoning that must never be emitted",
	}), CursorHookOptions{})
	if ok {
		t.Fatal("afterAgentThought must produce no events")
	}
	if res.Model != "" {
		t.Fatalf("Model = %q for model_id=default, want empty", res.Model)
	}

	// A resolved model is REPORTED, not EMITTED. afterAgentThought's only product
	// is the cache entry the capture layer writes from these two fields; the
	// ai_response it used to mint was keyed on session+model and would now
	// collide with every usage row of the session.
	raw := hookPayload("afterAgentThought", map[string]interface{}{
		"model_id":     "grok-4.5",
		"model_params": []map[string]string{{"id": "effort", "value": "high"}},
		"text":         "internal reasoning that must never be emitted",
	})
	res, ok = NormalizeCursorHook(raw, CursorHookOptions{})
	if ok || len(res.Events) != 0 {
		t.Fatalf("afterAgentThought emitted %d event(s), want 0", len(res.Events))
	}
	if res.Model != "grok-4.5" || res.GenerationID != "gen-1" {
		t.Fatalf("Model/GenerationID = %q/%q, want grok-4.5/gen-1", res.Model, res.GenerationID)
	}
	blob, _ := json.Marshal(res)
	if strings.Contains(string(blob), "internal reasoning") {
		t.Fatalf("afterAgentThought leaked its reasoning text:\n%s", blob)
	}
}

// --- tokens ------------------------------------------------------------------

// stopPayload is a `stop` payload in the shape observed on Cursor 3.12.17,
// 2026-08-18 (generation 03d46a51 of the probe).
func stopPayload(gen string, extra map[string]interface{}) []byte {
	p := map[string]interface{}{
		"generation_id": gen,
		"status":        "completed",
		"loop_count":    0,
		"model":         "default",
		"model_id":      "default",
	}
	for k, v := range extra {
		p[k] = v
	}
	return hookPayload("stop", p)
}

// The four counts ride one ai_response per generation, tagged per-request.
//
// The tag is the load-bearing part. Absent, the backend reads a row as
// CUMULATIVE, differences it against a running maximum and drops the first row
// as a baseline: the observed pair would book 92,763 and then +3,372, losing an
// entire turn. Output FELL 902 -> 525 between these two generations, which no
// cumulative counter can do.
func TestCursorHookStopEmitsPerRequestUsage(t *testing.T) {
	raw := stopPayload("03d46a51", map[string]interface{}{
		"input_tokens": 92763, "output_tokens": 902,
		"cache_read_tokens": 68096, "cache_write_tokens": 0,
	})
	res, ok := NormalizeCursorHook(raw, CursorHookOptions{})
	if !ok {
		t.Fatal("stop produced no events")
	}
	e, found := firstOfKind(res.Events, "ai_response")
	if !found {
		t.Fatal("no ai_response carrying the usage")
	}
	d := dataOf(t, e)
	if d["usageScope"] != "request" {
		t.Fatalf("usageScope = %v, want request — absent means CUMULATIVE downstream", d["usageScope"])
	}
	for k, want := range map[string]interface{}{
		"inputTokens": int64(92763), "outputTokens": int64(902),
		"cacheReadTokens": int64(68096), "cacheWriteTokens": int64(0),
	} {
		if d[k] != want {
			t.Fatalf("%s = %v (%T), want %v", k, d[k], d[k], want)
		}
	}
}

// An aborted turn carries NO token keys at all — turnTokenUsage was undefined,
// and the payload simply lacks them. They must stay absent: a zero here reads
// downstream as "this turn cost nothing" rather than "we were not told", and
// this rail has already paid for that confusion twice.
//
// Note what is deliberately NOT here: a `status == "completed"` filter. Absent-
// means-absent covers an unenumerated status that DOES bill (errored,
// interrupted, max-iterations) for free, where a status allowlist would drop it
// silently and the undercount would look like a real number.
func TestCursorHookAbortedTurnHasAbsentCountsNotZeros(t *testing.T) {
	raw := stopPayload("cd025d2d", map[string]interface{}{"status": "aborted"})
	res, ok := NormalizeCursorHook(raw, CursorHookOptions{})
	if ok || len(res.Events) != 0 {
		t.Fatalf("an aborted turn with no model and no counts emitted %d event(s), want 0", len(res.Events))
	}

	// With a model known for the generation, the row exists and says only that.
	res, ok = NormalizeCursorHook(raw, CursorHookOptions{
		ResolveModel: func(string) string { return "grok-4.6" },
	})
	if !ok {
		t.Fatal("an aborted turn with a known model must still report the model")
	}
	e, _ := firstOfKind(res.Events, "ai_response")
	d := dataOf(t, e)
	for _, k := range []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens"} {
		if _, present := d[k]; present {
			t.Fatalf("%s present on an aborted turn — absent is not zero, and zero is a claim", k)
		}
	}
	if d["model"] != "grok-4.6" || d["usageScope"] != "request" || len(d) != 2 {
		t.Fatalf("aborted-turn data = %v, want exactly {model, usageScope}", d)
	}
}

// The model comes from the SAME generation or from nowhere.
//
// Cursor auto-routes by default and genuinely switches models mid-conversation,
// so "the model this session was using" is not a fact about this turn. A row
// whose generation resolved nothing is emitted with tokens and NO model — not
// dropped, not defaulted, not inherited.
func TestCursorHookUsageModelIsPerGenerationOrAbsent(t *testing.T) {
	byGen := map[string]string{"g1": "composer-2.5", "g2": "claude-opus-5"}
	opts := CursorHookOptions{ResolveModel: func(g string) string { return byGen[g] }}

	for gen, want := range byGen {
		res, ok := NormalizeCursorHook(stopPayload(gen, map[string]interface{}{"output_tokens": 10}), opts)
		if !ok {
			t.Fatalf("%s produced no events", gen)
		}
		e, _ := firstOfKind(res.Events, "ai_response")
		if got := dataOf(t, e)["model"]; got != want {
			t.Fatalf("%s model = %v, want %v — a turn must not inherit its neighbour's model", gen, got, want)
		}
	}

	// A generation the cache never saw: tokens, no model. readUsage declines to
	// price it, which is correct and visible, where a defaulted model would be
	// confidently wrong.
	res, ok := NormalizeCursorHook(stopPayload("g3", map[string]interface{}{"input_tokens": 500}), opts)
	if !ok {
		t.Fatal("a modelless usage row must still be emitted — it is counted, not dropped")
	}
	e, _ := firstOfKind(res.Events, "ai_response")
	d := dataOf(t, e)
	if _, present := d["model"]; present {
		t.Fatalf("model = %v on a generation with no cache entry, want absent", d["model"])
	}
	if d["inputTokens"] != int64(500) {
		t.Fatalf("inputTokens = %v, want 500 — the row is kept for its tokens", d["inputTokens"])
	}
}

// Each turn gets its own identity. The retired model event was keyed on
// session+model so a session's repeated model reports collapsed to ONE row;
// reusing that here would collapse a four-turn session's usage onto one id and
// lose three turns' spend to ordinary dedupe — silently, because the surviving
// row looks perfectly healthy.
func TestCursorHookFourTurnsYieldFourUsageRows(t *testing.T) {
	ids := map[string]bool{}
	for _, gen := range []string{"g1", "g2", "g3", "g4"} {
		res, ok := NormalizeCursorHook(stopPayload(gen, map[string]interface{}{
			"input_tokens": 1000, "output_tokens": 50,
		}), CursorHookOptions{ResolveModel: func(string) string { return "grok-4.6" }})
		if !ok {
			t.Fatalf("%s produced no events", gen)
		}
		e, _ := firstOfKind(res.Events, "ai_response")
		ids[e.ID] = true
	}
	if len(ids) != 4 {
		t.Fatalf("four turns produced %d distinct ids, want 4 — the rest are lost to dedupe", len(ids))
	}
}

// `duration` is fractional MILLISECONDS.// `duration` is fractional MILLISECONDS. Read as seconds, a 2-second `go build`
// reported as 33 minutes — caught only by a live run, because nothing downstream
// can tell that a duration is implausible.
func TestCursorHookCommandDurationIsMilliseconds(t *testing.T) {
	raw := hookPayload("afterShellExecution", map[string]interface{}{
		"command":  "go build ./...",
		"duration": 2021.129,
	})
	res, ok := NormalizeCursorHook(raw, CursorHookOptions{})
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
		res, ok := NormalizeCursorHook(hookPayload(c.step, c.extra), CursorHookOptions{})
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
	a, _ := NormalizeCursorHook(raw, CursorHookOptions{})
	b, _ := NormalizeCursorHook(raw, CursorHookOptions{})
	if len(a.Events) == 0 || len(a.Events) != len(b.Events) {
		t.Fatalf("event counts differ: %d vs %d", len(a.Events), len(b.Events))
	}
	for i := range a.Events {
		if a.Events[i].ID != b.Events[i].ID {
			t.Fatalf("id %d not deterministic: %s vs %s", i, a.Events[i].ID, b.Events[i].ID)
		}
	}

	// A usage row's id is derived from the generation, so Cursor redelivering one
	// `stop` payload is idempotent while two turns stay distinct.
	u := stopPayload("g1", map[string]interface{}{"input_tokens": 7, "output_tokens": 1})
	u1, _ := NormalizeCursorHook(u, CursorHookOptions{})
	u2, _ := NormalizeCursorHook(u, CursorHookOptions{})
	if u1.Events[0].ID != u2.Events[0].ID {
		t.Fatal("a redelivered stop payload minted a second id — it would double-count")
	}

	// And with no generation id at all, the counts and status carry the identity:
	// a retry still collapses, two different turns still separate. Dropping the
	// row instead would lose the turn outright, and a session-keyed id would lose
	// every turn but one.
	n1, _ := NormalizeCursorHook(stopPayload("", map[string]interface{}{"input_tokens": 7, "output_tokens": 1}), CursorHookOptions{})
	n2, _ := NormalizeCursorHook(stopPayload("", map[string]interface{}{"input_tokens": 7, "output_tokens": 1}), CursorHookOptions{})
	n3, _ := NormalizeCursorHook(stopPayload("", map[string]interface{}{"input_tokens": 9, "output_tokens": 2}), CursorHookOptions{})
	if n1.Events[0].ID != n2.Events[0].ID {
		t.Fatal("a retried generation-less stop payload minted a second id")
	}
	if n1.Events[0].ID == n3.Events[0].ID {
		t.Fatal("two different generation-less turns collided onto one id")
	}
}

// A step we did not register must emit nothing. Its payload has not been
// inspected for source, so the safe answer is silence.
func TestCursorHookUnregisteredStepsEmitNothing(t *testing.T) {
	// afterAgentResponse stays on this list ON PURPOSE. It reads the same
	// turnTokenUsage object as `stop` and would double-count every generation,
	// and it carries the assistant's full message where `stop` carries none.
	for _, step := range []string{"preToolUse", "postToolUse", "preCompact", "subagentStart", "afterAgentResponse"} {
		res, ok := NormalizeCursorHook(hookPayload(step, map[string]interface{}{
			"tool_input":  map[string]string{"contents": "FILE BODY"},
			"tool_output": "COMMAND OUTPUT",
		}), CursorHookOptions{})
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
		if _, ok := NormalizeCursorHook(raw, CursorHookOptions{}); ok {
			t.Fatalf("accepted an unusable payload: %s", raw)
		}
	}
}

// The hook rail CLAIMS a session away from the transcript watcher. If it emitted
// a prompt without the repo identity the watcher supplies, a machine with hooks
// enrolled would silently lose repo attribution for every Cursor session — the
// events still arrive, so nothing looks broken.
func TestCursorHookPromptCarriesRepoIdentityLikeTheTranscriptRail(t *testing.T) {
	raw := hookPayload("beforeSubmitPrompt", map[string]interface{}{
		"prompt": "rename the handler",
		"cwd":    "/Users/e/repos/thing",
	})
	res, ok := NormalizeCursorHook(raw, CursorHookOptions{})
	if !ok {
		t.Fatal("beforeSubmitPrompt produced no events")
	}
	if res.Workdir != "/Users/e/repos/thing" {
		t.Fatalf("Workdir = %q, want the payload's cwd", res.Workdir)
	}

	StampCursorHookRepoIdentity(res.Events, "~/repos/thing", "deadbeef", "github.com", true)
	e, _ := firstOfKind(res.Events, "prompt")
	d := dataOf(t, e)
	for k, want := range map[string]interface{}{
		"workdir":  "~/repos/thing",
		"repoRoot": "deadbeef",
		"repoHost": "github.com",
	} {
		if d[k] != want {
			t.Fatalf("%s = %v, want %v", k, d[k], want)
		}
	}
	// Stamped explicitly, never omitempty: "not a repo" must stay
	// distinguishable from "a CLI too old to have looked".
	if d["repoTracked"] != true {
		t.Fatalf("repoTracked = %v, want true", d["repoTracked"])
	}
	StampCursorHookRepoIdentity(res.Events, "~/x", "cafe", "", false)
	if dataOf(t, e)["repoTracked"] != false {
		t.Fatal("repoTracked was not stamped false for an untracked dir")
	}
}

// workspace_roots is the fallback when a step reports no cwd of its own.
func TestCursorHookWorkdirFallsBackToWorkspaceRoots(t *testing.T) {
	res, ok := NormalizeCursorHook(hookPayload("beforeSubmitPrompt", map[string]interface{}{
		"prompt":          "hi",
		"workspace_roots": []string{"/repo/a", "/repo/b"},
	}), CursorHookOptions{})
	if !ok {
		t.Fatal("no events")
	}
	if res.Workdir != "/repo/a" {
		t.Fatalf("Workdir = %q, want /repo/a", res.Workdir)
	}
}

// model_params carries a reasoning-effort token. It is NOT allowlisted on either
// side, so emitting it would be stripped silently and read as "an older CLI".
func TestCursorHookDoesNotEmitReasoningEffort(t *testing.T) {
	res, _ := NormalizeCursorHook(stopPayload("g1", map[string]interface{}{
		"model_params": []map[string]string{{"id": "effort", "value": "high"}},
	}), CursorHookOptions{ResolveModel: func(string) string { return "grok-4.5" }})
	e, found := firstOfKind(res.Events, "ai_response")
	if !found {
		t.Fatal("no ai_response")
	}
	// Exactly the model and the scope tag. Asserting on the whole serialized
	// event would be a false negative waiting to happen — "high" is also the
	// value of provenance.observability.
	d := dataOf(t, e)
	if len(d) != 2 || d["model"] != "grok-4.5" || d["usageScope"] != "request" {
		t.Fatalf("ai_response data = %v, want exactly {model, usageScope}", d)
	}
}

// --- the privacy boundary, across every step this rail registers -------------

// Every `stop`, `afterAgentResponse` and `beforeSubmitPrompt` payload observed
// carries `user_email` — the first PII field this rail has ever presented, and
// the one the next person adding a field here will be looking at.
//
// The default-deny allowlists exclude it by OMISSION, which is exactly the
// property that makes a new field vanish silently rather than leak silently. This
// test is the one that would notice it being added: it runs the full projection
// over one event of every kind this rail can produce and asserts that the four
// payload fields carrying a person or their prose survive on none of them.
//
// The values are synthetic on purpose. A fixture copied from the real probe run
// would put a live engineer's address and prompts into the repository to prove
// they never leave a machine.
func TestCursorHookNeverShipsEmailOrProse(t *testing.T) {
	const (
		email  = "engineer@example.com"
		prose  = "ASSISTANT PROSE THAT MUST NOT SHIP"
		typed  = "the engineer's own question"
		attach = "SECRET-ATTACHMENT-PATH"
	)
	steps := []struct {
		step  string
		extra map[string]interface{}
	}{
		{"sessionStart", nil},
		{"sessionEnd", map[string]interface{}{"final_status": "completed"}},
		{"beforeSubmitPrompt", map[string]interface{}{
			"prompt":      typed,
			"attachments": []map[string]string{{"type": "rule", "file_path": attach}},
		}},
		{"afterFileEdit", map[string]interface{}{
			"file_path": "a.go",
			"edits":     []map[string]string{{"old_string": prose, "new_string": prose}},
		}},
		{"afterShellExecution", map[string]interface{}{"command": "go test ./..."}},
		{"postToolUseFailure", map[string]interface{}{"tool_name": "read", "failure_type": "denied"}},
		{"afterAgentThought", map[string]interface{}{"model_id": "grok-4.6", "text": prose}},
		{"stop", map[string]interface{}{
			"status": "completed", "input_tokens": 10, "output_tokens": 2,
			"text": prose, "user_email": email,
		}},
	}
	for _, c := range steps {
		extra := map[string]interface{}{"user_email": email}
		for k, v := range c.extra {
			extra[k] = v
		}
		raw := redact.RedactBytes(hookPayload(c.step, extra))
		res, _ := NormalizeCursorHook(raw, CursorHookOptions{
			ResolveModel: func(string) string { return "grok-4.6" },
		})
		for i := range res.Events {
			e := res.Events[i]
			redact.ProjectEvent(&e, false)
			blob, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("%s: %v", c.step, err)
			}
			for _, forbidden := range []string{email, prose, attach} {
				if strings.Contains(string(blob), forbidden) {
					t.Fatalf("%s leaked %q past projection:\n%s", c.step, forbidden, blob)
				}
			}
			// The engineer's own typed prompt is the ONE prose field this rail
			// carries on purpose, and only on the kind that exists for it.
			if strings.Contains(string(blob), typed) && e.Kind != "prompt" {
				t.Fatalf("%s put the engineer's prompt on a %s event:\n%s", c.step, e.Kind, blob)
			}
		}
	}
}
