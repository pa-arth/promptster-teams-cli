package capture

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
)

// TestStartupBeatReportsTheDrainsFirstOutcome: the first beat of a watch process
// must not describe a queue nobody has tried yet.
//
// RunTeamsWatch starts the heartbeat BEFORE the watchers, and the watchers start
// the drain. So the startup beat used to be built while the drain did not exist:
// every (re)start reported deliveryState "unknown" beside whatever the queue held
// on disk — events hooks queued while the daemon was down, hours old. The backend
// monitor reads "unknown" as "no health data" and pages on the queue's age, and
// the beat stood for five minutes while the drain emptied that queue in a second.
func TestStartupBeatReportsTheDrainsFirstOutcome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "claude"))
	t.Setenv("PROMPTSTER_CURSOR_HOME", filepath.Join(tmp, "cursor"))

	beats := make(chan map[string]interface{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"kind":"presence"`) {
			var ev struct {
				Data map[string]interface{} `json:"data"`
			}
			_ = json.Unmarshal(body, &ev)
			beats <- ev.Data
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	old := event.NewEvent("user_prompt", "sess-startup")
	old.Ts = time.Now().Add(-76 * time.Minute).UTC().Format(time.RFC3339Nano)
	if err := outbox.Append(old); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	session := Session{DeviceID: "dev-startup", SessionToken: "PSE-TEST", TaskRoot: tmp}
	stop := StartPresenceHeartbeat(session)
	defer stop()

	// Production order: the watchers (and so the drain) come up after the beat.
	ctx, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() {
		go func() { defer close(drained); outbox.Drain(ctx, srv.Client(), "PSE-TEST", nil) }()
	})
	defer func() { cancel(); <-drained }()

	select {
	case beat := <-beats:
		if beat["deliveryState"] != "ok" || beat["pendingEvents"] != float64(0) {
			t.Fatalf("startup beat = deliveryState %v, pendingEvents %v, oldest %v; want ok with the drained queue (0)",
				beat["deliveryState"], beat["pendingEvents"], beat["pendingOldestEventAt"])
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no startup beat")
	}
}

// TestStartupBeatNotSentAfterStop: stopping during the startup wait must end the
// goroutine without appending or POSTing a beat.
func TestStartupBeatNotSentAfterStop(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "claude"))
	t.Setenv("PROMPTSTER_CURSOR_HOME", filepath.Join(tmp, "cursor"))
	prev := presenceStartupWait
	presenceStartupWait = 500 * time.Millisecond
	t.Cleanup(func() { presenceStartupWait = prev })

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)
	if err := outbox.Append(event.NewEvent("user_prompt", "sess-stop")); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		runPresenceHeartbeat(Session{DeviceID: "dev-stop", SessionToken: "PSE-TEST", TaskRoot: tmp}, done)
	}()
	close(done)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat goroutine still running 2s after stop")
	}
	if n := posts.Load(); n != 0 {
		t.Fatalf("stopped heartbeat still POSTed %d beat(s)", n)
	}
}
