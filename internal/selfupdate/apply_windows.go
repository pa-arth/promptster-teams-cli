//go:build windows

package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// applySwapAndReexec performs the Windows equivalent of the unix in-place
// swap+reexec. Windows cannot rename over or delete a running .exe, so it does
// the move-old-aside dance: rename the running binary to "<self>.old", move the
// staged binary into <self>, then spawn a fresh detached process from the new
// binary and exit so the old image releases its file handle (the ".old" file is
// cleaned up on the next successful update, when it is no longer running).
//
// Best-effort by contract: on any pre-spawn failure it tries to roll the ".old"
// rename back and returns an error, leaving the daemon running the old version.
// On success it does not return — it calls os.Exit(0) after spawning the child
// so only the new image keeps capturing.
func applySwapAndReexec(self, staged string) error {
	if err := swapInPlace(self, staged); err != nil {
		return err
	}
	return reexecInto(self)
}

// swapInPlace performs the move-old-aside swap and RETURNS, for the foreground
// `update` command. See the unix twin for why that path must not re-exec.
//
// The ".old" file is left behind deliberately: on Windows it is still the image
// of any process currently running from it, so it cannot be deleted yet. The
// next successful swap clears it.
func swapInPlace(self, staged string) error {
	old := self + ".old"
	_ = os.Remove(old) // clear any leftover from a prior update
	if err := os.Rename(self, old); err != nil {
		return fmt.Errorf("selfupdate: move running binary aside: %w", err)
	}
	if err := os.Rename(staged, self); err != nil {
		// Roll back so the daemon still has a binary to be re-launched from.
		_ = os.Rename(old, self)
		return fmt.Errorf("selfupdate: move staged binary into place: %w", err)
	}
	return nil
}

// reexecInto is the Windows stand-in for execve: spawn a detached process from
// the binary at target and exit, so this image releases its file handles and
// only the new one keeps capturing. On success it does not return.
//
// It marks the child with EnvHandoff. Windows has no execve, so unlike unix
// there are momentarily TWO processes, and the new one races the old one for the
// single-instance lock it is inheriting. Losing that race is not a harmless
// retry — the child prints "capture already running" and exits, the parent is
// already on its way out, and the machine captures nothing until the next login.
// The marker tells the child this is a handoff and it should WAIT for the lock
// rather than bow out (see capture.RunTeamsWatch).
func reexecInto(target string) error {
	// #nosec G204 -- target is our own resolved install path or the managed one; argv is this process's own os.Args.
	cmd := exec.Command(target, os.Args[1:]...)
	cmd.Env = append(os.Environ(), EnvHandoff+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("selfupdate: relaunch new binary: %w", err)
	}
	_ = cmd.Process.Release()
	os.Exit(0)
	return nil
}
