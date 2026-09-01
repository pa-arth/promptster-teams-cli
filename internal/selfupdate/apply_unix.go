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
	if err := swapInPlace(self, staged); err != nil {
		return err
	}
	// Re-exec the freshly-swapped binary. Never returns on success.
	return reexecInto(self)
}

// swapInPlace is applySwapAndReexec without the re-exec: it puts the verified
// binary on disk and RETURNS.
//
// It exists for the foreground `update` command, which must not re-exec. That
// path replaces a binary the engineer invoked by hand, and reexecInto carries
// os.Args[1:] into the new image — so re-execing there would relaunch
// `promptster-teams update` as the new version, turning a one-shot command into
// a second update run. The daemon is a different process and is restarted
// explicitly by that command instead.
func swapInPlace(self, staged string) error {
	// #nosec G302 -- an executable MUST be 0755; the staged file was sha256 + minisign verified before this call.
	if err := os.Chmod(staged, 0o755); err != nil {
		return fmt.Errorf("selfupdate: chmod staged binary: %w", err)
	}
	if err := os.Rename(staged, self); err != nil {
		return fmt.Errorf("selfupdate: swap in new binary: %w", err)
	}
	return nil
}

// reexecInto replaces the running process image with the binary at target,
// keeping this process's arguments and environment. On success it DOES NOT
// RETURN — same PID, same redirected log fds, same supervisor.json entry, so
// capture continues seamlessly on the new version.
//
// argv[0] is set to target rather than carried over from os.Args, so `ps` names
// the file that is actually executing. That matters most in the case this exists
// for: a daemon catching up out of a path that no longer exists into the managed
// one, where the inherited argv[0] would name the deleted file forever.
// (os.Executable() reads /proc/self/exe and _NSGetExecutablePath, so it is
// unaffected either way — nothing in this repo reads os.Args[0].)
//
// The single-instance flock fd is opened O_CLOEXEC by the Go runtime, so execve
// releases it and the new image re-acquires it cleanly — no self-deadlock.
func reexecInto(target string) error {
	argv := append([]string{target}, os.Args[1:]...)
	// #nosec G204 G702 -- target is os.Executable()-resolved or the managed install path (never user input) and the args are this process's own argv; re-execing ourselves is the whole point, not command injection.
	if err := syscall.Exec(target, argv, os.Environ()); err != nil {
		return fmt.Errorf("selfupdate: re-exec %s: %w", target, err)
	}
	return nil
}
