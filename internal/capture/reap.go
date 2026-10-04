//go:build linux || darwin

package capture

import (
	"fmt"
	"os"
)

// reapInheritedChildren waits on children this process did not spawn. A self-update
// re-execs the watcher in place (syscall.Exec), and any git/claude still running
// at that moment stays our child with no exec.Cmd left to Wait on it, so each one
// lingered as a zombie until the daemon restarted. Run it before the watcher
// spawns anything: at that point every child is inherited, so waiting by PID
// cannot race an exec.Cmd's own Wait.
func reapInheritedChildren() {
	pids, err := childPIDs(os.Getpid())
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptster-teams: could not list inherited child processes (%v); any left by a self-update stay zombies until restart\n", err)
		return
	}
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			go func() { _, _ = p.Wait() }()
		}
	}
}
