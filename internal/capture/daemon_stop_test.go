package capture

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/service"
)

// fakeManager stands in for launchd/systemd. Stop() runs onStop, letting a test
// reproduce the thing that matters: the real supervisor's Stop kills the watcher
// as a side effect, so anything StopTeamsDaemon wants to know about the watcher
// must be sampled before that call.
type fakeManager struct{ onStop func() error }

func (f fakeManager) Enable() error  { return nil }
func (f fakeManager) Disable() error { return nil }
func (f fakeManager) Stop() error {
	if f.onStop != nil {
		return f.onStop()
	}
	return nil
}
func (f fakeManager) Status() (bool, string, error) { return true, "fake", nil }

// fakeWatcherPID starts a process pidLooksLikeOurs accepts. Identity is matched
// on the command line, so the trick is a script whose *path* contains
// "promptster-teams": ps then reports "/bin/sh /…/promptster-teams-fixture".
//
// A copy of /bin/sleep would be the obvious fixture and does not work — macOS
// refuses to exec a copied platform binary (the code signature doesn't survive),
// so the process dies instantly, ps finds nothing, and the test skips itself into
// uselessness. A script has no signature to lose. It blocks on `read` against a
// stdin pipe held open here, so it stays alive with no child process to orphan
// (`exec sleep` would replace the shell and lose the matching command line).
func fakeWatcherPID(t *testing.T) int {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "promptster-teams-fixture")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nread line\n"), 0o700); err != nil { // #nosec G302 -- must be executable
		t.Fatalf("write fixture: %v", err)
	}
	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture: %v", err)
	}
	reaped := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !pidLooksLikeOurs(cmd.Process.Pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if !pidLooksLikeOurs(cmd.Process.Pid) {
		t.Fatalf("fixture at %s is not recognised by pidLooksLikeOurs; the test cannot guard anything", bin)
	}
	return cmd.Process.Pid
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	_ = w.Close()
	os.Stderr = old
	return <-done
}

// TestStopReportsStoppedWhenServiceStopKillsWatcher is the regression guard for
// a `stop` that lied. The autostart service's Stop unloads the job, which kills
// the watcher it owns — so if StopTeamsDaemon checked liveness AFTER that call,
// it found nothing alive, reported "no tracked background capture was running",
// and told the user to go hunt with pgrep for a process it had just killed
// itself. Liveness must be sampled first.
//
// Caught by driving the real binary against a live launchd-managed watcher; no
// unit test existed that could have.
func TestStopReportsStoppedWhenServiceStopKillsWatcher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture relies on POSIX process semantics")
	}
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	pid := fakeWatcherPID(t)

	// The watcher's pidfile, as a launchd/systemd-started `watch` writes it (no
	// supervisor.json — that's only written by `start`).
	writeWatcherPidfile(t, pid)

	// Stop() kills the watcher, exactly as `launchctl bootout` does.
	killed := false
	restore := swapServiceManager(fakeManager{onStop: func() error {
		killed = true
		p, _ := os.FindProcess(pid)
		_ = p.Kill()
		for i := 0; i < 50 && processExists(pid); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}})
	defer restore()

	out := captureStderr(t, func() {
		if err := StopTeamsDaemon(); err != nil {
			t.Errorf("StopTeamsDaemon() = %v, want nil", err)
		}
	})

	if !killed {
		t.Fatal("StopTeamsDaemon never stopped the autostart service; the supervisor would restart capture")
	}
	if strings.Contains(out, "no tracked background capture was running") {
		t.Errorf("`stop` killed a live watcher via the service, then reported nothing was running.\n"+
			"Liveness must be sampled before service.Stop(). Got:\n%s", out)
	}
	if !strings.Contains(out, "background capture stopped") {
		t.Errorf("want a success report after stopping a live watcher, got:\n%s", out)
	}
}

// TestStopReportsNothingRunningWhenNothingRuns keeps the honest negative: with
// no watcher alive, `stop` must still say so rather than claim a phantom stop.
func TestStopReportsNothingRunningWhenNothingRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture relies on POSIX process semantics")
	}
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	restore := swapServiceManager(fakeManager{})
	defer restore()

	out := captureStderr(t, func() {
		if err := StopTeamsDaemon(); err != nil {
			t.Errorf("StopTeamsDaemon() = %v, want nil", err)
		}
	})
	if !strings.Contains(out, "no tracked background capture was running") {
		t.Errorf("want the nothing-running report, got:\n%s", out)
	}
}

// TestStopIgnoresStalePidfile guards the PID-reuse path: a pidfile naming a live
// but unrelated process must not be reported as a stop (nor signaled).
func TestStopIgnoresStalePidfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture relies on POSIX process semantics")
	}
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	bystander := startReapedFixture(t, ":") // a plain sleep — not ours
	writeWatcherPidfile(t, bystander)
	restore := swapServiceManager(fakeManager{})
	defer restore()

	out := captureStderr(t, func() { _ = StopTeamsDaemon() })

	if !processExists(bystander) {
		t.Error("`stop` signaled a process that wasn't ours — a recycled PID would kill a bystander")
	}
	if strings.Contains(out, "background capture stopped") {
		t.Errorf("a stale pidfile pointing at a foreign process must not report a stop, got:\n%s", out)
	}
}

func writeWatcherPidfile(t *testing.T, pid int) {
	t.Helper()
	if err := saveClaudeWatcherState(claudeWatcherState{
		PID: pid, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("write watcher pidfile: %v", err)
	}
}

func swapServiceManager(m service.Manager) func() {
	prev := newServiceManager
	newServiceManager = func() service.Manager { return m }
	return func() { newServiceManager = prev }
}
