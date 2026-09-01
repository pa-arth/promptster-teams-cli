package selfupdate

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestManualGUIPrompt drives the REAL dialog against the real desktop. It is
// opt-in (PROMPTSTER_GUI_MANUAL_TEST=1) because it puts a window on someone's
// screen, which no CI run and no `go test ./...` should ever do.
//
// It exists because the claim underneath ConsentAsk — "a detached daemon with no
// terminal can still reach a human" — is a claim about the operating system, not
// about this code, and the automated tests all stub the dialog away. The only
// honest way to know is to run it from a process with no controlling terminal
// and watch what happens.
func TestManualGUIPrompt(t *testing.T) {
	if os.Getenv("PROMPTSTER_GUI_MANUAL_TEST") != "1" {
		t.Skip("opt-in: set PROMPTSTER_GUI_MANUAL_TEST=1 to show a real dialog")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	res := promptDarwin(ctx, "0.24.0", "0.25.0",
		"https://github.com/pa-arth/promptster-teams-cli/releases/tag/v0.25.0")
	t.Logf("promptDarwin result = %v (0=unavailable 1=declined 2=accepted)", res)
	if res == guiUnavailable {
		t.Error("no dialog could be shown from this process")
	}
}
