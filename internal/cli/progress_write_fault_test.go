package cli

import (
	"errors"
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

// fakeWriteFault points the surfaces at a fixed persistence verdict.
func fakeWriteFault(t *testing.T, fault error) {
	t.Helper()
	prev := persistFaultNow
	persistFaultNow = func() error { return fault }
	t.Cleanup(func() { persistFaultNow = prev })
}

var errSealed = errors.New("permission denied")

// A WARNING, not an OK — the contrast with the reconstruction line is the point.
// A replay is deliberate behaviour whose correct response is to wait; an
// unwritable state dir is a fault with a fix, and reporting them at the same
// level makes the actionable one indistinguishable from the one that is fine.
func TestDoctorWarnsAboutAnUnwritableStateDir(t *testing.T) {
	lines := progressWriteFaultLines(errSealed)
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
	fakeWriteFault(t, errSealed)

	view := newStatusModel().View()

	for _, want := range []string{"NOT SAVING", "restart"} {
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
	fakeWriteFault(t, errSealed)

	out := captureStdout(t, printStatusStatic)

	for _, want := range []string{"progress", "NOT SAVING"} {
		if !strings.Contains(out, want) {
			t.Errorf("static status missing %q\n---\n%s", want, out)
		}
	}
}

// THE P1 REGRESSION, from the CLI side and with NOTHING STUBBED.
//
// Every other test here stubs `persistFaultNow`, which proves the rendering but
// not that the surface can learn the fault in the first place. The first version
// of this change read a map populated by the watcher — a different process — so
// it rendered perfectly in tests and was permanently silent in production.
//
// This one seals a real directory and runs the real probe.
func TestTheStatusSurfaceLearnsTheFaultOnItsOwn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	t.Setenv("PROMPTSTER_TEAMS_TOKEN", fakeDevKey)
	fakeRecon(t, capturedNothingReplaying())
	// persistFaultNow deliberately NOT stubbed.

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	out := captureStdout(t, printStatusStatic)

	if !strings.Contains(out, "NOT SAVING") {
		t.Errorf("a real read-only state dir went unreported by the real surface — this is the "+
			"cross-process hole, and stubbing the probe hides it\n---\n%s", out)
	}
}

// A STORAGE FAULT MUST NOT SEND THE OPERATOR TO `login`, which is the second
// thing review caught.
//
// `ok` selects doctor's closing line, and false prints "Run promptster-teams
// login to get set up." Clearing it on a write fault contradicts the
// permissions-and-free-space diagnosis printed one line earlier and points at a
// command that cannot fix a full disk. The queue-health block directly above
// already declines to touch `ok` for the same reason; this now matches it.
func TestAStorageFaultDoesNotTellTheOperatorToLogIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	t.Setenv("PROMPTSTER_TEAMS_TOKEN", fakeDevKey)
	// Refused instantly, so doctor's reachability probe cannot stall the test.
	t.Setenv("PROMPTSTER_TEAMS_API_URL", "http://127.0.0.1:1")
	fakeRecon(t, capturedNothingReplaying())
	fakeWriteFault(t, errSealed)

	out := captureStdout(t, cmdTeamsDoctor)

	if !strings.Contains(out, "cannot save capture progress") {
		t.Fatalf("the fault itself went unreported, so this test proves nothing\n---\n%s", out)
	}
	if strings.Contains(out, "to get set up") {
		t.Errorf("a full disk was diagnosed and then the operator was told to run `login`, "+
			"which cannot fix it and contradicts the line above\n---\n%s", out)
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
