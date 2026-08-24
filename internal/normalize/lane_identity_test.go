package normalize

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// These tests exist because within-session parallelism was unmeasurable, and the
// reason was narrow: lane identity rode `subagent_usage` only. That is a SPEND
// event. It answers what the delegates cost and it cannot answer how many ran at
// once, because a lane's interval has to come from the first and last moment it
// was WORKING — and the kinds that carry work carried no lane.
//
// The failure mode each test guards is the same one, and it is silent: a lane
// with no id is not an error anywhere, it is a session that looks like it ran a
// single lane. Absent and one are the two readings that must never merge.

func laneOf(t *testing.T, e event.Event) string {
	t.Helper()
	d, ok := e.Data.(map[string]interface{})
	if !ok {
		return ""
	}
	s, _ := d["agentId"].(string)
	return s
}

// --- Cursor ------------------------------------------------------------------

// The rail that most needed lane identity was guaranteed not to have it. Cursor
// emits no subagent_usage at all — deliberately: its transcripts carry no token
// counts, and a spend event with absent counts reads downstream as a MEASURED
// ZERO. So on the one rail where the id could not ride spend, it had to ride the
// work, which is what this test pins.
func TestCursorSidechainStampsLaneOnWorkEvents(t *testing.T) {
	p := procWith("parent-sess")
	p.Sidechain = true
	p.LaneID = "cd5866a6-d091-4188-bd14-3661e0ce0c8f"

	p.Process([]byte(`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Friday, Jul 31, 2026, 5:10 PM (UTC-5)</timestamp>\n<user_query>go</user_query>"}]}}`), 0)
	edit := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"/w/a.go","old_string":"x","new_string":"y"}}]}}`)

	e, ok := firstOfKind(p.Process(edit, 50), "file_diff")
	if !ok {
		t.Fatal("no file_diff from sidechain")
	}
	if got := laneOf(t, e); got != p.LaneID {
		t.Fatalf("file_diff agentId = %q, want the child transcript's lane %q", got, p.LaneID)
	}
	// The session must NOT be the lane. A lane id that is really a session id
	// does not read as wrong downstream — it reads as a tool that runs exactly
	// one lane, which is the specific wrong answer this whole change exists to
	// stop producing.
	if got := laneOf(t, e); got == "parent-sess" {
		t.Fatal("agentId is the session id, not a lane")
	}
}

// The parent transcript is the main chain. Stamping it would assert "one lane"
// where the truth is "the main chain, plus whatever it delegated" — so a
// processor with no LaneID must stamp nothing rather than stamp its session.
func TestCursorParentStampsNoLane(t *testing.T) {
	p := procWith("parent-sess")
	edit := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"/w/a.go","old_string":"x","new_string":"y"}}]}}`)

	e, ok := firstOfKind(p.Process(edit, 50), "file_diff")
	if !ok {
		t.Fatal("no file_diff from parent")
	}
	if got := laneOf(t, e); got != "" {
		t.Fatalf("parent file_diff carries agentId %q, want none", got)
	}
}

// --- Codex -------------------------------------------------------------------

