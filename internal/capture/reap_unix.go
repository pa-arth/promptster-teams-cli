//go:build !windows

package capture

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// reapInheritedChildren waits on children this process did not spawn. A self-update
// re-execs the watcher in place (syscall.Exec), and any git/claude still running
// at that moment stays our child with no exec.Cmd left to Wait on it, so each one
// lingered as a zombie until the daemon restarted. Run it before the watcher
// spawns anything: at that point every child is inherited, so waiting by PID
// cannot race an exec.Cmd's own Wait.
func reapInheritedChildren() {
	// #nosec G204 -- fixed argv; the only argument is our own PID.
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return // exit 1 = no children, the normal case
	}
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil {
			go func() { _, _ = p.Wait() }()
		}
	}
}
