package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// The Cursor hook runs in a process Cursor spawns, so its cwd is Cursor's choice
// and never the daemon's workspace. TaskRoot decides both the base
// RelativizeEventPaths strips and the key recordAiTouchedPath writes, so a wrong
// root does not mislabel a path — it writes ai-paths keys in a space the git
// watcher never reads, and every Cursor edit then attributes as human.
//
// Observed before the fix: 35 of 40 absolute Cursor file paths were under HOME
// and should have relativized, versus 0 of 33 for claude-code.

func writeCursorWatcherState(t *testing.T, watchDir string) {
	t.Helper()
	b, err := json.Marshal(cursorWatcherState{PID: 1234, WatchDir: watchDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorWatcherStatePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonWatchRootPrefersRecordedWorkspaceOverCwd(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	writeCursorWatcherState(t, "/Users/dev")

	if got := daemonWatchRoot(); got != "/Users/dev" {
		t.Fatalf("daemonWatchRoot() = %q, want the daemon's recorded workspace %q", got, "/Users/dev")
	}
}

func TestDaemonWatchRootFallsBackToTheClaudeRail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	// Cursor rail absent — the same daemon writes both, so the claude rail carries
	// the identical TaskRoot and covers the pre-first-heartbeat window.
	b, err := json.Marshal(claudeWatcherState{PID: 1234, WatchDir: "/Users/dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude-watcher.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	if got := daemonWatchRoot(); got != "/Users/dev" {
		t.Fatalf("daemonWatchRoot() = %q, want %q from the claude rail", got, "/Users/dev")
	}
}

func TestDaemonWatchRootIsEmptyWhenNothingRecorded(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	// Nothing recorded is NOT an error: the caller keeps loadSession's value, so
	// a machine that never ran the daemon behaves exactly as it did before.
	if got := daemonWatchRoot(); got != "" {
		t.Fatalf("daemonWatchRoot() = %q, want empty", got)
	}
}

func TestDaemonWatchRootHonoursAStaleEntry(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	// pid 0 / no heartbeat: the daemon is not running. The root is still the space
	// every existing ai-paths key was written under, so honouring it is what keeps
	// the hook's writes readable. Falling back to cwd here would emit keys nothing
	// can ever look up — a liveness check would actively cause the bug.
	b, err := json.Marshal(cursorWatcherState{PID: 0, WatchDir: "/Users/dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorWatcherStatePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}

	if got := daemonWatchRoot(); got != "/Users/dev" {
		t.Fatalf("daemonWatchRoot() = %q, want the recorded workspace even when stale", got)
	}
}

// The end-to-end property the fix exists for: an absolute path under the
// daemon's workspace must reach the ai-paths ledger as a WORKSPACE-RELATIVE key,
// because that is the space reconcileCommitAttribution looks up.
func TestCursorEditReachesTheLedgerInTheWatcherKeySpace(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)

	home := t.TempDir()
	writeCursorWatcherState(t, home)

	rel := filepath.Join("repos", "myrepo", "main.go")
	abs := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The cwd Cursor happened to spawn the hook in. Passing this through — which
	// is what the code did before — is the whole bug, so the test must go through
	// the same chooser the hook calls rather than reaching for the daemon root
	// itself. Otherwise it would pass against the unfixed code and prove nothing.
	cursorSpawnCwd := t.TempDir()
	root := cursorHookTaskRoot(cursorSpawnCwd)
	if root != home {
		t.Fatalf("cursorHookTaskRoot(%q) = %q, want the daemon workspace %q",
			cursorSpawnCwd, root, home)
	}

	// Exactly what emitCursorEvent does, in order: relativize against TaskRoot,
	// then let dedupeFileDiff write the ledger entry.
	ev := event.Event{
		SessionID: "sess-1",
		Kind:      "file_diff",
		Source:    "cursor",
		Data: map[string]interface{}{
			"path": abs, "linesAdded": 1, "linesRemoved": 0,
		},
		Provenance: &event.Provenance{Attribution: "likely_ai", Methods: []string{"cursor-hook"}},
	}
	normalize.RelativizeEventPaths(&ev, root)
	dedupeFileDiff(root, &ev)

	var ledger aiPathsLedger
	data, err := os.ReadFile(aiPathsLedgerPath())
	if err != nil {
		t.Fatalf("no ai-paths ledger written: %v", err)
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	paths := ledger.Sessions["sess-1"].Paths

	if _, ok := paths[rel]; !ok {
		got := make([]string, 0, len(paths))
		for p := range paths {
			got = append(got, p)
		}
		t.Fatalf("ai-paths ledger has %v, want the workspace-relative key %q.\n"+
			"An absolute key here IS the bug: reconcileCommitAttribution looks up "+
			"scope.ledgerPath(<workspace-relative>) and would never match it, so the "+
			"edit attributes as human and durability never sees it.", got, rel)
	}
	for p := range paths {
		if filepath.IsAbs(p) {
			t.Fatalf("ledger key %q is absolute; keys must be workspace-relative", p)
		}
	}
}
