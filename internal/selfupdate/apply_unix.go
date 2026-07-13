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
//
// On the rename-before-exec ordering: the staged file was sha256- AND
// minisign-verified before we got here, so the swapped-in binary is always a
// GOOD build for this GOOS/GOARCH. If syscall.Exec somehow fails after the
// rename, the daemon exits but the on-disk binary is the valid new version, so
// the autostart supervisor relaunches straight into it — the update still takes,
// just on the next start instead of in place.
func applySwapAndReexec(self, staged string) error {
	// #nosec G302 -- an executable MUST be 0755; the staged file was sha256 + minisign verified before this call.
	if err := os.Chmod(staged, 0o755); err != nil {
		return fmt.Errorf("selfupdate: chmod staged binary: %w", err)
	}
	if err := os.Rename(staged, self); err != nil {
		return fmt.Errorf("selfupdate: swap in new binary: %w", err)
	}
	// Re-exec the freshly-swapped binary with the same argv and environment.
	// Never returns on success.
	// #nosec G204 G702 -- `self` is os.Executable()-resolved (not user input) and os.Args is our own process argv; re-execing ourselves with our own args is the whole point of an in-place self-update, not command injection.
	if err := syscall.Exec(self, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("selfupdate: re-exec %s: %w", self, err)
	}
	return nil
}
