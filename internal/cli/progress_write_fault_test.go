package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
)

// §2.3 EXISTS BECAUSE OF WHAT HAPPENED LAST TIME. The reconstruction line
// shipped to `printStatusStatic` only, and review had to catch it: `status`
// opens the TUI unless stdout is not a TTY or `--once` is passed, so a report
// present only on the static path is absent from the surface operators use.
//
// So this fault is asserted on BOTH `status` surfaces and in `doctor`, and the
// mutations below run each direction separately — wiring one and not the other
// must fail, both ways round.

// fakeWriteFaults points the surfaces at a fixed set of faulted watchers.
func fakeWriteFaults(t *testing.T, faulted ...string) {
	t.Helper()
	prev := writeFaultsNow
	writeFaultsNow = func() []string { return faulted }
	t.Cleanup(func() { writeFaultsNow = prev })
}

// A WARNING, not an OK — the contrast with the reconstruction line is the point.
// A replay is deliberate behaviour whose correct response is to wait; an
// unwritable state dir is a fault with a fix, and reporting them at the same
// level makes the actionable one indistinguishable from the one that is fine.
func TestDoctorWarnsAboutAnUnwritableStateDir(t *testing.T) {
	lines := progressWriteFaultLines([]string{"claude"})
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if lines[0].level != queueWarn {
		t.Errorf("level = %v, want queueWarn — this one has a fix, unlike a running replay",
			lines[0].level)
	}
	if !strings.Contains(lines[0].text, "restart") && !strings.Contains(lines[0].text, "RESTART") {
		t.Errorf("must say the replay repeats every restart:\n%s", lines[0].text)
	}
}

func TestAHealthyDeviceGetsNoWriteFaultLine(t *testing.T) {
	if got := progressWriteFaultLines(nil); got != nil {
		t.Errorf("a healthy device reported a fault: %+v", got)
	}
	if got := statusProgressWriteFaultRow(nil); got != nil {
		t.Errorf("a healthy device got a status row: %+v", got)
	}
}

// The dashboard: the surface `status` actually opens.
func TestTheDashboardShowsTheWriteFault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	t.Setenv("PROMPTSTER_TEAMS_TOKEN", fakeDevKey)
	fakeRecon(t, capturedNothingReplaying())
	fakeWriteFaults(t, "claude")

	view := newStatusModel().View()

	for _, want := range []string{"NOT SAVING", "claude", "restart"} {
		if !strings.Contains(view, want) {
			t.Errorf("dashboard missing %q — this is the view `status` opens by default\n---\n%s",
				want, view)
		}
	}
}

// The static snapshot: `status --once`, pipes, CI.
func TestTheStaticStatusShowsTheWriteFault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	t.Setenv("PROMPTSTER_TEAMS_TOKEN", fakeDevKey)
	fakeRecon(t, capturedNothingReplaying())
	fakeWriteFaults(t, "codex")

	out := captureStdout(t, printStatusStatic)

	for _, want := range []string{"progress", "NOT SAVING", "codex"} {
		if !strings.Contains(out, want) {
			t.Errorf("static status missing %q\n---\n%s", want, out)
		}
	}
}

func capturedNothingReplaying() capture.ReconstructionState { return capture.ReconstructionState{} }

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = prev
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
