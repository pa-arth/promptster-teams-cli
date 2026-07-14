package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// ingestClientTimeout mirrors the 5s http.Client timeout the watchers ingest
// with. Kept as a local literal on purpose: if someone raises the real one, this
// test should fail and force a matching look at shutdownGrace.
const ingestClientTimeout = 5 * time.Second

// TestShutdownGraceExceedsIngestTimeout guards the regression that made `stop`
// unreliable: the watchers only check for a signal between poll iterations, and
// a poll ships events with a 5s-timeout HTTP client. A grace window shorter than
// one hung send (it was 2s) meant any busy watcher got SIGKILLed rather than
// exiting cleanly — which the OS supervisor then read as a crash and restarted.
func TestShutdownGraceExceedsIngestTimeout(t *testing.T) {
	if shutdownGrace <= ingestClientTimeout {
		t.Fatalf("shutdownGrace=%s must exceed the %s ingest client timeout, or a single "+
			"hung send guarantees SIGKILL instead of a clean shutdown", shutdownGrace, ingestClientTimeout)
	}
}

// startReapedFixture launches a long-lived `sleep` under sh, applies setup (e.g.
// a signal trap) first, and returns its pid once setup is provably in effect.
//
// Two subtleties, both of which produced wrong results before being handled:
//
// The ready marker closes a startup race. cmd.Start() returns as soon as sh is
// forked, long before it has run setup — so a signal sent immediately hits the
// default disposition rather than the trap, and the escalation test passed
// vacuously against a process that died on the first SIGINT. Touching the marker
// after setup and before exec makes the handoff deterministic (an ignored signal
// survives exec, so the trap still holds once sleep replaces the shell).
//
// The background reaper matters because an exited child of this test process
// stays a zombie until waited on, and processExists ("kill -0") reports a zombie
// as alive — which would make signalAndWaitForExit look like it hung for the
// full grace window against a process that had already died. Production never
// hits this: `stop` signals a detached daemon it did not spawn, so init reaps it.
func startReapedFixture(t *testing.T, setup string) int {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("sh", "-c", setup+"; touch '"+ready+"'; exec sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture: %v", err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture (setup=%q) never signalled readiness", setup)
	return 0
}

// TestSignalAndWaitForExitReturnsOnCleanExit checks the common path: a process
// that honors SIGINT is reaped promptly, nowhere near the grace deadline.
func TestSignalAndWaitForExitReturnsOnCleanExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell to build the signal-handling fixture")
	}
	pid := startReapedFixture(t, ":")

	start := time.Now()
	signalAndWaitForExit(pid)
	elapsed := time.Since(start)

	if elapsed >= shutdownGrace {
		t.Errorf("a SIGINT-honoring process took %s to exit; expected well under the %s grace "+
			"window (it should never reach the SIGKILL escalation)", elapsed, shutdownGrace)
	}
}

// TestSignalAndWaitForExitEscalatesToKill checks the escalation still fires: a
// process that ignores SIGINT must not survive `stop`. Widening the grace window
// must not turn the SIGKILL backstop into a hang.
func TestSignalAndWaitForExitEscalatesToKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell to build the signal-handling fixture")
	}
	if testing.Short() {
		t.Skip("waits out the full shutdownGrace window")
	}
	// Trap and ignore SIGINT, so only SIGKILL can end it.
	pid := startReapedFixture(t, `trap "" INT`)

	signalAndWaitForExit(pid)

	// signalAndWaitForExit returns right after sending SIGKILL; give the kernel a
	// beat to tear the process down and the reaper to collect it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && processExists(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if processExists(pid) {
		t.Errorf("pid %d ignored SIGINT and survived signalAndWaitForExit; the SIGKILL "+
			"escalation must still end an unresponsive watcher", pid)
	}
}

// TestPidLooksLikeOursRejectsForeignProcess guards the PID-reuse check that
// stops `stop` from signaling a bystander that inherited a recycled PID.
func TestPidLooksLikeOursRejectsForeignProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell to build the fixture")
	}
	pid := startReapedFixture(t, ":")

	if pidLooksLikeOurs(pid) {
		t.Error("a plain `sleep` was identified as a promptster capture process; `stop` would signal bystanders")
	}
	if pidLooksLikeOurs(0) || pidLooksLikeOurs(-1) {
		t.Error("invalid pids must never look like ours")
	}
}
