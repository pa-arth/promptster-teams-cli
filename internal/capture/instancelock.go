package capture

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// watchLockPath is the process-lifetime lock guarding the single `watch`
// supervisor. It lives next to supervisor.json under StateDir so `stop`'s state
// sweep and this lock share one directory.
func watchLockPath() string { return filepath.Join(state.StateDir(), "watch.lock") }

// watchRunning reports whether a `watch` supervisor is alive, by any launch path
// — manual `start`, a foreground `watch`, or the autostart service (which never
// writes supervisor.json). It reads the PID the lock holder stamped into
// watch.lock and confirms the process is live, self-healing a stale file the way
// DaemonStatus does. The PID is written at offset 0, outside the byte range the
// Windows lock covers, so this read never contends with the holder's lock.
// WatchRunning is the exported view of watchRunning for status/doctor, so they
// report capture started by any path — manual `start`, foreground `watch`, or
// the autostart service (which never writes supervisor.json).
func WatchRunning() (pid int, running bool) { return watchRunning() }

func watchRunning() (pid int, running bool) {
	data, err := os.ReadFile(watchLockPath())
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if processExists(pid) {
		return pid, true
	}
	return 0, false
}
