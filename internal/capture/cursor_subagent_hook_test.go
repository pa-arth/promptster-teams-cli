package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// A subagent's hook payload carries the CHILD conversation id and no
// transcript_path (Cursor looks the path up by child id alone and misses
// <parent>/subagents/<child>.jsonl). Measured on ops.ai 2026-09-28: 162 of 196
// prompt-less Cursor sessions were subagents split off this way.
func TestSubagentHookEventsRollUpToParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMPTSTER_CURSOR_HOME", home)
	child := filepath.Join(home, "projects", "ws", "agent-transcripts", "parent-1", "subagents", "child-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(child), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	parent, path := cursorSubagentTranscript("child-1")
	if parent != "parent-1" || path != child {
		t.Fatalf("lookup = %q %q", parent, path)
	}
	if p, _ := cursorSubagentTranscript("parent-1"); p != "" {
		t.Fatalf("main-chain session resolved as subagent of %q", p)
	}
	if p, _ := cursorSubagentTranscript("*"); p != "" {
		t.Fatal("glob metacharacters in the id must not match")
	}

	res, ok := normalize.NormalizeCursorHook([]byte(`{"hook_event_name":"afterShellExecution",
		"conversation_id":"child-1","generation_id":"g","command":"go test ./...","duration":10}`),
		normalize.CursorHookOptions{})
	if !ok || res.TranscriptPath != "" {
		t.Fatalf("ok=%v transcript=%q", ok, res.TranscriptPath)
	}
	normalize.RollUpCursorSubagent(&res, parent)
	if res.SessionID != "parent-1" {
		t.Fatalf("session = %q", res.SessionID)
	}
	for _, e := range res.Events {
		if e.SessionID != "parent-1" || e.Data.(map[string]interface{})["agentId"] != "child-1" {
			t.Fatalf("event %s: session %q data %v", e.Kind, e.SessionID, e.Data)
		}
	}
}
