package normalize

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// Real session_meta headers captured from codex-cli 0.146.0 on 2026-08-24,
// trimmed of base_instructions. One `codex exec` run that delegated twice, so
// two subagent rollouts sit beside the thread the human typed into.
//
// The arm matters more than the values. The guardian rollouts the original
// parser was written against take `{"subagent":{"other":"guardian"}}` — a bare
// string — and carry `multi_agent_version:"disabled"`. These take the
// `thread_spawn` arm, whose value is an OBJECT, under
// `multi_agent_version:"v2"`. That flag, not the CLI version, decides which arm
// an org produces: both shapes came from the SAME 0.146.0 binary.
const (
	spawnParentThreadID = "01a035e4-d992-7d91-bea5-d25c0cc94401"
	spawnLaneAThreadID  = "01a035e4-e963-7f81-8bef-677b16745ed8"
	spawnLaneBThreadID  = "01a035e4-edce-7d03-8ab9-1436c5f8c4c5"

	spawnParentMeta = `{"timestamp":"2026-08-24T22:29:45.494Z","type":"session_meta","payload":{"session_id":"` + spawnParentThreadID + `","id":"` + spawnParentThreadID + `","timestamp":"2026-08-24T22:29:45.494Z","cwd":"/tmp/ws","originator":"codex_exec","cli_version":"0.146.0","source":"exec","thread_source":"user","model_provider":"openai","history_mode":"legacy"}}`

	// agent_role is NULL on both observed lanes. That is the wire, not a
	// simplification — see the honesty note on codexSubagentName.
	spawnLaneAMeta = `{"timestamp":"2026-08-24T22:29:49.539Z","type":"session_meta","payload":{"session_id":"` + spawnParentThreadID + `","id":"` + spawnLaneAThreadID + `","forked_from_id":"` + spawnParentThreadID + `","parent_thread_id":"` + spawnParentThreadID + `","timestamp":"2026-08-24T22:29:49.539Z","cwd":"/tmp/ws","originator":"codex_exec","cli_version":"0.146.0","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + spawnParentThreadID + `","depth":1,"agent_path":"/root/count_a","agent_nickname":"Aristotle","agent_role":null}}},"thread_source":"subagent","agent_nickname":"Aristotle","agent_path":"/root/count_a","model_provider":"openai","history_mode":"legacy","multi_agent_version":"v2"}}`

	spawnLaneBMeta = `{"timestamp":"2026-08-24T22:29:50.670Z","type":"session_meta","payload":{"session_id":"` + spawnParentThreadID + `","id":"` + spawnLaneBThreadID + `","forked_from_id":"` + spawnParentThreadID + `","parent_thread_id":"` + spawnParentThreadID + `","timestamp":"2026-08-24T22:29:50.670Z","cwd":"/tmp/ws","originator":"codex_exec","cli_version":"0.146.0","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + spawnParentThreadID + `","depth":1,"agent_path":"/root/count_b","agent_nickname":"Pauli","agent_role":"reviewer"}}},"thread_source":"subagent","agent_nickname":"Pauli","agent_path":"/root/count_b","model_provider":"openai","history_mode":"legacy","multi_agent_version":"v2"}}`

	spawnUsageLine = `{"timestamp":"2026-08-24T22:29:52.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1200,"cached_input_tokens":0,"output_tokens":48,"reasoning_output_tokens":0,"total_tokens":1248}}}}`

	// A delegated thread's final answer is what flushes its spend as
	// subagent_usage — the prose is dropped, the cost is kept.
	spawnAnswerLine = `{"timestamp":"2026-08-24T22:29:53.000Z","type":"event_msg","payload":{"type":"agent_message","message":"3 lines","phase":"final_answer"}}`
)

func spawnLane(t *testing.T, threadID, meta string) map[string]interface{} {
	t.Helper()
	events := runCodexRollout(t, threadID, []string{meta, spawnUsageLine, spawnAnswerLine})
	for _, e := range events {
		if e.Kind == "subagent_usage" {
			return codexData(e)
		}
	}
	return nil
}

