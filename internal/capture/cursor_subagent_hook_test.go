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
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	touch := func(parts ...string) string {
		p := filepath.Join(append([]string{home, "projects"}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	child := touch("ws", "agent-transcripts", "parent-1", "subagents", "child-1.jsonl")
	// The same child uuid under an older, unclaimed project dir must not win.
	touch("old-ws", "agent-transcripts", "stale-parent", "subagents", "child-1.jsonl")

	if p, _ := cursorSubagentTranscript("child-1"); p != "" {
		t.Fatalf("resolved %q before the parent was ever claimed", p)
	}
	recordCursorHookClaim(touch("ws", "agent-transcripts", "parent-1", "parent-1.jsonl"), "parent-1")

	parent, path := cursorSubagentTranscript("child-1")
	if parent != "parent-1" || path != child {
		t.Fatalf("lookup = %q %q", parent, path)
	}
	if p, _ := cursorSubagentTranscript("parent-1"); p != "" {
		t.Fatalf("main-chain session resolved as subagent of %q", p)
	}
	if p, _ := cursorSubagentTranscript("../parent-1"); p != "" {
		t.Fatal("path separators in the id must not match")
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
