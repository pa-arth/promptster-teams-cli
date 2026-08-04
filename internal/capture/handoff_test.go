package capture

import (
	"os"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/selfupdate"
)

// Windows has no execve, so a self-update or catch-up spawns a replacement and
// exits while still holding the lock. If the replacement bows out of that race,
// the parent is already gone and nothing captures until the next login — so it
// has to wait for the handle to drop.
func TestAwaitWatchLockWaitsForTheOutgoingWatcherToRelease(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	release, ok, err := acquireWatchLock()
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()

	start := time.Now()
	got, ok, err := awaitWatchLock(10 * time.Second)
	if err != nil || !ok {
		t.Fatalf("awaitWatchLock: ok=%v err=%v — the handoff lost capture entirely", ok, err)
	}
	defer got()
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("returned after %v — it cannot have contended for a lock that was still held", elapsed)
	}
}

// A timeout is not an error: past the ceiling the holder really is staying, and
// the caller must fall through to the ordinary "already running" path rather
// than failing the command.
func TestAwaitWatchLockGivesUpQuietlyWhenTheHolderStays(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	release, ok, err := acquireWatchLock()
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer release()

	got, ok, err := awaitWatchLock(300 * time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v, want nil — a timeout is the ordinary already-running case", err)
	}
	if ok || got != nil {
		t.Fatalf("ok=%v release=%v, want a clean refusal", ok, got != nil)
	}
}

// The marker must not survive into the process's own environment: unix re-execs
// with os.Environ(), so a marker left set would turn a later ordinary start into
// a silent 20-second waiter instead of an immediate "already running".
func TestConsumeHandoffMarkerReadsOnceAndClears(t *testing.T) {
	t.Setenv(selfupdate.EnvHandoff, "1")
	if !consumeHandoffMarker() {
		t.Fatal("marked process read as unmarked")
	}
	if v := os.Getenv(selfupdate.EnvHandoff); v != "" {
		t.Errorf("marker still set to %q after being consumed", v)
	}
	if consumeHandoffMarker() {
		t.Error("second read still reports a handoff — the marker would be inherited by a re-exec")
	}
}
