package capture

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// TestRealCodexRolloutCapture is an opt-in developer smoke test for replaying a
// locally-produced Codex rollout through the same redaction -> normalization ->
// projection -> signing -> outbox path used by the watcher. It never prints
// event bodies or transcript text. CI skips it because CI has no Codex home.
//
// Run with:
//
//	PROMPTSTER_TEST_CODEX_ROLLOUT=/path/to/rollout-....jsonl \
//	go test ./internal/capture -run TestRealCodexRolloutCapture -v
func TestRealCodexRolloutCapture(t *testing.T) {
	rollout := os.Getenv("PROMPTSTER_TEST_CODEX_ROLLOUT")
	if rollout == "" {
		t.Skip("set PROMPTSTER_TEST_CODEX_ROLLOUT to replay a real Codex rollout")
	}
	if _, err := os.Stat(rollout); err != nil {
		t.Fatal(err)
	}

	tmp := os.Getenv("PROMPTSTER_TEST_CAPTURE_DIR")
	if tmp == "" {
		tmp = t.TempDir()
	} else {
		if err := os.MkdirAll(tmp, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	pubkey, err := sign.GenerateSessionKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PROMPTSTER_TEST_CAPTURE_DIR") != "" {
		if err := os.WriteFile(filepath.Join(tmp, "device-pubkey"), []byte(pubkey), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	root := codexRolloutCwd(rollout)
	if root == "" {
		t.Fatal("rollout has no session_meta cwd")
	}
	proc := normalize.NewCodexRolloutProcessor(codexSessionIDFromPath(rollout))
	proc.RepoRoot, proc.RepoHost, proc.RepoTracked = sessionRepoIdentity(root)
	progress := codexWatchProgress{Offsets: map[string]int64{}, Match: map[string]string{rollout: "yes"}}
	session := Session{DeviceID: "dev-real-rollout-smoke", TaskRoot: root}

	queued := tailCodexRollout(rollout, progress, proc, session, false)
	if queued == 0 {
		t.Fatal("real rollout produced zero queued events")
	}

	f, err := os.Open(state.OutboxPath())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	kinds := map[string]int{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		var ev event.Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		kinds[ev.Kind]++
		if ev.Source != "codex" {
			t.Errorf("event %s source = %q, want codex", ev.Kind, ev.Source)
		}
		if ev.DeviceID != session.DeviceID {
			t.Errorf("event %s deviceId = %q", ev.Kind, ev.DeviceID)
		}
		if ev.Sig == "" {
			t.Errorf("event %s is unsigned", ev.Kind)
		}
		if ev.RawPayload != "" {
			t.Errorf("event %s retained rawPayload after projection", ev.Kind)
		}
		if data, ok := ev.Data.(map[string]interface{}); ok {
			for _, forbidden := range []string{
				"diff", "content", "stdout", "stderr", "lastAssistantMessage",
				"inputPreview", "argsPreview", "toolName",
			} {
				if _, present := data[forbidden]; present {
					t.Errorf("event %s retained forbidden data key %q", ev.Kind, forbidden)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"session_start", "prompt", "command"} {
		if kinds[required] == 0 {
			t.Errorf("real rollout produced no %s events (kinds=%v)", required, kinds)
		}
	}
	if got := progress.Offsets[rollout]; got <= 0 {
		t.Errorf("rollout offset did not advance: %d", got)
	}
	t.Logf("captured %d projected, signed events from real Codex rollout at %s: %v",
		queued, time.Now().UTC().Format(time.RFC3339), kinds)
}
