package capture

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWatchLockIsSingleInstance verifies the guard that keeps a login-launched
// watcher and a manual `start` from both capturing and double-counting the
// seat-utilization metric.
func TestWatchLockIsSingleInstance(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	rel1, ok1, err1 := acquireWatchLock()
	if err1 != nil || !ok1 {
		t.Fatalf("first acquire: ok=%v err=%v", ok1, err1)
	}
	defer rel1()

	// While held, watchRunning must report us (PID stamped, readable despite the
	// lock — the Windows lock sits at a high offset, clear of the PID at 0).
	if pid, running := watchRunning(); !running || pid != os.Getpid() {
		t.Fatalf("watchRunning = (%d, %v), want (%d, true)", pid, running, os.Getpid())
	}

	// A second acquire must fail (contention), not error.
	rel2, ok2, err2 := acquireWatchLock()
	if err2 != nil {
		t.Fatalf("second acquire errored: %v", err2)
	}
	if ok2 {
		rel2()
		t.Fatal("second acquire succeeded while the first was held — single-instance guard is broken")
	}
}

// TestWatchRunningIgnoresStaleReusedPID guards the PID-reuse trap: a stale
// watch.lock left after a crash/reboot, whose old PID has been recycled by an
// unrelated live process, must NOT read as running. Liveness comes from the
// lock (nobody holds it here), not the stored PID.
func TestWatchRunningIgnoresStaleReusedPID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	// PID 1 (init/launchd) always exists — a PID-based check would call this
	// "running"; a lock-based check must not, since no one holds the lock.
	if err := os.WriteFile(filepath.Join(dir, "watch.lock"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, running := watchRunning(); running {
		t.Fatalf("watchRunning reported running (pid %d) from a stale lock with a reused PID — must derive liveness from the lock", pid)
	}
}
