package normalize

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

func TestCodexResponseItemFinalUsesFollowingCumulativeUsage(t *testing.T) {
	p := NewCodexRolloutProcessor("thread-1")
	lines := []string{
		`{"timestamp":"2026-08-27T00:00:00Z","type":"session_meta","payload":{"id":"thread-1","session_id":"thread-1","thread_source":"user"}}`,
		`{"timestamp":"2026-08-27T00:00:01Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}`,
		`{"timestamp":"2026-08-27T00:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2}}}}`,
		`{"timestamp":"2026-08-27T00:00:03Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"msg-fake","phase":"final_answer","content":"Done"}}}`,
		`{"timestamp":"2026-08-27T00:00:04Z","type":"response_item","payload":{"type":"message","id":"msg-fake","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done"}]}}`,
		`{"timestamp":"2026-08-27T00:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":20,"cached_input_tokens":8,"output_tokens":5}}}}`,
		`{"timestamp":"2026-08-27T00:00:06Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":"Done"}}`,
	}
	count := 0
	for i, line := range lines {
		for _, e := range p.Process([]byte(line)) {
			if e.Kind != "ai_response" {
				continue
			}
			count++
			d := e.Data.(map[string]interface{})
			if d["model"] != "gpt-5.6-sol" || d["inputTokens"] != int64(20) || d["cacheReadTokens"] != int64(8) || d["outputTokens"] != int64(5) {
				t.Fatalf("usage = %v", d)
			}
		}
		if i == 4 && !p.HasBufferedState() {
			t.Fatal("final answer must remain buffered until the following token_count")
		}
	}
	if count != 1 {
		t.Fatalf("ai_response count = %d, want 1", count)
	}
}

// A rollout interrupted after the final answer never writes task_complete.
func TestCodexStaleFinalFlushesWithoutTaskComplete(t *testing.T) {
	p := NewCodexRolloutProcessor("thread-1")
	for _, line := range []string{
		`{"timestamp":"2026-08-27T00:00:01Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}`,
		`{"timestamp":"2026-08-27T00:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done"}]}}`,
		`{"timestamp":"2026-08-27T00:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":20,"cached_input_tokens":8,"output_tokens":5}}}}`,
	} {
		if got := p.Process([]byte(line)); len(aiResponses(got)) != 0 {
			t.Fatal("no ai_response before task_complete or a stale flush")
		}
	}
	if got := p.FlushStaleFinal(); len(got) != 0 {
		t.Fatal("first poll must only age the buffered final")
	}
	got := aiResponses(p.FlushStaleFinal())
	if len(got) != 1 {
		t.Fatalf("stale flush ai_response count = %d, want 1", len(got))
	}
	if d := got[0].Data.(map[string]interface{}); d["inputTokens"] != int64(20) {
		t.Fatalf("usage = %v", d)
	}
	if again := p.FlushStaleFinal(); len(again) != 0 {
		t.Fatal("flush must not repeat")
	}
}

// The next turn starting also proves the previous one ended without task_complete.
func TestCodexNextTurnFlushesUnfinishedFinal(t *testing.T) {
	p := NewCodexRolloutProcessor("thread-1")
	count := 0
	for _, line := range []string{
		`{"timestamp":"2026-08-27T00:00:01Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}`,
		`{"timestamp":"2026-08-27T00:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done"}]}}`,
		`{"timestamp":"2026-08-27T00:01:00Z","type":"event_msg","payload":{"type":"task_started"}}`,
	} {
		count += len(aiResponses(p.Process([]byte(line))))
	}
	if count != 1 {
		t.Fatalf("ai_response count = %d, want 1", count)
	}
}

func aiResponses(evs []event.Event) []event.Event {
	var out []event.Event
	for _, e := range evs {
		if e.Kind == "ai_response" {
			out = append(out, e)
		}
	}
	return out
}

func TestCodexOldFinalAnswerCopyDoesNotDuplicate(t *testing.T) {
	p := NewCodexRolloutProcessor("thread-1")
	lines := []string{
		`{"timestamp":"2026-07-01T00:00:00Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}`,
		`{"timestamp":"2026-07-01T00:00:01Z","type":"event_msg","payload":{"type":"agent_message","phase":"final_answer","message":"Done"}}`,
		`{"timestamp":"2026-07-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done"}]}}`,
		`{"timestamp":"2026-07-01T00:00:03Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":"Done"}}`,
	}
	count := 0
	for _, line := range lines {
		for _, e := range p.Process([]byte(line)) {
			if e.Kind == "ai_response" {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("ai_response count = %d, want 1", count)
	}
}
