package main

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
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid)).CombinedOutput()
		if err != nil {
			return false
		}
		text := string(out)
		return strings.Contains(text, fmt.Sprintf(" %d ", pid)) || strings.Contains(text, fmt.Sprintf(",%d", pid))
	}
	return exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil
}
