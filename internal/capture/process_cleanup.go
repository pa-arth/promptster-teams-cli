package capture

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// pidLooksLikeOurs reports whether pid is (still) a promptster capture process,
// guarding against a stale pidfile whose PID the OS has since reused for an
// unrelated process — signaling that would kill a bystander. Best-effort: on
// Windows (no cheap cmdline read) it returns true and callers fall back to the
// plain liveness check. A ps error is treated as "not ours" (the pid is gone).
// Matches the substring "promptster-teams" so it covers the dev binary, the
// per-platform npm binary (…-darwin-arm64), and the in-process watch subcommands,
// while never matching a bare `promptster` (promptster-cli) process.
func pidLooksLikeOurs(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	// #nosec G204 -- constant argv; pid rendered via strconv.Itoa, not user input. Reads only the process command line.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "promptster-teams")
}

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
	// #nosec G204 -- constant argv; pattern is one of three hardcoded daemon command strings (see killStalePromptsterDaemons callers), not user input.
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
