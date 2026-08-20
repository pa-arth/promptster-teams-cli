package capture

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// The join is per GENERATION, and that is the whole reason it exists: Cursor
// auto-routes by default and switches models mid-conversation, so a
// session-keyed cache would attribute one turn's tokens to another turn's model.
func TestGenerationModelJoinDoesNotLeakAcrossTurns(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	recordCursorGenerationModel("g1", "composer-2.5")
	recordCursorGenerationModel("g2", "claude-opus-5")

	if got := cursorGenerationModel("g1"); got != "composer-2.5" {
		t.Fatalf("g1 = %q, want composer-2.5", got)
	}
	if got := cursorGenerationModel("g2"); got != "claude-opus-5" {
		t.Fatalf("g2 = %q, want claude-opus-5", got)
	}
	// A generation nobody recorded answers "", not the neighbour's model. That ""
	// is a real answer: the row ships with tokens and no model rather than a
	// confidently wrong one.
	if got := cursorGenerationModel("g3"); got != "" {
		t.Fatalf("g3 = %q, want empty — a turn must not inherit a model", got)
	}
}

// Bounded by AGE. An entry older than the TTL answers "" rather than a stale
// model — inheriting across the window is exactly what per-generation keying is
// for, and a machine left running must not carry last week's routing.
func TestGenerationModelExpires(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	stale := cursorGenerations{
		V: cursorGenerationsVersion,
		Entries: map[string]cursorGeneration{
			"old": {Model: "grok-4.6", TsMs: time.Now().Add(-cursorGenerationTTL - time.Hour).UnixMilli()},
		},
	}
	saveCursorGenerations(stale)
	if got := cursorGenerationModel("old"); got != "" {
		t.Fatalf("an expired entry answered %q — stale routing would be attributed to a fresh turn", got)
	}
}

// Bounded by COUNT as well, because age alone is not a bound: a busy machine
// with several conversations open mints generations far faster than a six-hour
// TTL retires them. Oldest-first, so the entries dropped are the ones whose
// `stop` is least likely to still be coming.
func TestGenerationCacheIsBoundedByCount(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	c := cursorGenerations{V: cursorGenerationsVersion, Entries: map[string]cursorGeneration{}}
	base := time.Now()
	for i := 0; i < cursorGenerationsMax+50; i++ {
		c.Entries[string(rune('a'+i%26))+strings.Repeat("x", i)] = cursorGeneration{
			Model: "m", TsMs: base.Add(time.Duration(i) * time.Millisecond).UnixMilli(),
		}
	}
	pruneCursorGenerations(&c, base)
	if len(c.Entries) != cursorGenerationsMax {
		t.Fatalf("cache holds %d entries after pruning, want the %d bound", len(c.Entries), cursorGenerationsMax)
	}
}

// The counters that replace the probe. A usage row with no model is COUNTED, not
// dropped, and the count is what keeps model coverage observable once the probe
// that measured it is torn down.
func TestUsageObservationsCountModellessRows(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	usage := func(out int64) event.Event {
		return event.Event{Kind: "ai_response", Data: map[string]interface{}{
			"usageScope": "request", "outputTokens": out,
		}}
	}
	recordCursorUsageObservations("conv", "grok-4.6", []event.Event{usage(902)})
	recordCursorUsageObservations("conv", "", []event.Event{usage(525)})

	c := loadCursorGenerations()
	if c.UsageRows != 2 || c.ModellessRows != 1 {
		t.Fatalf("usageRows/modelless = %d/%d, want 2/1", c.UsageRows, c.ModellessRows)
	}
	// THE PER-REQUEST PREMISE, COUNTED RATHER THAN ASSERTED. Output fell 902 ->
	// 525, which no cumulative counter can do. A measurement recorded only as
	// prose has no expiry; this one has a number that keeps moving.
	if c.PerRequestComparisons != 1 || c.PerRequestDecreases != 1 {
		t.Fatalf("perRequest comparisons/decreases = %d/%d, want 1/1",
			c.PerRequestComparisons, c.PerRequestDecreases)
	}
}

// End to end through the two hook invocations, in the order Cursor fires them:
// the thought records the model, the stop that follows joins it onto the tokens.
func TestThoughtThenStopProducesOnePricedUsageRow(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	thought := []byte(`{"hook_event_name":"afterAgentThought","conversation_id":"c1",
		"generation_id":"g1","model_id":"composer-2.5","text":"reasoning"}`)
	res, _ := normalize.NormalizeCursorHook(thought, normalize.CursorHookOptions{ResolveModel: cursorGenerationModel})
	if res.Step != "afterAgentThought" || res.Model != "composer-2.5" || res.GenerationID != "g1" {
		t.Fatalf("thought resolved %+v", res)
	}
	recordCursorGenerationModel(res.GenerationID, res.Model)

	stop := []byte(`{"hook_event_name":"stop","conversation_id":"c1","generation_id":"g1",
		"model":"default","model_id":"default","status":"completed",
		"input_tokens":96135,"output_tokens":525,"cache_read_tokens":94976,"cache_write_tokens":0}`)
	res, ok := normalize.NormalizeCursorHook(stop, normalize.CursorHookOptions{ResolveModel: cursorGenerationModel})
	if !ok || len(res.Events) != 1 {
		t.Fatalf("stop produced %d events, want 1", len(res.Events))
	}
	d, _ := res.Events[0].Data.(map[string]interface{})
	if d["model"] != "composer-2.5" {
		t.Fatalf("model = %v, want composer-2.5 — the join did not happen", d["model"])
	}
	if d["usageScope"] != "request" || d["inputTokens"] != int64(96135) || d["outputTokens"] != int64(525) {
		blob, _ := json.Marshal(d)
		t.Fatalf("usage row = %s", blob)
	}
}

// `stop` IS A GATING HOOK: Cursor parses this command's stdout and, if it finds
// `followup_message`, SUBMITS THAT TEXT AS A NEW CHAT TURN — up to five times.
// A handler that echoed its input, or printed a debug blob that happened to
// carry that key, would be driving the customer's agent with our telemetry, and
// nothing about it would look like a bug in a hook.
//
// The containment is that the response is a compile-time constant. This test
// asserts the property that makes it one: the constant says nothing the payload
// could have put there.
func TestCursorHookStdoutIsAConstantThatCannotDriveTheAgent(t *testing.T) {
	if strings.Contains(cursorHookStdout, "followup_message") {
		t.Fatalf("the hook response carries followup_message: %s", cursorHookStdout)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(cursorHookStdout), &parsed); err != nil {
		t.Fatalf("the hook response is not JSON Cursor can read: %v", err)
	}
	if len(parsed) != 1 || parsed["continue"] != true {
		t.Fatalf("hook response = %v, want exactly {continue: true}", parsed)
	}
	// And it is the ENTIRE contribution of this command to stdout — no other
	// write, no format verb an operand could reach.
	src, err := os.ReadFile("cmd_cursor_hook.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "os.Stdout") {
			continue
		}
		if !strings.Contains(line, "cursorHookStdout") {
			t.Fatalf("a second write to stdout: %s", strings.TrimSpace(line))
		}
	}
}
