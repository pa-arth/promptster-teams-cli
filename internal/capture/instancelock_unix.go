//go:build !windows

package capture

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// acquireWatchLock takes a process-lifetime exclusive lock so only one `watch`
// supervisor runs regardless of how it was launched (manual `start`, foreground
// `watch`, or the autostart service). On success it stamps our PID into the file
// for watchRunning/status and returns a release func that MUST be held for the
// process lifetime (closing the fd releases the flock). ok=false means another
// watcher already holds the lock — the caller should exit without capturing so
// the seat-utilization metric isn't double-counted. err is reserved for real
// filesystem failures, not contention.
//
// flock is whole-file and advisory on unix, so watchRunning's plain read of the
// PID is unaffected by the lock.
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Stamp our PID (truncate first so a shorter PID can't leave stale trailing
	// bytes from a previous holder). Held-fd writes need no extra locking.
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
	_ = f.Sync()
	release = func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return release, true, nil
}

// watchLockHeld reports whether a live watcher currently holds the lock, by
// probing it non-destructively: open a fresh fd and try a non-blocking exclusive
// flock. flock is per-open-file-description, so this contends even with a lock
// held by another fd in the same process. Liveness comes from the lock (a dead
// holder's flock is auto-released by the kernel), making it immune to PID reuse.
// A missing lock file means nobody has ever locked → not running.
func watchLockHeld() bool {
	// #nosec G304 -- path is watchLockPath() derived from StateDir(), not user input.
	f, err := os.OpenFile(watchLockPath(), os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err == syscall.EWOULDBLOCK // still held by a live watcher
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
