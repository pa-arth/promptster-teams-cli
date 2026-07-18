package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestReadAiTouchedPaths is the first unit test for the AI-paths ledger:
// two sessions each record a path, and the reader must map each relPath back to
// the session that touched it.
func TestReadAiTouchedPaths(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	recordAiTouchedPath("sess-A", "", "src/foo.go")
	recordAiTouchedPath("sess-B", "", "src/bar.go")

	got := readAiTouchedPaths("")
	if got["src/foo.go"] != "sess-A" {
		t.Errorf("foo.go => %q, want sess-A", got["src/foo.go"])
	}
	if got["src/bar.go"] != "sess-B" {
		t.Errorf("bar.go => %q, want sess-B", got["src/bar.go"])
	}
}

// TestReadAiTouchedPathsExpiresByTTL ages one session's timestamp past
// aiPathsTTL on disk and asserts the reader drops its paths while keeping the
// fresh session's.
func TestReadAiTouchedPathsExpiresByTTL(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	recordAiTouchedPath("fresh", "", "keep.go")
	recordAiTouchedPath("stale", "", "drop.go")

	// Rewrite the "stale" session's timestamp to well beyond the TTL window.
	data, err := os.ReadFile(aiPathsLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	var ledger aiPathsLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	e := ledger.Sessions["stale"]
	e.TsMs = time.Now().Add(-aiPathsTTL - time.Hour).UnixMilli()
	ledger.Sessions["stale"] = e
	out, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aiPathsLedgerPath(), out, 0o600); err != nil {
		t.Fatal(err)
	}

	got := readAiTouchedPaths("")
	if _, ok := got["drop.go"]; ok {
		t.Error("path from an expired session must be excluded by TTL")
	}
	if got["keep.go"] != "fresh" {
		t.Errorf("keep.go => %q, want fresh", got["keep.go"])
	}
}

// TestReadAiTouchedPathsTieBreakDeterministic: two sessions touch the same path
// with an identical timestamp; the reader must return the lexicographically
// smaller sessionId, and it must be stable across repeated reads.
func TestReadAiTouchedPathsTieBreakDeterministic(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	recordAiTouchedPath("sess-B", "", "shared.go")
	recordAiTouchedPath("sess-A", "", "shared.go")

	// Force an exact-ts tie on disk (the API stamps time.Now, which differs).
	data, err := os.ReadFile(aiPathsLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	var ledger aiPathsLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	a, b := ledger.Sessions["sess-A"], ledger.Sessions["sess-B"]
	tie := time.Now().UnixMilli()
	a.TsMs, b.TsMs = tie, tie
	ledger.Sessions["sess-A"], ledger.Sessions["sess-B"] = a, b
	out, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aiPathsLedgerPath(), out, 0o600); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		got := readAiTouchedPaths("")
		if got["shared.go"] != "sess-A" {
			t.Fatalf("read %d: shared.go => %q, want deterministic sess-A", i, got["shared.go"])
		}
	}
}

// TestReadAiTouchedPathsWorkspaceScoping: two sessions under different root keys
// touch the same-named path; a scoped read returns only the session under that
// root, and an unscoped read still matches.
func TestReadAiTouchedPathsWorkspaceScoping(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	recordAiTouchedPath("sess-X", "keyX", "README.md")
	recordAiTouchedPath("sess-Y", "keyY", "README.md")

	if got := readAiTouchedPaths("keyX"); got["README.md"] != "sess-X" {
		t.Errorf("keyX scope: README.md => %q, want sess-X", got["README.md"])
	}
	if got := readAiTouchedPaths("keyY"); got["README.md"] != "sess-Y" {
		t.Errorf("keyY scope: README.md => %q, want sess-Y", got["README.md"])
	}
	if got := readAiTouchedPaths(""); got["README.md"] == "" {
		t.Error("unscoped read should still match a known-rootKey entry")
	}
}

// TestGitWatchRootKeyCanonical: a symlinked and a resolved spelling of the same
// directory must hash to the same key.
func TestGitWatchRootKeyCanonical(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if gitWatchRootKey(real) != gitWatchRootKey(link) {
		t.Errorf("symlinked and resolved keys differ: %q vs %q", gitWatchRootKey(real), gitWatchRootKey(link))
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
