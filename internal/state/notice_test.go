package state

import (
	"os"
	"testing"
	"time"
)

func TestCapturePauseLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()

	if p, _ := CapturePaused(now); p {
		t.Fatal("paused with no record")
	}
	if err := PauseCapture(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if p, until := CapturePaused(now.Add(1000 * time.Hour)); !p || !until.IsZero() {
		t.Fatalf("indefinite pause: got %v %v", p, until)
	}
	if err := PauseCapture(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if p, _ := CapturePaused(now); !p {
		t.Fatal("not paused before expiry")
	}
	if p, _ := CapturePaused(now.Add(2 * time.Hour)); p {
		t.Fatal("still paused after expiry")
	}
	// A damaged record must not silently resume capture.
	if err := os.WriteFile(capturePausePath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, _ := CapturePaused(now); !p {
		t.Fatal("corrupt pause record read as not paused")
	}
	if err := ResumeCapture(); err != nil {
		t.Fatal(err)
	}
	if p, _ := CapturePaused(now); p {
		t.Fatal("paused after resume")
	}
	if err := ResumeCapture(); err != nil {
		t.Fatalf("resume twice: %v", err)
	}
}

func TestNoticeAcknowledgedIsVersioned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if NoticeAcknowledged() {
		t.Fatal("acknowledged with no record")
	}
	if err := SaveNoticeAck(NoticeAck{Version: "older", AcknowledgedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if NoticeAcknowledged() {
		t.Fatal("an older notice version counted as acknowledged")
	}
	if err := SaveNoticeAck(NoticeAck{Version: NoticeVersion, AcknowledgedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if !NoticeAcknowledged() {
		t.Fatal("current version not acknowledged")
	}
}
