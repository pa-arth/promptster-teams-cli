package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDevKey is a syntactically-valid but entirely fake developer key for tests
// — not a secret. The trailing comment tells gitleaks to skip it (its
// generic-api-key rule otherwise flags any literal next to *_TOKEN).
const fakeDevKey = "PSE-ABCD-2345-6789-JKLM-NPQR-STUV" // gitleaks:allow

// writeWatcherPidfile drops a watcher pidfile pointing at the current process
// (which is guaranteed alive) into the state dir, so Snapshot() reports it live.
func writeWatcherPidfile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestStatusModelViewLive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	t.Setenv("PROMPTSTER_TEAMS_TOKEN", fakeDevKey)

	pid := os.Getpid()
	// A recent heartbeat so the watcher reads as live (Snapshot requires a fresh
	// heartbeat, not just a live PID, to reject stale/reused-PID pidfiles).
	hb := time.Now().UTC().Format(time.RFC3339)
	writeWatcherPidfile(t, dir, "claude-watcher.json",
		`{"pid":`+itoa(pid)+`,"startedAt":"`+hb+`","lastHeartbeat":"`+hb+`","eventsCaptured":299,"bytesConsumed":3581277}`)

	m := newStatusModel()
	if !m.snap.Live {
		t.Fatal("expected snapshot to be Live with a running watcher pidfile")
	}
	if !m.snap.Claude.Running {
		t.Fatal("expected claude watcher to be Running")
	}

	view := m.View()
	for _, want := range []string{"capture", "watchers", "buffer", "healthy", "299 events"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q\n---\n%s", want, view)
		}
	}
}

func TestStatusModelViewIdle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	t.Setenv("PROMPTSTER_TEAMS_TOKEN", fakeDevKey)

	m := newStatusModel()
	if m.snap.Live {
		t.Fatal("expected snapshot to be idle with no pidfiles")
	}
	view := m.View()
	if !strings.Contains(view, "idle") || !strings.Contains(view, "not running") {
		t.Errorf("idle view missing expected markers\n---\n%s", view)
	}
}

func TestStatusModelStaleHeartbeatNotLive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	t.Setenv("PROMPTSTER_TEAMS_TOKEN", fakeDevKey)

	// Live PID (this process) but a heartbeat far in the past — the scenario where
	// a crashed watcher's pidfile lingers and its PID gets reused. Must NOT read
	// as live capture.
	pid := os.Getpid()
	writeWatcherPidfile(t, dir, "claude-watcher.json",
		`{"pid":`+itoa(pid)+`,"startedAt":"2020-01-01T00:00:00Z","lastHeartbeat":"2020-01-01T00:00:00Z","eventsCaptured":1}`)

	m := newStatusModel()
	if m.snap.Live {
		t.Fatal("expected stale-heartbeat watcher to NOT be reported live")
	}
}

func TestHumanizeBytesNoPanic(t *testing.T) {
	// 1024^5 (a petabyte) previously indexed past "KMGT" and panicked.
	for _, n := range []int64{1 << 50, 1 << 60, 1<<63 - 1} {
		if got := humanizeBytes(n); got == "" {
			t.Errorf("humanizeBytes(%d) returned empty", n)
		}
	}
}

func TestHumanizeDuration(t *testing.T) {
	cases := map[string]string{
		"45s":   humanizeDuration(45e9),
		"2m":    humanizeDuration(120e9),
		"1h38m": humanizeDuration((1*3600 + 38*60) * 1e9),
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("humanizeDuration: got %q want %q", got, want)
		}
	}
}

func TestHumanizeBytes(t *testing.T) {
	if got := humanizeBytes(56); got != "56 B" {
		t.Errorf("got %q want 56 B", got)
	}
	if got := humanizeBytes(3581277); got != "3.4 MB" {
		t.Errorf("got %q want 3.4 MB", got)
	}
}

// itoa avoids importing strconv just for the test fixture.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
