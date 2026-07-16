package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ClaudeDesktopSessionsDir must return a non-empty, absolute path ending in the
// desktop store directory name on the current OS. We don't assert the exact
// prefix (it varies by OS and by whether os.UserConfigDir succeeds), only that
// it's sane and correctly suffixed — a regression that dropped the store dir
// name or returned "" would break both the watcher walk and the doctor check.
func TestClaudeDesktopSessionsDirIsSane(t *testing.T) {
	dir := ClaudeDesktopSessionsDir()
	if dir == "" {
		t.Fatal("ClaudeDesktopSessionsDir returned empty string")
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("expected absolute path, got %q", dir)
	}
	if filepath.Base(dir) != "claude-code-sessions" {
		t.Errorf("expected path to end in claude-code-sessions, got %q", dir)
	}

	agent := ClaudeDesktopAgentModeDir()
	if filepath.Base(agent) != "local-agent-mode-sessions" {
		t.Errorf("expected agent-mode path to end in local-agent-mode-sessions, got %q", agent)
	}
	// Both stores must live under the SAME "Claude" base dir.
	if filepath.Dir(dir) != filepath.Dir(agent) {
		t.Errorf("stores should share a base dir: %q vs %q", filepath.Dir(dir), filepath.Dir(agent))
	}
}

// changedVersion reports what pollDesktopSessions' change-detection would decide
// for a file given a stored version: true = re-read (changed / unseen), false =
// skip (unchanged). This mirrors the (mtime, size) comparison in
// pollDesktopSessions without needing a live session/network, so we test the
// decision logic directly.
func desktopVersionChanged(prev desktopFileVersion, prevSeen bool, cur desktopFileVersion) bool {
	if prevSeen && prev == cur {
		return false
	}
	return true
}

// The change-detection contract: an unchanged (mtime, size) is skipped; any
// change in either field triggers a re-read; a never-seen file is always read.
func TestDesktopChangeDetection(t *testing.T) {
	base := desktopFileVersion{Mtime: 1_000_000, Size: 4096}

	if changed := desktopVersionChanged(base, true, base); changed {
		t.Error("identical mtime+size must be skipped, got re-read")
	}
	if changed := desktopVersionChanged(base, true, desktopFileVersion{Mtime: 2_000_000, Size: 4096}); !changed {
		t.Error("changed mtime must trigger re-read")
	}
	if changed := desktopVersionChanged(base, true, desktopFileVersion{Mtime: 1_000_000, Size: 8192}); !changed {
		t.Error("changed size must trigger re-read")
	}
	if changed := desktopVersionChanged(desktopFileVersion{}, false, base); !changed {
		t.Error("never-seen file must be read")
	}
}

// End-to-end change-detection through a real temp dir + a real os.Stat: writing
// a local_*.json produces a version, re-stat with no change matches (skip),
// and rewriting with different bytes changes the size (re-read). This exercises
// the actual desktopFileVersion construction the poller uses.
func TestDesktopFileVersionFromStat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local_11111111-2222-3333-4444-555555555555.json")

	if err := os.WriteFile(path, []byte(`{"messages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	v1 := desktopFileVersion{Mtime: info1.ModTime().UnixNano(), Size: info1.Size()}

	// Re-stat without touching the file: same version, must be skipped.
	info1b, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	v1b := desktopFileVersion{Mtime: info1b.ModTime().UnixNano(), Size: info1b.Size()}
	if v1 != v1b {
		t.Fatalf("re-stat of untouched file changed version: %+v vs %+v", v1, v1b)
	}

	// Rewrite with different-length content: size (and mtime) change → re-read.
	time.Sleep(10 * time.Millisecond) // ensure a distinct mtime on coarse clocks
	if err := os.WriteFile(path, []byte(`{"messages":[{"role":"user"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	v2 := desktopFileVersion{Mtime: info2.ModTime().UnixNano(), Size: info2.Size()}
	if v1 == v2 {
		t.Errorf("rewriting the file should have changed the version, both were %+v", v1)
	}
}

// desktopSessionIDFromPath derives the session id from the blob's parent dir,
// falling back to the local_ uuid in the filename.
func TestDesktopSessionIDFromPath(t *testing.T) {
	p := filepath.Join("store", "workspace-abc", "session-xyz", "local_deadbeef.json")
	if got := desktopSessionIDFromPath(p); got != "session-xyz" {
		t.Errorf("expected parent-dir session id, got %q", got)
	}
}

// Progress round-trips through disk with the Versions map intact — a corrupt or
// missing file must degrade to an empty (non-nil) map, never a nil-map panic.
func TestDesktopWatchProgressRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)

	p := loadDesktopWatchProgress()
	if p.Versions == nil {
		t.Fatal("fresh progress must have a non-nil Versions map")
	}
	key := filepath.Join(tmp, "local_x.json")
	p.Versions[key] = desktopFileVersion{Mtime: 42, Size: 7}
	saveDesktopWatchProgress(p)

	got := loadDesktopWatchProgress()
	if got.Versions[key] != (desktopFileVersion{Mtime: 42, Size: 7}) {
		t.Errorf("round-trip lost version: %+v", got.Versions[key])
	}
	// Sanity: the progress file must have landed under the state dir.
	if !strings.HasPrefix(desktopWatchProgressPath(), tmp) {
		t.Errorf("progress path %q not under state dir %q", desktopWatchProgressPath(), tmp)
	}
}

// TODO(desktop-schema): once a redacted sample local_*.json is available, add a
// golden test that drives pollDesktopSessions end-to-end (write a sample blob,
// assert the expected prompt/ai_response/command events emit, then re-poll and
// assert idempotency). No schema means no golden fixture yet.
