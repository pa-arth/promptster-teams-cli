//go:build !windows

package capture

import "syscall"

// detachSysProcAttr fully detaches a spawned child from the launching shell.
// Setsid puts it in a new session with no controlling terminal, so closing the
// terminal (which delivers SIGHUP to the foreground session) doesn't kill the
// background daemon.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
