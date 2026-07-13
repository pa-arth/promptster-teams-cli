//go:build !windows

package selfupdate

import (
	"fmt"
	"os"
	"syscall"
)

// applySwapAndReexec makes staged executable, atomically renames it over self
// (same directory, so rename is atomic on POSIX and a running binary can be
// replaced by inode swap), then re-execs the running process image in place via
// execve. On success it DOES NOT RETURN: the current process becomes the new
// binary running the same argv/env, so the watch daemon keeps its PID, its
// redirected log fds, and its supervisor.json entry, and capture continues
// seamlessly on the new version. The single-instance flock fd is opened
// O_CLOEXEC by the Go runtime, so execve releases it and the new image
// re-acquires it cleanly — no self-deadlock.
//
// It returns an error only for a pre-exec failure (chmod/rename), leaving the
// old binary in place; syscall.Exec only returns on failure to exec.
func applySwapAndReexec(self, staged string) error {
	if err := os.Chmod(staged, 0o755); err != nil {
		return fmt.Errorf("selfupdate: chmod staged binary: %w", err)
	}
	if err := os.Rename(staged, self); err != nil {
		return fmt.Errorf("selfupdate: swap in new binary: %w", err)
	}
	// Re-exec the freshly-swapped binary with the same argv and environment.
	// Never returns on success.
	if err := syscall.Exec(self, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("selfupdate: re-exec %s: %w", self, err)
	}
	return nil
}
