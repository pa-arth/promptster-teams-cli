package normalize

import "testing"

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
