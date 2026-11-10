package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

func TestTakeFlag(t *testing.T) {
	args, ok := takeFlag([]string{"--key", "K", "--accept-notice"}, "--accept-notice")
	if !ok || strings.Join(args, " ") != "--key K" {
		t.Fatalf("got %v %v", args, ok)
	}
	if _, ok := takeFlag([]string{"-accept-notice"}, "--accept-notice"); !ok {
		t.Fatal("single-dash form not recognised")
	}
}

func TestCaptureVisibilityLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	if got := captureVisibilityLine(now); got != "capturing — your org admins can view these sessions" {
		t.Fatalf("got %q", got)
	}
	_ = state.PauseCapture(now.Add(time.Hour))
	if got := captureVisibilityLine(now); !strings.HasPrefix(got, "paused until ") {
		t.Fatalf("got %q", got)
	}
}

func TestNonInteractiveNoticeNeedsExplicitAccept(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// `go test` stdin is not a terminal.
	if ensureNoticeAcknowledged(false) {
		t.Fatal("acknowledged without a terminal or --accept-notice")
	}
	if state.NoticeAcknowledged() {
		t.Fatal("recorded an acknowledgement nobody gave")
	}
	if !ensureNoticeAcknowledged(true) || !state.NoticeAcknowledged() {
		t.Fatal("--accept-notice did not record the acknowledgement")
	}
}
