package normalize

import (
	"encoding/json"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

func codexFinalAnswer(t *testing.T) event.Event {
	t.Helper()
	p := NewCodexRolloutProcessor("thread-1")
	var got []event.Event
	for _, line := range []string{
		`{"timestamp":"2026-07-01T00:00:00Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}`,
		`{"timestamp":"2026-07-01T00:00:01Z","type":"event_msg","payload":{"type":"agent_message","phase":"final_answer","message":"Fixed the handler."}}`,
		`{"timestamp":"2026-07-01T00:00:03Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":"Fixed the handler."}}`,
	} {
		got = append(got, aiResponses(p.Process([]byte(line)))...)
	}
	if len(got) != 1 {
		t.Fatalf("ai_response count = %d, want 1", len(got))
	}
	return got[0]
}

func projectedCopy(e event.Event, prose bool, dropText bool) map[string]interface{} {
	d := map[string]interface{}{}
	for k, v := range e.Data.(map[string]interface{}) {
		d[k] = v
	}
	if dropText {
		delete(d, "text")
	}
	e.Data = d
	redact.ProjectEvent(&e, prose)
	return e.Data.(map[string]interface{})
}

func TestCodexFinalAnswerProseKeptWhenPolicyOn(t *testing.T) {
	d := projectedCopy(codexFinalAnswer(t), true, false)
	if d["text"] != "Fixed the handler." {
		t.Fatalf("text = %#v, want the final answer", d["text"])
	}
	if _, leaked := d["lastAssistantMessage"]; leaked {
		t.Fatal("lastAssistantMessage survived projection")
	}
}

// Off: the projected bytes equal what a pre-change event (no `text` key) projects to.
func TestCodexFinalAnswerPolicyOffIsByteIdentical(t *testing.T) {
	e := codexFinalAnswer(t)
	now, _ := json.Marshal(projectedCopy(e, false, false))
	before, _ := json.Marshal(projectedCopy(e, false, true))
	if string(now) != string(before) {
		t.Fatalf("policy-off projection changed:\n now    %s\n before %s", now, before)
	}
}
