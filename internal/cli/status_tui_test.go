package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
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

// TestStatusModelShowsWidenedScope: the dashboard is the DEFAULT `status` view
// (the static one needs --once or a non-TTY), so it is where an engineer looks
// to answer "is my second checkout captured?". Registering a directory the
// daemon's own watch dir does not contain must show up here, not only in the
// static view.
//
// Asserts on the model field and the rendered capture PANEL, never on the word
// "watch" in the whole view — the unrelated "watchers" panel contains that, so
// a view-wide substring check stays green with the row deleted.
func TestStatusModelShowsWidenedScope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	t.Setenv("PROMPTSTER_TEAMS_TOKEN", fakeDevKey)

	primary := t.TempDir()
	second := t.TempDir()
	t.Setenv("PROMPTSTER_TEAMS_WATCH_DIR", primary)

	pid := os.Getpid()
	hb := time.Now().UTC().Format(time.RFC3339)
	writeWatcherPidfile(t, dir, "claude-watcher.json",
		`{"pid":`+itoa(pid)+`,"startedAt":"`+hb+`","lastHeartbeat":"`+hb+`","watchDir":`+quote(primary)+`,"eventsCaptured":1}`)

	if _, _, err := capture.RegisterCaptureRoot(second); err != nil {
		t.Fatal(err)
	}

	m := newStatusModel()
	if !strings.Contains(m.watch, second) {
		t.Errorf("model scope must name the registered second tree; got %q", m.watch)
	}
	if !strings.Contains(m.watch, primary) {
		t.Errorf("model scope must still name the daemon's own root; got %q", m.watch)
	}
	// The panel renders from m.watch; assert a distinctive fragment of the scope
	// survives into it, so deleting the row fails here.
	panel := m.capturePanel()
	frag := filepath.Base(second)
	if !strings.Contains(panel, frag) {
		t.Errorf("capture panel must render the widened scope (looking for %q)\n---\n%s", frag, panel)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