// A user-spawned Codex delegate used to arrive with NO attribution at all.
// codexSubagentName scanned the `subagent` object for a single string; the
// thread_spawn arm's value is an object, so the scan found none and returned
// "" — and an omitted key is indistinguishable from a rail that reports no
// agent. This pins the label that IS on the wire.
func TestCodexThreadSpawnLaneCarriesItsNickname(t *testing.T) {
	a := spawnLane(t, spawnLaneAThreadID, spawnLaneAMeta)
	if a == nil {
		t.Fatal("no subagent_usage emitted for a thread_spawn lane")
	}
	if got := a["summary"]; got != "Aristotle" {
		t.Fatalf("summary = %v, want the lane's nickname %q", got, "Aristotle")
	}
	// agent_role is null on this lane, so there is no TYPE to report. Absent,
	// never backfilled from the nickname: a per-invocation identity in the type
	// field would make every lane its own delegate type.
	if _, ok := a["attributionAgent"]; ok {
		t.Fatalf("attributionAgent present with a null agent_role: %v", a["attributionAgent"])
	}
}

// Two concurrent lanes are the whole point: they share a session and a type,
// and before this they shared everything a reader could see.
func TestCodexConcurrentLanesAreDistinguishable(t *testing.T) {
	a := spawnLane(t, spawnLaneAThreadID, spawnLaneAMeta)
	b := spawnLane(t, spawnLaneBThreadID, spawnLaneBMeta)
	if a == nil || b == nil {
		t.Fatal("expected subagent_usage on both lanes")
	}
	if a["summary"] == b["summary"] {
		t.Fatalf("both lanes labelled %v — concurrent delegates must be separable", a["summary"])
	}
	// Lane B declares a role, so it gets a TYPE as well as a label. The two are
	// different fields answering different questions.
	if got := b["attributionAgent"]; got != "reviewer" {
		t.Fatalf("attributionAgent = %v, want %q", got, "reviewer")
	}
	if got := b["summary"]; got != "Pauli" {
		t.Fatalf("summary = %v, want %q", got, "Pauli")
	}
}

// The arm this parser was originally written against must keep working. It is
// the only arm one live external org produces today, so a regression here is
// invisible in our own capture and total in theirs.
func TestCodexOtherArmStillCarriesItsTypeName(t *testing.T) {
	events := runCodexRollout(t, codexSubThreadID, []string{codexSubagentMeta, spawnUsageLine, spawnAnswerLine})
	var data map[string]interface{}
	for _, e := range events {
		if e.Kind == "subagent_usage" {
			data = codexData(e)
		}
	}
	if data == nil {
		t.Fatal("no subagent_usage emitted for the guardian arm")
	}
	if got := data["attributionAgent"]; got != "guardian" {
		t.Fatalf("attributionAgent = %v, want %q", got, "guardian")
	}
	// guardian is a TYPE, not a lane nickname. Backfilling it into summary
	// would read downstream as a named lane that does not exist.
	if _, ok := data["summary"]; ok {
		t.Fatalf("summary present on the `other` arm: %v", data["summary"])
	}
}

// A vendor string reaching an allowlisted field is clamped to a NAME. This is
// the guard that keeps a future arm carrying a path or a prompt from being
// discovered on the wire rather than here.
func TestCodexLaneLabelRejectsAPath(t *testing.T) {
	pathish := `{"timestamp":"2026-08-24T22:29:49.539Z","type":"session_meta","payload":{"session_id":"` + spawnParentThreadID + `","id":"` + spawnLaneAThreadID + `","parent_thread_id":"` + spawnParentThreadID + `","cwd":"/tmp/ws","originator":"codex_exec","source":{"subagent":{"thread_spawn":{"depth":1,"agent_nickname":"/Users/someone/secret/agent.md","agent_role":"../../etc"}}},"thread_source":"subagent","model_provider":"openai"}}`
	data := spawnLane(t, spawnLaneAThreadID, pathish)
	if data == nil {
		t.Fatal("expected a subagent_usage event")
	}
	if v, ok := data["summary"]; ok {
		t.Fatalf("a path-shaped nickname reached summary: %v", v)
	}
	if v, ok := data["attributionAgent"]; ok {
		t.Fatalf("a path-shaped role reached attributionAgent: %v", v)
	}
}

var _ = event.Event{}
