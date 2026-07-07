package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// fileDiffEvent builds a minimal file_diff event for dedup tests.
func fileDiffEvent(path, diff string) event.Event {
	e := event.NewEvent("file_diff", "sess-1")
	e.Data = map[string]interface{}{"path": path, "diff": diff}
	return e
}

func TestDedupeFileDiffAcrossChannels(t *testing.T) {
	// Isolate the ledger under a temp state dir.
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)

	ws := t.TempDir()
	target := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(target, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Channel 1 (AI) emits first → wins.
	ai := fileDiffEvent("a.txt", "@@\n+hello world")
	if !dedupeFileDiff(ws, &ai) {
		t.Fatal("first emission should be allowed")
	}

	// Channel 2 (git watcher) sees the SAME resulting content → deduped.
	git := fileDiffEvent("a.txt", "diff --git a/a.txt b/a.txt\n+hello world")
	if dedupeFileDiff(ws, &git) {
		t.Error("second emission of identical content should be deduped")
	}

	// A genuine human edit changes the content → NOT deduped (override signal).
	if err := os.WriteFile(target, []byte("hello world EDITED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	human := fileDiffEvent("a.txt", "diff --git a/a.txt b/a.txt\n+EDITED")
	if !dedupeFileDiff(ws, &human) {
		t.Error("a new resulting content must be emitted, not deduped")
	}
}

func TestDedupeIgnoresNonFileDiff(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	cmd := event.NewEvent("command", "sess-1")
	cmd.Data = map[string]interface{}{"command": "ls"}
	if !dedupeFileDiff("/ws", &cmd) {
		t.Error("non-file_diff events must always pass through")
	}
}
