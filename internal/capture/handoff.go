package capture

import (
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/selfupdate"
)

// A HANDOFF IS THE ONE CASE WHERE "ALREADY RUNNING" IS THE WRONG ANSWER.
//
// The single-instance lock exists so a second watcher can never double-count,
// and bowing out of it is almost always right. The exception is a process that
// was spawned specifically to REPLACE the holder.
//
// On unix that case cannot arise: execve replaces the image in place, so there
// is only ever one process and the flock fd (O_CLOEXEC) is released and
// re-acquired by the same pid. Windows has no execve, so selfupdate's reexecInto
// spawns a detached child and exits — and for the few milliseconds until the
// parent's handles actually drop, the child is racing a lock the parent still
// holds. Losing that race is not a retry-later: the child exits, the parent has
// already gone, and the machine captures nothing until the next login. Both the
// Windows self-update swap and the on-disk catch-up land here, so the window is
// hit on every update rather than never.
//
// The marker is what keeps the fix narrow. A blanket retry would make a human
// double-starting capture sit there for 20 seconds before being told what is
// obvious immediately; only a process we spawned ourselves waits.
const (
	// handoffLockWait is generous because it is nearly free: the parent calls
	// os.Exit immediately after spawning us, so the real wait is milliseconds and
	// the ceiling is only ever paid on a machine that is already broken.
	handoffLockWait = 20 * time.Second
	handoffLockPoll = 200 * time.Millisecond
)

// consumeHandoffMarker reports whether this process was spawned to replace a
// running watcher, and clears the marker so it cannot be inherited.
func consumeHandoffMarker() bool {
	marked := os.Getenv(selfupdate.EnvHandoff) != ""
	_ = os.Unsetenv(selfupdate.EnvHandoff)
	return marked
}

// awaitWatchLock retries acquireWatchLock until it succeeds or limit elapses.
// A timeout returns ok=false and no error — the caller then takes the ordinary
// "already running" path, which is the correct outcome once the handle really
// has not been released.
func awaitWatchLock(limit time.Duration) (release func(), ok bool, err error) {
	deadline := time.Now().Add(limit)
	for {
		release, ok, err = acquireWatchLock()
		if err != nil || ok {
			return release, ok, err
		}
		if !time.Now().Before(deadline) {
			return nil, false, nil
		}
		time.Sleep(handoffLockPoll)
	}
}
