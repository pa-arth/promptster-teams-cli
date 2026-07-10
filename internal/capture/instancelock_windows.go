//go:build windows

package capture

import (
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/windows"
)

// acquireWatchLock takes a process-lifetime exclusive lock so only one `watch`
// supervisor runs regardless of how it was launched (manual `start`, foreground
// `watch`, or the autostart Task Scheduler job). ok=false means another watcher
// already holds it — the caller should exit without capturing so the
// seat-utilization metric isn't double-counted. err is reserved for real
// failures, not contention.
//
// Windows LockFileEx is mandatory, so watchRunning's read of the PID would fail
// if it fell inside the locked range. We stamp the PID at offset 0 and lock a
// single byte at a high offset (1<<32, beyond EOF and never written), leaving the
// PID freely readable.
func acquireWatchLock() (release func(), ok bool, err error) {
	path := watchLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	// #nosec G304 -- path is watchLockPath() derived from StateDir(), not user input.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	handle := windows.Handle(f.Fd())
	// Lock one byte at file offset 1<<32 (OffsetHigh=1), clear of the PID at 0.
	ol := &windows.Overlapped{OffsetHigh: 1}
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		_ = f.Close()
		if err == windows.ERROR_LOCK_VIOLATION {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Stamp our PID at offset 0 (truncate first so a shorter PID can't leave
	// stale trailing bytes from a previous holder).
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
	_ = f.Sync()
	release = func() {
		_ = windows.UnlockFileEx(handle, 0, 1, 0, ol)
		_ = f.Close()
	}
	return release, true, nil
}

// watchLockHeld reports whether a live watcher currently holds the lock, by
// probing the same high-offset byte range acquireWatchLock locks with a
// fail-immediately exclusive lock. Liveness comes from the lock (Windows
// releases it when the holder dies), so this is immune to PID reuse. A missing
// lock file means nobody has ever locked → not running.
func watchLockHeld() bool {
	// #nosec G304 -- path is watchLockPath() derived from StateDir(), not user input.
	f, err := os.OpenFile(watchLockPath(), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	handle := windows.Handle(f.Fd())
	ol := &windows.Overlapped{OffsetHigh: 1}
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		return err == windows.ERROR_LOCK_VIOLATION // still held by a live watcher
	}
	_ = windows.UnlockFileEx(handle, 0, 1, 0, ol)
	return false
}
