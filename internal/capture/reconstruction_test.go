package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reconstructionFixture stands up an isolated state dir plus a Claude projects
// tree, and returns the projects root.
func reconstructionFixture(t *testing.T) string {
	t.Helper()
	root := claudeProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	resetProgressReplayAnnouncements()
	resetProgressFaultReports()
	t.Cleanup(func() {
		resetProgressReplayAnnouncements()
		resetProgressFaultReports()
	})
	return root
}

// seedClaudeProgress writes a progress file claiming `path` matched and was read
// up to `offset`.
func seedClaudeProgress(t *testing.T, path string, offset int64) {
	t.Helper()
	p := claudeWatchProgress{
		Offsets: map[string]int64{claudeProgressKey(path): offset},
		Match:   map[string]string{claudeProgressKey(path): "yes"},
		V:       claudeProgressSchemaV,
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeWatchProgressPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ageFile backdates a file so it reads as history rather than a live tail.
func ageFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// An old transcript with unread bytes IS reconstruction, and the numbers are
// what REMAINS — "3 files, 40 MB left" is actionable, "read 900 MB" is trivia
// and on a resumed replay is not even recoverable.
func TestAnUnreadOldTranscriptCountsAsReconstruction(t *testing.T) {
	root := reconstructionFixture(t)
	ws := t.TempDir()
	path, size := writeClaudeHistory(t, root, resolvePath(ws), "old.jsonl", 20)
	ageFile(t, path, 6*24*time.Hour)
	seedClaudeProgress(t, path, 0)

	got := ReconstructionNow()

	if !got.Running {
		t.Fatal("an old matched transcript read to byte 0 is a pending replay")
	}
	if got.Files != 1 {
		t.Errorf("Files = %d, want 1", got.Files)
	}
	if got.Bytes != size {
		t.Errorf("Bytes = %d, want %d (what REMAINS, not what was read)", got.Bytes, size)
	}
	if got.Oldest.IsZero() {
		t.Error("Oldest is zero — the date the replay has reached is the answer to " +
			"'how much of the window remains'")
	}
}

// THE LIVE TAIL IS NOT RECONSTRUCTION, and this is the distinction the whole
// file turns on. Every active session has unread bytes for a few hundred
// milliseconds each time someone types; counting those would report every
// working laptop as mid-replay and make the signal worthless.
func TestALiveTailIsNotReconstruction(t *testing.T) {
	root := reconstructionFixture(t)
	ws := t.TempDir()
	path, _ := writeClaudeHistory(t, root, resolvePath(ws), "live.jsonl", 20)
	ageFile(t, path, 30*time.Second) // written seconds ago
	seedClaudeProgress(t, path, 0)

	if got := ReconstructionNow(); got.Running {
		t.Errorf("a transcript written 30s ago is being TAILED, not reconstructed; got %+v", got)
	}
}

// A transcript already read to EOF is finished work and contributes nothing.
func TestAFullyReadTranscriptIsNotPending(t *testing.T) {
	root := reconstructionFixture(t)
	ws := t.TempDir()
	path, size := writeClaudeHistory(t, root, resolvePath(ws), "done.jsonl", 20)
	ageFile(t, path, 6*24*time.Hour)
	seedClaudeProgress(t, path, size)

	if got := ReconstructionNow(); got.Running {
		t.Errorf("a transcript read to EOF is not pending replay; got %+v", got)
	}
}

// An unclassified or cwd-mismatched transcript is not work in progress.
// Counting it would report a replay that is never going to happen, and on a
// machine with unrelated projects that is most of the disk.
func TestAnUnmatchedTranscriptIsNotPending(t *testing.T) {
	root := reconstructionFixture(t)
	ws := t.TempDir()
	path, _ := writeClaudeHistory(t, root, resolvePath(ws), "other.jsonl", 20)
	ageFile(t, path, 6*24*time.Hour)

	p := claudeWatchProgress{
		Offsets: map[string]int64{},
		Match:   map[string]string{claudeProgressKey(path): "no"},
		V:       claudeProgressSchemaV,
	}
	data, _ := json.Marshal(p)
	if err := os.WriteFile(claudeWatchProgressPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if got := ReconstructionNow(); got.Running {
		t.Errorf("a cwd-mismatched transcript will never be replayed; got %+v", got)
	}
}

// --- §2.5: a lost or unreadable progress file --------------------------------

// A MISSING progress file is a fresh install. Silent, correctly: there is no
// history to lose. Warning here would fire on every first run.
func TestAMissingProgressFileIsSilent(t *testing.T) {
	reconstructionFixture(t)
	var buf strings.Builder
	prev := progressReplayOut
	progressReplayOut = &buf
	t.Cleanup(func() { progressReplayOut = prev })

	_ = loadClaudeWatchProgress()

	if got := buf.String(); got != "" {
		t.Errorf("a fresh install warned about its own absence:\n%s", got)
	}
}

// A CORRUPT progress file costs the same full re-read as a schema bump, except
// nobody chose it and nothing announced it. It must say so.
func TestACorruptProgressFileIsReported(t *testing.T) {
	reconstructionFixture(t)
	if err := os.WriteFile(claudeWatchProgressPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	prev := progressReplayOut
	progressReplayOut = &buf
	t.Cleanup(func() { progressReplayOut = prev })

	_ = loadClaudeWatchProgress()

	got := buf.String()
	if got == "" {
		t.Fatal("a corrupt progress file silently re-read the whole window — that silence " +
			"is the entire failure this reports")
	}
	for _, want := range []string{"CORRUPT", "LOST", "28 days", "every poll"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
}

// The fault REPEATS on every poll — the load fails again three seconds later and
// the save that would fix it writes to the same broken path. So it is rate
// limited rather than latched: an operator arriving an hour in must still see
// it, but it must not become the log.
func TestAProgressFileFaultIsRateLimitedNotSilenced(t *testing.T) {
	reconstructionFixture(t)
	if err := os.WriteFile(claudeWatchProgressPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	prev := progressReplayOut
	progressReplayOut = &buf
	t.Cleanup(func() { progressReplayOut = prev })

	now := time.Now()
	prevClock := progressFaultClock
	progressFaultClock = func() time.Time { return now }
	t.Cleanup(func() { progressFaultClock = prevClock })

	// Twenty polls inside one interval: one report.
	for i := 0; i < 20; i++ {
		_ = loadClaudeWatchProgress()
	}
	if n := strings.Count(buf.String(), "CORRUPT"); n != 1 {
		t.Errorf("reported %d times within one interval, want 1 — 3s polls would make this the log", n)
	}

	// Past the interval: it says so again, because the fault is still there and a
	// warning that scrolled past an hour ago is close to silent.
	now = now.Add(progressFaultRepeatInterval + time.Second)
	_ = loadClaudeWatchProgress()
	if n := strings.Count(buf.String(), "CORRUPT"); n != 2 {
		t.Errorf("reported %d times total, want 2 — an unfixed fault must keep saying so", n)
	}
}

// Claude and Codex fault independently; one must not silence the other.
func TestProgressFileFaultsAreReportedPerWatcher(t *testing.T) {
	reconstructionFixture(t)
	for _, p := range []string{claudeWatchProgressPath(), codexWatchProgressPath()} {
		if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf strings.Builder
	prev := progressReplayOut
	progressReplayOut = &buf
	t.Cleanup(func() { progressReplayOut = prev })

	_ = loadClaudeWatchProgress()
	_ = loadCodexWatchProgress()

	got := buf.String()
	if !strings.Contains(got, "claude-watcher") || !strings.Contains(got, "codex-watcher") {
		t.Errorf("each watcher reports its own progress-file fault:\n%s", got)
	}
}

// The report names the actual file, because "check the state directory" without
// a path is a support ticket rather than a fix.
func TestTheFaultReportNamesTheFile(t *testing.T) {
	reconstructionFixture(t)
	if err := os.WriteFile(claudeWatchProgressPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	prev := progressReplayOut
	progressReplayOut = &buf
	t.Cleanup(func() { progressReplayOut = prev })

	_ = loadClaudeWatchProgress()

	if !strings.Contains(buf.String(), filepath.Base(claudeWatchProgressPath())) {
		t.Errorf("the report must name the file:\n%s", buf.String())
	}
}
