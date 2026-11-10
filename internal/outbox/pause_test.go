package outbox

import (
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

func TestAppendDropsWhilePaused(t *testing.T) {
	newOutboxTest(t)
	t.Setenv("HOME", t.TempDir())
	if err := state.PauseCapture(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := Append(event.NewEvent("prompt", "sess")); err != nil {
		t.Fatalf("paused Append must not error (tails would rewind and resend): %v", err)
	}
	if n := PendingIn(LaneLive()); n != 0 {
		t.Fatalf("queued %d event(s) while paused", n)
	}
	if err := state.ResumeCapture(); err != nil {
		t.Fatal(err)
	}
	if err := Append(event.NewEvent("prompt", "sess")); err != nil {
		t.Fatal(err)
	}
	if n := PendingIn(LaneLive()); n != 1 {
		t.Fatalf("after resume: %d pending, want 1", n)
	}
}
