package normalize

import "testing"

func TestCodexCumulativeUsageEmitsWithoutFinalResponse(t *testing.T) {
	p := NewCodexRolloutProcessor("thread-1")
	p.Process([]byte(`{"timestamp":"2026-09-14T00:00:00Z","type":"session_meta","payload":{"id":"thread-1","session_id":"thread-1","thread_source":"user"}}`))
	line := []byte(`{"timestamp":"2026-09-14T00:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":120,"cached_input_tokens":90,"output_tokens":15},"model_context_window":258400}}}`)
	got := p.Process(line)
	if len(got) != 1 || got[0].Kind != "codex_session_usage" || got[0].Source != "codex" {
		t.Fatalf("events = %+v", got)
	}
	data := got[0].Data.(map[string]interface{})
	if data["threadId"] != "thread-1" || data["inputTokens"] != int64(120) || data["cacheReadTokens"] != int64(90) || data["outputTokens"] != int64(15) {
		t.Fatalf("data = %+v", data)
	}
	if p.lastContextWindow != 258400 {
		t.Fatalf("context window lost: %d", p.lastContextWindow)
	}
	if again := p.Process(line); len(again) != 1 || again[0].ID != got[0].ID {
		t.Fatal("replayed token_count line must keep a deterministic id")
	}
	if len(p.Process([]byte(`{"timestamp":"2026-09-14T00:00:02Z","type":"event_msg","payload":{"type":"task_complete"}}`))) != 0 {
		t.Fatal("usage event must not synthesize an answer")
	}
}

func TestCodexCumulativeUsageRejectsIncompleteVector(t *testing.T) {
	p := NewCodexRolloutProcessor("thread-1")
	p.Process([]byte(`{"timestamp":"2026-09-14T00:00:00Z","type":"session_meta","payload":{"id":"thread-1","session_id":"thread-1"}}`))
	for _, usage := range []string{
		`{"input_tokens":100,"output_tokens":10}`,
		`{"input_tokens":10,"cached_input_tokens":20,"output_tokens":1}`,
		`{"input_tokens":1.5,"cached_input_tokens":1,"output_tokens":1}`,
		`{"input_tokens":-1,"cached_input_tokens":0,"output_tokens":1}`,
	} {
		line := `{"timestamp":"2026-09-14T00:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` + usage + `}}}`
		if got := p.Process([]byte(line)); len(got) != 0 {
			t.Fatalf("accepted incomplete usage %s: %+v", usage, got)
		}
	}
}