// A Codex delegate is its OWN rollout file, merged into the parent conversation
// by session id. Without the stamp its file_diffs arrive indistinguishable from
// the parent's own work and the conversation looks like one lane doing
// everything.
func TestCodexSubagentThreadStampsLaneOnWorkEvents(t *testing.T) {
	evs := runCodexRollout(t, codexSubThreadID, codexSubagentLines)

	var checked int
	for _, e := range evs {
		if e.Kind == "session_start" {
			// session_start belongs to the CONVERSATION, not to a lane: the
			// parent and each delegate open with their own session_meta, and one
			// logical session must begin exactly once.
			if got := laneOf(t, e); got != "" {
				t.Fatalf("session_start carries agentId %q — it is conversation-scoped", got)
			}
			continue
		}
		if got := laneOf(t, e); got != codexSubThreadID {
			t.Fatalf("%s agentId = %q, want the delegate's thread %q", e.Kind, got, codexSubThreadID)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no lane-bearing events in the delegate rollout — the fixture stopped exercising this")
	}
}

// The thread the human types into has no lane to name: its threadID IS the
// session. Stamping it would turn "the main chain" into "lane 1 of 1".
func TestCodexUserThreadStampsNoLane(t *testing.T) {
	for _, e := range runCodexRollout(t, codexParentThreadID, codexParentLines) {
		if got := laneOf(t, e); got != "" {
			t.Fatalf("user-thread %s carries agentId %q, want none", e.Kind, got)
		}
	}
}

// The vendor hands us the delegate's TYPE name and we dropped it. It is a
// different question from the lane id and both are needed: the id says WHICH
// invocation, the name says WHAT KIND. Deduplicating concurrent delegates on the
// name instead of the id is a measured 32x undercount.
func TestCodexSubagentCarriesAgentName(t *testing.T) {
	evs := runCodexRollout(t, codexSubThreadID, codexSubagentLines)
	e, ok := firstOfKind(evs, "subagent_usage")
	if !ok {
		t.Fatal("no subagent_usage from the delegate rollout")
	}
	d := codexData(e)
	if got, _ := d["attributionAgent"].(string); got != "guardian" {
		t.Fatalf("attributionAgent = %q, want %q from session_meta.source", got, "guardian")
	}
	if got, _ := d["agentId"].(string); got != codexSubThreadID {
		t.Fatalf("agentId = %q, want the thread id %q — the name is not the id", got, codexSubThreadID)
	}
}

// `source` is a tagged union, and two rollouts is the entire sample the parser
// was written against. The inner key is not hardcoded, and a shape that does not
// resolve to exactly one name yields nothing — an absent name reads as "not
// observed", a wrong one reads as a fact.
func TestCodexSubagentNameShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    string
	}{
		{"documented shape", map[string]interface{}{"source": map[string]interface{}{
			"subagent": map[string]interface{}{"other": "guardian"}}}, "guardian"},
		{"an inner key we have never seen", map[string]interface{}{"source": map[string]interface{}{
			"subagent": map[string]interface{}{"builtin": "reviewer"}}}, "reviewer"},
		{"subagent carries the name directly", map[string]interface{}{"source": map[string]interface{}{
			"subagent": "reviewer"}}, "reviewer"},
		{"a human thread", map[string]interface{}{"source": "cli"}, ""},
		{"no source at all", map[string]interface{}{}, ""},
		{"two candidates and no rule for choosing", map[string]interface{}{"source": map[string]interface{}{
			"subagent": map[string]interface{}{"other": "a", "builtin": "b"}}}, ""},
		// The reason this is clamped at all: attributionAgent is definitionally a
		// NAME, and it is allowlisted. A future variant carrying a path must be
		// dropped here rather than discovered on the wire.
		{"a path where a name should be", map[string]interface{}{"source": map[string]interface{}{
			"subagent": map[string]interface{}{"other": "/Users/paarth/agents/guardian.md"}}}, ""},
		{"prose where a name should be", map[string]interface{}{"source": map[string]interface{}{
			"subagent": map[string]interface{}{"other": "judge this action"}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexSubagentName(tc.payload); got != tc.want {
				t.Fatalf("codexSubagentName = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- the stamper itself ------------------------------------------------------

// An empty lane stamps NOTHING. "This rail did not tell us" is a state the
// consumer contract distinguishes from "one lane"; a placeholder would merge
// every unidentifiable lane into a single fake one.
func TestStampLaneIDEmptyStampsNothing(t *testing.T) {
	evs := []event.Event{{Kind: "command", Data: map[string]interface{}{"exitCode": 0}}}
	for _, e := range stampLaneID(evs, "") {
		if _, present := e.Data.(map[string]interface{})["agentId"]; present {
			t.Fatal("an empty lane id was stamped")
		}
	}
}

// A non-map Data is left alone rather than replaced: the lane id is worth less
// than whatever a future kind chose to put there.
func TestStampLaneIDLeavesNonMapPayloadAlone(t *testing.T) {
	evs := []event.Event{{Kind: "command", Data: "not-a-map"}}
	if got := stampLaneID(evs, "lane-1")[0].Data; got != "not-a-map" {
		t.Fatalf("Data = %#v, want the original payload untouched", got)
	}
}
