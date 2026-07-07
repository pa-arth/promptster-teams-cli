//go:build windows

package capture

import "syscall"

// detachSysProcAttr detaches a spawned child on Windows by giving it a new
// process group, so a Ctrl-C / Ctrl-Break sent to the launching console isn't
// propagated to the background daemon.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
