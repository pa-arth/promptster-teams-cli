package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

// The capture process records WHICH BINARY it is actually executing, because
// nothing else on the machine can answer that question.
//
// Every version surface we had reports the binary that ran the COMMAND — `doctor`
// prints version.Version of the foreground process, and under `npx` that is a
// binary npm downloaded seconds earlier. The daemon is a different, long-lived
// process that may have been started months ago from a different path, and the
// two are routinely NOT the same build:
//
//   - self-update swaps the file and re-execs, so a daemon that cannot update
//     (unwritable dir, a lockfile pin, a path npm deleted out from under it)
//     stays on old code indefinitely while the foreground binary moves on;
//   - autostart bakes an ABSOLUTE path, so launchd can be running a completely
//     different file from the one on PATH;
//   - a daemon holds its inode after its file is deleted, so it keeps capturing
//     from a build that no longer exists on disk.
//
// In every one of those the machine LOOKS healthy: capture is running, doctor is
// green, and features shipped weeks ago are silently absent (this cost a customer
// their whole Cursor rail — the hook enrollment lives at watch startup, so a
// daemon that never restarted never enrolled). Recording it here turns a
// multi-hour support thread into one line of `doctor`.
//
// This file is DIAGNOSTIC ONLY. Nothing gates capture on it, and an absent or
// unreadable record degrades to "unknown", never to a claim.
type watchRuntime struct {
	PID       int    `json:"pid"`
	Version   string `json:"version"`
	Exe       string `json:"exe,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
}

func watchRuntimePath() string { return filepath.Join(state.StateDir(), "watch-runtime.json") }

// recordWatchRuntime stamps the running build. Called once the watch lock is
// held, so the record always describes the process that owns capture — and
// called again after a self-update re-exec, because a re-exec re-enters
// RunTeamsWatch from the top.
//
// Best-effort: a failure here must never stop capture. The cost of losing it is
// a "version unknown" line in doctor.
func recordWatchRuntime() {
	exe, _ := os.Executable()
	data, err := json.Marshal(watchRuntime{
		PID:       os.Getpid(),
		Version:   version.Version,
		Exe:       exe,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return
	}
	path := watchRuntimePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func clearWatchRuntime() { _ = os.Remove(watchRuntimePath()) }

// CaptureProcess describes the capture process as it exists right now.
type CaptureProcess struct {
	// Running comes from the watch lock — the same authoritative signal `stop`
	// uses, immune to PID reuse.
	Running bool
	PID     int
	// Version and Exe describe the build that is actually capturing. They are
	// EMPTY when unknown, and unknown is a real and common answer: any daemon
	// started by a build that predates this record leaves them blank. Callers
	// must treat empty as "cannot tell", never as "matches".
	Version string
	Exe     string
}

// RunningCapture reports the live capture process and, when it can be proven,
// the build it is running.
//
// The proof is the PID: the recorded runtime is only trusted when its PID is the
// one holding the lock right now. A record left behind by a dead daemon would
// otherwise let doctor print a version for a process that no longer exists —
// which is worse than printing nothing, because the whole point of the line is
// to be believed.
func RunningCapture() CaptureProcess {
	pid, running := watchRunning()
	if !running {
		return CaptureProcess{}
	}
	cp := CaptureProcess{Running: true, PID: pid}
	data, err := os.ReadFile(watchRuntimePath())
	if err != nil {
		return cp
	}
	var rt watchRuntime
	if err := json.Unmarshal(data, &rt); err != nil {
		return cp
	}
	if rt.PID <= 0 || pid <= 0 || rt.PID != pid {
		return cp
	}
	cp.Version = rt.Version
	cp.Exe = rt.Exe
	return cp
}
