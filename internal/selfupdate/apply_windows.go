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

	// #nosec G204 -- self is our own resolved install path; argv is this process's own os.Args.
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("selfupdate: relaunch new binary: %w", err)
	}
	_ = cmd.Process.Release()
	os.Exit(0)
	return nil
}
