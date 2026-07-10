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
// unrelated process — signaling that would kill a bystander. It verifies the
// process identity on every platform (cmdline via `ps` on unix, image name via
// `tasklist` on Windows) and treats an unreadable/errored lookup as "not ours"
// so a reused PID is never signaled when ownership can't be confirmed. Matches
// "promptster" so it covers the dev binary, the per-platform npm binary
// (…-darwin-arm64 / …-windows-x64.exe), and the in-process watch subcommands.
func pidLooksLikeOurs(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		// #nosec G204 -- constant argv; pid formatted into a fixed filter expression, not user input. Reads only the task list.
		out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid)).CombinedOutput()
		if err != nil {
			return false
		}
		return strings.Contains(strings.ToLower(string(out)), "promptster")
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
