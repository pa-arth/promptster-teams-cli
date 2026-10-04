//go:build linux || darwin

package capture

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// A child left un-Waited (what an in-place re-exec leaves behind) must be reaped.
func TestReapInheritedChildren(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skip("no `true`:", err)
	}
	pid := cmd.Process.Pid
	time.Sleep(100 * time.Millisecond) // let it exit and turn zombie
	reapInheritedChildren()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
	}
	t.Fatalf("child %d still a zombie after reapInheritedChildren", pid)
}
