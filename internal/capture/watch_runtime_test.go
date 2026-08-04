package capture

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

// The record is only worth anything if it describes the process that is
// capturing RIGHT NOW, so it is written under the lock and read back only when
// its PID is the live holder's.
func TestRunningCaptureReportsTheBuildHoldingTheLock(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	release, ok, err := acquireWatchLock()
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer release()

	recordWatchRuntime()
	cp := RunningCapture()
	if !cp.Running || cp.PID != os.Getpid() {
		t.Fatalf("RunningCapture = %+v, want running pid %d", cp, os.Getpid())
	}
	if cp.Version != version.Version {
		t.Errorf("Version = %q, want %q", cp.Version, version.Version)
	}
	if cp.Exe == "" {
		t.Error("Exe should name the running binary — it is the diagnostic that says WHICH file is capturing")
	}

	clearWatchRuntime()
	if v := RunningCapture().Version; v != "" {
		t.Errorf("after clear, Version = %q, want \"\" (unknown)", v)
	}
}

// A record left by a dead daemon must NOT be attributed to whatever is holding
// the lock now. Printing a version for the wrong process is worse than printing
// nothing: the whole value of the line is that it can be believed, and this is
// the exact shape of the bug it exists to catch (a stale file describing a
// process that has moved on).
func TestRunningCaptureRefusesARecordFromAnotherProcess(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	release, ok, err := acquireWatchLock()
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer release()

	data, err := json.Marshal(watchRuntime{PID: os.Getpid() + 99999, Version: "9.9.9", Exe: "/ghost"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(watchRuntimePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	cp := RunningCapture()
	if !cp.Running {
		t.Fatal("the lock is held; capture must still read as running")
	}
	if cp.Version != "" || cp.Exe != "" {
		t.Errorf("a foreign record must not be reported: got %+v", cp)
	}
}

func TestRunningCaptureIsEmptyWhenNothingHoldsTheLock(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	if cp := RunningCapture(); cp.Running || cp.Version != "" {
		t.Errorf("no watcher: got %+v, want zero", cp)
	}
}

// isOlderVersion decides whether `start` restarts a live daemon, so both false
// directions are load-bearing: restarting on equal versions churns capture on
// every start, and restarting on an unstamped build kills a developer's own
// watcher mid-test.
func TestIsOlderVersionOnlyFiresOnAStrictlyOlderRelease(t *testing.T) {
	cases := []struct {
		running, ours string
		want          bool
	}{
		{"0.11.3", "0.12.2", true},
		{"0.12.1", "0.12.2", true},
		{"0.12.2", "0.12.2", false},
		{"0.13.0", "0.12.2", false},
		{"dev", "0.12.2", false},
		{"0.12.2", "dev", false},
		{"", "0.12.2", false},
		{"0.12.2", "", false},
	}
	for _, c := range cases {
		if got := isOlderVersion(c.running, c.ours); got != c.want {
			t.Errorf("isOlderVersion(%q, %q) = %v, want %v", c.running, c.ours, got, c.want)
		}
	}
}
