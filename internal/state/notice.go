package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// NoticeVersion names the employee notice text the engineer acknowledged. Bump
// it whenever the notice changes materially (what is captured, who can see it,
// retention): every engineer is then asked to acknowledge the new text once.
const NoticeVersion = "2026-11-10"

// NoticeAck is the on-disk record of the engineer acknowledging the employee
// notice. It lives beside auto-update-consent.json for the same reason: a
// one-time human decision that must outlive every cache eviction.
type NoticeAck struct {
	Version        string    `json:"version"`
	AcknowledgedAt time.Time `json:"acknowledgedAt"`
	// ProseNotified records that the engineer was told their org turned on
	// assistant-response capture, so that notice is printed once per change.
	ProseNotified bool `json:"proseNotified,omitempty"`
}

func noticeAckPath() string {
	return filepath.Join(GlobalPromptsterDir(), "employee-notice.json")
}

// LoadNoticeAck reads the record; a missing or corrupt file is the zero value
// (never acknowledged), which is the safe reading.
func LoadNoticeAck() NoticeAck {
	var a NoticeAck
	if data, err := os.ReadFile(noticeAckPath()); err == nil {
		_ = json.Unmarshal(data, &a)
	}
	return a
}

// SaveNoticeAck persists the record.
func SaveNoticeAck(a NoticeAck) error {
	return writeJSON(noticeAckPath(), a)
}

// NoticeAcknowledged reports whether the CURRENT notice version was acknowledged.
func NoticeAcknowledged() bool {
	return LoadNoticeAck().Version == NoticeVersion
}

// CapturePause is the engineer's pause (`promptster-teams pause`). A zero
// Until means "until resume".
type CapturePause struct {
	Until    time.Time `json:"until"`
	PausedAt time.Time `json:"pausedAt"`
}

func capturePausePath() string {
	return filepath.Join(GlobalPromptsterDir(), "capture-pause.json")
}

// PauseCapture persists a pause; until zero means indefinitely.
func PauseCapture(until time.Time) error {
	return writeJSON(capturePausePath(), CapturePause{Until: until, PausedAt: time.Now().UTC()})
}

// ResumeCapture lifts any pause. Removing an absent file is not an error.
func ResumeCapture() error {
	if err := os.Remove(capturePausePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CapturePaused reports whether capture is paused at now, and until when (zero
// = until resume). An expired pause reads as not paused. A file that exists but
// cannot be parsed reads as paused indefinitely: the engineer asked for a
// pause, and a damaged record must not silently resume capture.
func CapturePaused(now time.Time) (bool, time.Time) {
	data, err := os.ReadFile(capturePausePath())
	if err != nil {
		return false, time.Time{}
	}
	var p CapturePause
	if json.Unmarshal(data, &p) != nil {
		return true, time.Time{}
	}
	if !p.Until.IsZero() && !now.Before(p.Until) {
		return false, time.Time{}
	}
	return true, p.Until
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
