package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A DEVICE THAT CANNOT SAVE ITS PROGRESS REPLAYS THE WHOLE WINDOW ON EVERY
// RESTART, AND UNTIL NOW SAID NOTHING.
//
// The read-side report shipped in capture-delivery-lanes §2.5 cannot cover this,
// and that is the trap worth stating: a state directory that is read-only or
// full still SERVES the existing file perfectly. The read succeeds, it parses,
// it returns stale offsets. Nothing is corrupt. The machine with the problem is
// exactly the machine that looks healthy.

// writeFaultFixture isolates the state dir and clears both fault registries.
func writeFaultFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	resetProgressFaultReports()
	t.Cleanup(resetProgressFaultReports)
	return dir
}

// captureFaultOut redirects the report stream and returns the buffer.
func captureFaultOut(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := progressReplayOut
	progressReplayOut = &buf
	t.Cleanup(func() { progressReplayOut = prev })
	return &buf
}

// sealDir makes a directory reject new files, and restores it on cleanup so the
// temp dir can still be removed.
func sealDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// The real syscall path, not an injected error. The point is that an actual
// read-only state dir reaches the report — a fault injector would prove the
// report works while saying nothing about whether anything calls it.
func TestAReadOnlyStateDirIsReported(t *testing.T) {
	dir := writeFaultFixture(t)
	buf := captureFaultOut(t)
	sealDir(t, dir)

	saveClaudeWatchProgress(claudeWatchProgress{V: claudeProgressSchemaV})

	got := buf.String()
	if got == "" {
		t.Fatal("a device that cannot persist progress said nothing — that silence is the " +
			"whole failure, and no read-side report can fire here because the old file still reads fine")
	}
	// The consequence SPECIFIC to this fault. "28 days will be re-read" alone
	// duplicates the read-fault report and omits what makes this one worse.
	if !strings.Contains(got, "RESTART") {
		t.Errorf("the report must say the replay repeats on every restart:\n%s", got)
	}
	if !strings.Contains(got, filepath.Base(claudeWatchProgressPath())) {
		t.Errorf("the report must name the file — 'check the state directory' without a path "+
			"is a support ticket, not a fix:\n%s", got)
	}
}

// A successful temp write followed by a failed rename leaves the OLD file in
// place. This is the point `_ = os.Rename(...)` swallowed, and it is
// indistinguishable from a healthy save everywhere else: the offsets are just as
// unpersisted as if nothing had been written at all.
func TestAFailedCommitIsReported(t *testing.T) {
	dir := writeFaultFixture(t)
	buf := captureFaultOut(t)

	// A DIRECTORY at the destination path: the temp write succeeds, the rename
	// onto it cannot.
	if err := os.Mkdir(claudeWatchProgressPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = dir

	saveClaudeWatchProgress(claudeWatchProgress{V: claudeProgressSchemaV})

	if got := buf.String(); !strings.Contains(got, "COMMITTED") {
		t.Errorf("a failed rename left the old file in place and reported nothing:\n%s", got)
	}
	if !ProgressWriteFaultedHas("claude") {
		t.Error("a failed commit must register as a write fault")
	}
}

// A healthy save must not fire the fault path, or the warning becomes furniture.
func TestAHealthySaveIsSilent(t *testing.T) {
	writeFaultFixture(t)
	buf := captureFaultOut(t)

	saveClaudeWatchProgress(claudeWatchProgress{V: claudeProgressSchemaV})

	if got := buf.String(); got != "" {
		t.Errorf("a working device warned about itself:\n%s", got)
	}
	if len(ProgressWriteFaulted()) != 0 {
		t.Errorf("a healthy save left a fault registered: %v", ProgressWriteFaulted())
	}
}

// Once progress lands on disk the replay-on-restart cost is genuinely gone, so a
// transient full disk that recovers must stop warning. A flag that only ever
// sets leaves doctor red on a machine that fixed itself.
func TestASuccessfulSaveClearsTheFault(t *testing.T) {
	dir := writeFaultFixture(t)
	captureFaultOut(t)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	saveClaudeWatchProgress(claudeWatchProgress{V: claudeProgressSchemaV})
	if !ProgressWriteFaultedHas("claude") {
		t.Fatal("expected the sealed dir to register a fault")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	saveClaudeWatchProgress(claudeWatchProgress{V: claudeProgressSchemaV})

	if ProgressWriteFaultedHas("claude") {
		t.Error("the disk recovered and progress persisted, so the fault must clear — " +
			"a warning that never clears is furniture")
	}
}

// A state dir can fail both ways at once. Sharing one rate-limit key would let
// whichever fault reported first silence the other for fifteen minutes.
func TestTheReadAndWriteFaultsDoNotSilenceEachOther(t *testing.T) {
	dir := writeFaultFixture(t)
	buf := captureFaultOut(t)

	if err := os.WriteFile(claudeWatchProgressPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = loadClaudeWatchProgress() // read fault
	sealDir(t, dir)
	saveClaudeWatchProgress(claudeWatchProgress{V: claudeProgressSchemaV}) // write fault

	got := buf.String()
	if !strings.Contains(got, "CORRUPT") {
		t.Errorf("the read fault went missing:\n%s", got)
	}
	if !strings.Contains(got, "cannot SAVE") {
		t.Errorf("the write fault was silenced by the read fault's rate limiter:\n%s", got)
	}
}

// Rate limited, not latched. The save fails again on the next poll and the
// operator arriving an hour in must still see it — but a 3s poll loop must not
// become the log.
func TestTheWriteFaultIsRateLimitedNotSilenced(t *testing.T) {
	dir := writeFaultFixture(t)
	buf := captureFaultOut(t)
	sealDir(t, dir)

	now := time.Now()
	prev := progressFaultClock
	progressFaultClock = func() time.Time { return now }
	t.Cleanup(func() { progressFaultClock = prev })

	for i := 0; i < 20; i++ {
		saveClaudeWatchProgress(claudeWatchProgress{V: claudeProgressSchemaV})
	}
	if n := strings.Count(buf.String(), "cannot SAVE"); n != 1 {
		t.Errorf("reported %d times in one interval, want 1", n)
	}

	now = now.Add(progressFaultRepeatInterval + time.Second)
	saveClaudeWatchProgress(claudeWatchProgress{V: claudeProgressSchemaV})
	if n := strings.Count(buf.String(), "cannot SAVE"); n != 2 {
		t.Errorf("reported %d times total, want 2 — an unfixed fault must keep saying so", n)
	}
}

// The watchers fault independently; one must not stand in for the other.
func TestTheWatchersReportWriteFaultsIndependently(t *testing.T) {
	dir := writeFaultFixture(t)
	captureFaultOut(t)
	sealDir(t, dir)

	saveCodexWatchProgress(codexWatchProgress{V: codexProgressSchemaV})

	if got := ProgressWriteFaulted(); len(got) != 1 || got[0] != "codex" {
		t.Errorf("ProgressWriteFaulted() = %v, want [codex] only", got)
	}
}

// ProgressWriteFaultedHas is a test-side convenience over the exported slice.
func ProgressWriteFaultedHas(watcher string) bool {
	for _, w := range ProgressWriteFaulted() {
		if w == watcher {
			return true
		}
	}
	return false
}
