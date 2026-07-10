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

// watchRunning reports whether a `watch` supervisor is alive. Liveness is
// derived from the lock itself (watchLockHeld, a non-destructive try-lock), NOT
// from the stored PID: a dead holder's lock is auto-released by the OS, so this
// is immune to PID reuse. A stale watch.lock left after a crash/reboot whose old
// PID has since been recycled by an unrelated process would otherwise read as
// "running" and silently suppress capture (start/status bail). The stamped PID
// is read only for display, once we know a live holder exists.
func watchRunning() (pid int, running bool) {
	if !watchLockHeld() {
		return 0, false
	}
	// A live watcher holds the lock — read the PID it stamped (offset 0, outside
	// the Windows lock range) for display. Best-effort.
	if data, err := os.ReadFile(watchLockPath()); err == nil {
		if p, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && p > 0 {
			pid = p
		}
	}
	return pid, true
}
