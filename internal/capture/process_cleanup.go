package capture

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// signalAndWaitForExit sends SIGINT to pid, waits up to 2s for it to exit,
// then SIGKILLs if still alive. No-op if pid is invalid or already dead.
func signalAndWaitForExit(pid int) {
	if pid <= 0 || pid == os.Getpid() || !processExists(pid) {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(os.Interrupt)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = proc.Kill()
}

// killStalePromptsterDaemons finds and terminates any promptster daemon
// processes whose command line matches pattern (full-cmdline substring).
// Used as a belt-and-suspenders sweep for orphans whose state file was lost
// — e.g. overwritten by a later `promptster start` before the prior `done`
// ran, leaving the old PID untracked.
//
// Excludes the current process. Returns number of processes signalled.
// No-op on Windows (pgrep unavailable); state-file-based kill is the only
// cleanup path there.
func killStalePromptsterDaemons(pattern string) int {
	pids := findPromptsterDaemonPIDs(pattern)
	self := os.Getpid()
	count := 0
	for _, pid := range pids {
		if pid == self {
			continue
		}
		signalAndWaitForExit(pid)
		count++
	}
	return count
}

func findPromptsterDaemonPIDs(pattern string) []int {
	if runtime.GOOS == "windows" {
		return nil
	}
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if pid, err := strconv.Atoi(line); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
