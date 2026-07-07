package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

func TestClassifyClaudeTranscript(t *testing.T) {
	tmp := t.TempDir()
	cutoff := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	write := func(name string, lines ...string) string {
		p := filepath.Join(tmp, name)
		content := ""
		for _, l := range lines {
			content += l + "\n"
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	ws := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	// Production resolves the workspace through symlinks before matching
	// (macOS /var -> /private/var); mirror that here.
	ws = resolvePath(ws)

	match := write("match.jsonl",
		`{"type":"mode","mode":"normal"}`,
		fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"2026-06-10T10:00:00Z","message":{"content":"hi"}}`, ws),
	)
	if got := classifyClaudeTranscript(match, []string{ws}, cutoff); got != claudeMatchYes {
		t.Errorf("match: got %v", got)
	}

	other := write("other.jsonl",
		fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"2026-06-10T10:00:00Z","message":{"content":"hi"}}`, filepath.Join(tmp, "elsewhere")),
	)
	if got := classifyClaudeTranscript(other, []string{ws}, cutoff); got != claudeMatchNo {
		t.Errorf("other: got %v", got)
	}

	// No cwd yet — file just created, must stay undecided (retry next poll),
	// NOT be cached as a mismatch.
	young := write("young.jsonl", `{"type":"mode","mode":"normal"}`)
	if got := classifyClaudeTranscript(young, []string{ws}, cutoff); got != claudeMatchUndecided {
		t.Errorf("young: got %v", got)
	}

	old := write("old.jsonl",
		fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"2026-06-10T08:00:00Z","message":{"content":"hi"}}`, ws),
	)
	if got := classifyClaudeTranscript(old, []string{ws}, cutoff); got != claudeMatchNo {
		t.Errorf("old session before cutoff: got %v", got)
	}
}

func TestSuppressForTranscriptCapture(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	hookPrompt := event.Event{Kind: "prompt", Source: "claude-code"}
	lifecycle := event.Event{Kind: "session_start", Source: "claude-code"}
	cursorPrompt := event.Event{Kind: "prompt", Source: "cursor"}

	managed := Session{CaptureMode: ""}
	if suppressForTranscriptCapture(managed, &hookPrompt) {
		t.Error("managed mode must never suppress")
	}

	transcript := Session{CaptureMode: "transcript"}
	// No watcher state at all → unhealthy → no suppression, but the takeover
	// marker is recorded so a future watcher skips the backlog.
	if suppressForTranscriptCapture(transcript, &hookPrompt) {
		t.Error("unhealthy watcher must not suppress")
	}
	if _, err := os.Stat(claudeHookTakeoverPath()); err != nil {
		t.Errorf("takeover marker not written: %v", err)
	}

	// Healthy watcher (this process, fresh heartbeat) → suppress prompt but
	// keep lifecycle + non-claude sources.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveClaudeWatcherState(claudeWatcherState{PID: os.Getpid(), StartedAt: now, LastHeartbeat: now}); err != nil {
		t.Fatal(err)
	}
	if !suppressForTranscriptCapture(transcript, &hookPrompt) {
		t.Error("healthy watcher must suppress overlapping kinds")
	}
	if suppressForTranscriptCapture(transcript, &lifecycle) {
		t.Error("lifecycle kinds must never be suppressed")
	}
	if suppressForTranscriptCapture(transcript, &cursorPrompt) {
		t.Error("non-claude sources must never be suppressed")
	}

	// Stale heartbeat → unhealthy again.
	stale := time.Now().Add(-2 * claudeWatcherStaleAfter).UTC().Format(time.RFC3339Nano)
	if err := saveClaudeWatcherState(claudeWatcherState{PID: os.Getpid(), StartedAt: now, LastHeartbeat: stale}); err != nil {
		t.Fatal(err)
	}
	if suppressForTranscriptCapture(transcript, &hookPrompt) {
		t.Error("stale heartbeat must not suppress")
	}

	// Degraded watcher (running, parsing nothing) → unhealthy.
	if err := saveClaudeWatcherState(claudeWatcherState{PID: os.Getpid(), StartedAt: now, LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano), Degraded: true}); err != nil {
		t.Fatal(err)
	}
	if suppressForTranscriptCapture(transcript, &hookPrompt) {
		t.Error("degraded watcher must not suppress")
	}
}

func TestClaudeAgentIDFromPath(t *testing.T) {
	if got := claudeAgentIDFromPath("/x/p/sess/subagents/agent-ab276e3606abc0ce2.jsonl"); got != "ab276e3606abc0ce2" {
		t.Errorf("got %q", got)
	}
	if got := claudeAgentIDFromPath("/x/p/sess/subagents/deadbeef.jsonl"); got != "deadbeef" {
		t.Errorf("got %q", got)
	}
}

func TestIsClaudeSidechainFile(t *testing.T) {
	if !isClaudeSidechainFile("/x/projects/p/sess-1/subagents/agent-abc.jsonl") {
		t.Error("agent- prefix in subagents dir must be sidechain")
	}
	if !isClaudeSidechainFile("/x/projects/p/sess-1/subagents/deadbeef.jsonl") {
		t.Error("any file under subagents/ must be sidechain")
	}
	if isClaudeSidechainFile("/x/projects/p/2677c24e.jsonl") {
		t.Error("main transcript must not be sidechain")
	}
}

func TestClaudeDegradationStepCatchesMidSessionFailure(t *testing.T) {
	// Healthy stretch: events flowing, counters stay reset.
	degraded, since := claudeDegradationStep(false, 5, 40_000, 0)
	if degraded || since != 0 {
		t.Fatalf("healthy poll: degraded=%v since=%d", degraded, since)
	}
	// Mid-session parser break: bytes accumulate across event-less polls and
	// trip the threshold EVEN THOUGH events were sent earlier in the session
	// (the old eventsSent==0 condition could never fire here).
	for i := 0; i < 3; i++ {
		degraded, since = claudeDegradationStep(degraded, 0, 100_000, since)
	}
	if !degraded {
		t.Fatalf("300KB without a parsed event must degrade (since=%d)", since)
	}
	// Stays degraded on further empty polls.
	if d, _ := claudeDegradationStep(degraded, 0, 10, since); !d {
		t.Error("degraded must persist while nothing parses")
	}
	// Recovery: one parsed event resets everything.
	degraded, since = claudeDegradationStep(degraded, 1, 500, since)
	if degraded || since != 0 {
		t.Errorf("recovery failed: degraded=%v since=%d", degraded, since)
	}
}
