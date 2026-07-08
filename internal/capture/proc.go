package capture

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// processExists reports whether a process with the given pid is alive.
// Used by the watchers to avoid spawning a duplicate background daemon.
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		// #nosec G204 -- constant argv; pid is an int formatted into a fixed filter expression, not user input. Liveness probe only.
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid)).CombinedOutput()
		if err != nil {
			return false
		}
		text := string(out)
		return strings.Contains(text, fmt.Sprintf(" %d ", pid)) || strings.Contains(text, fmt.Sprintf(",%d", pid))
	}
	// #nosec G204 -- constant argv; pid is an int rendered via strconv.Itoa, not user input. Signal 0 is a liveness probe, sends nothing.
	return exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil
}
