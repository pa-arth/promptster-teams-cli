package normalize

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

type laneEvent struct {
	kind string
	data map[string]interface{}
}

func laneData(e event.Event) map[string]interface{} {
	m, _ := e.Data.(map[string]interface{})
	return m
}

// A Claude sidechain is processed counters-only on purpose — its prose is
// agent-authored and must not leave the machine. The dispatch LABEL is not
// prose: it is the parent's Task `description`, already emitted verbatim as
// task_dispatch.summary for every dispatch. This pins that it rides the SPEND
// event too, which is the only place the label and the money meet.
func TestClaudeSidechainUsageCarriesItsDispatchLabel(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	p.UsageOnly = true
	p.AgentID = "aaa9a3a70c738c722"
	p.Summary = "Scout backend ingest contract"

	var events []laneEvent
	for _, line := range []string{
		`{"type":"assistant","timestamp":"2026-08-24T10:00:00.000Z","isSidechain":true,"message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"user","timestamp":"2026-08-24T10:00:01.000Z","isSidechain":true}`,
	} {
		for _, e := range p.Process([]byte(line)) {
			events = append(events, laneEvent{e.Kind, laneData(e)})
		}
	}

	var seen bool
	for _, e := range events {
		if e.kind != "subagent_usage" {
			continue
		}
		seen = true
		if got := e.data["summary"]; got != "Scout backend ingest contract" {
			t.Fatalf("summary = %v, want the dispatch label", got)
		}
		if got := e.data["agentId"]; got != "aaa9a3a70c738c722" {
			t.Fatalf("agentId = %v, want the lane id beside the label", got)
		}
	}
	if !seen {
		t.Fatal("no subagent_usage emitted")
	}
}

// A sidechain with no sidecar must omit the key, never stamp an empty string:
// "this lane did not tell us" and "this lane had no purpose" are opposite
// readings, and an empty label merges every unlabelled lane into one.
func TestClaudeSidechainOmitsAnAbsentLabel(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-1")
	p.UsageOnly = true
	p.AgentID = "aaa9a3a70c738c722"

	for _, line := range []string{
		`{"type":"assistant","timestamp":"2026-08-24T10:00:00.000Z","isSidechain":true,"message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"user","timestamp":"2026-08-24T10:00:01.000Z","isSidechain":true}`,
	} {
		for _, e := range p.Process([]byte(line)) {
			if e.Kind != "subagent_usage" {
				continue
			}
			if v, ok := laneData(e)["summary"]; ok {
				t.Fatalf("summary present with no sidecar: %v", v)
			}
		}
	}
}

// The label is capped where task_dispatch caps the identical string, so one
// label cannot ship at two lengths from two producers.
func TestClaudeLaneLabelIsCappedLikeTaskDispatch(t *testing.T) {
	long := ""
	for len(long) < 400 {
		long += "delegate a bounded slice of the migration "
	}
	p := NewClaudeTranscriptProcessor("sess-1")
	p.UsageOnly = true
	p.Summary = long

	for _, line := range []string{
		`{"type":"assistant","timestamp":"2026-08-24T10:00:00.000Z","isSidechain":true,"message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"user","timestamp":"2026-08-24T10:00:01.000Z","isSidechain":true}`,
	} {
		for _, e := range p.Process([]byte(line)) {
			if e.Kind != "subagent_usage" {
				continue
			}
			got, _ := laneData(e)["summary"].(string)
			if len(got) > 103 { // 100 + the "..." strPreview appends
				t.Fatalf("label shipped at %d bytes uncapped", len(got))
			}
			if got == "" {
				t.Fatal("a long label was dropped entirely rather than capped")
			}
		}
	}
}
