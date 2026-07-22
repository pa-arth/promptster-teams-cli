package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

func TestReplayCodexRolloutPrefixRestoresDetachedCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-restart.jsonl")
	prefix := "" +
		`{"timestamp":"2026-07-22T17:30:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_original","input":"const r = await tools.exec_command({cmd:\"go test ./...\"}); text(r.output);"}}` + "\n" +
		`{"timestamp":"2026-07-22T17:30:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_original","output":[{"type":"input_text","text":"Script running with cell ID 42464\n"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(prefix), 0o600); err != nil {
		t.Fatal(err)
	}

	// Model a daemon restart: the durable offset points past the handoff, but
	// the new processor begins with empty pending/running maps.
	p := normalize.NewCodexRolloutProcessor("sess-restart")
	replayCodexRolloutPrefix(path, int64(len(prefix)), p)

	poll := `{"timestamp":"2026-07-22T17:30:02Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_poll","input":"const r = await tools.write_stdin({session_id:42464,chars:\"\"}); text(r.output);"}}`
	complete := `{"timestamp":"2026-07-22T17:30:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_poll","output":[{"type":"input_text","text":"Script completed\nOutput:\nok"}]}}`
	if got := p.Process([]byte(poll)); len(got) != 0 {
		t.Fatalf("poll call emitted events: %#v", got)
	}
	events := p.Process([]byte(complete))
	if len(events) != 1 || events[0].Kind != "command" {
		t.Fatalf("completion after restart = %#v, want original command", events)
	}
	data := events[0].Data.(map[string]interface{})
	if data["command"] != "go test ./..." {
		t.Errorf("restored command = %v", data["command"])
	}
}
