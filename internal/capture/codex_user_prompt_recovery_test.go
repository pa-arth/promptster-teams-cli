package capture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// End-to-end proof through the REAL watcher, not just the normalizer: a rollout
// that carries the human turn only on the `response_item` channel — the shape
// that made a live org capture zero prompts across 29 sessions — must produce a
// prompt event in the outbox.
//
// The normalizer unit tests cover the decision; this covers the WIRING (the
// per-poll flush hook and the shared emit path), which is the half a normalizer
// test cannot reach.

func codexOutboxPromptTexts(t *testing.T, outbox string) []string {
	t.Helper()
	f, err := os.Open(outbox)
	if err != nil {
		return nil
	}
	defer f.Close()
	var texts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var ev struct {
			Kind string `json:"kind"`
			Data struct {
				Text string `json:"text"`
			} `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Kind == "prompt" {
			texts = append(texts, ev.Data.Text)
		}
	}
	return texts
}

// codexRolloutWithoutEventMsg writes a rollout whose human turn appears ONLY as
// a response_item — no event_msg/user_message anywhere — preceded by the
// synthetic <environment_context> item Codex always injects on that channel.
func codexRolloutWithoutEventMsg(t *testing.T, root, workspace, human string, trailing bool) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "07", "30")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-07-30T16-38-12-019fb3e4-03e4-77f1-a022-c908046e4da4.jsonl")
	ts := time.Now().UTC().Format(time.RFC3339)

	body := codexSessionMetaLine(resolvePath(workspace), ts) +
		fmt.Sprintf(`{"timestamp":"%s","type":"response_item","payload":{"type":"message","role":"user",`+
			`"content":[{"type":"input_text","text":"<environment_context>\n  <cwd>%s</cwd>\n</environment_context>"}]}}`+"\n",
			ts, resolvePath(workspace)) +
		fmt.Sprintf(`{"timestamp":"%s","type":"response_item","payload":{"type":"message","role":"user",`+
			`"content":[{"type":"input_text","text":%q}]}}`+"\n", ts, human)
	if trailing {
		// The agent answers, which is what flushes the buffered turn on the very
		// next line — the common case.
		body += fmt.Sprintf(`{"timestamp":"%s","type":"response_item","payload":{"type":"message","role":"assistant",`+
			`"content":[{"type":"output_text","text":"on it"}]}}`+"\n", ts)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexRecoveryHarness(t *testing.T) (root, workspace, outbox string, session Session, cutoff time.Time) {
	t.Helper()
	root = codexSessionsRoot(t)
	stateDir := t.TempDir()
	outbox = filepath.Join(stateDir, "outbox.jsonl")
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", outbox)

	workspace = t.TempDir()
	session = Session{
		DeviceID:     "sess-codex-recovery",
		SessionToken: "PSE-TEST",
		TaskRoot:     workspace,
		StartedAt:    time.Now(),
	}
	return root, workspace, outbox, session, session.StartedAt.Add(-2 * time.Minute)
}

// The headline case: the human's turn reaches the outbox even though the
// rollout carries no event_msg for it, and the synthetic environment_context
// item on the same channel does NOT.
func TestWatcherRecoversHumanPromptFromResponseItem(t *testing.T) {
	root, workspace, outbox, session, cutoff := codexRecoveryHarness(t)
	const human = "refactor the ingest handler to stream instead of buffering"
	codexRolloutWithoutEventMsg(t, root, workspace, human, true)

	processors := map[string]*normalize.CodexRolloutProcessor{}
	pollCodexRollouts(session, resolvePath(workspace), cutoff, processors, false)

	got := codexOutboxPromptTexts(t, outbox)
	if len(got) != 1 {
		t.Fatalf("got %d prompts %q, want exactly 1 — the human turn must be recovered", len(got), got)
	}
	if got[0] != human {
		t.Errorf("prompt text = %q, want %q", got[0], human)
	}
}

// The Greptile P1: when the human's turn is the LAST line of the rollout there
// is no following line to flush it, so only the per-poll staleness flush can
// release it — and it must take TWO polls, never one, or a poll landing between
// the response_item and a not-yet-written event_msg would double-count.
func TestWatcherFlushesFinalPromptOnlyAfterASecondPoll(t *testing.T) {
	root, workspace, outbox, session, cutoff := codexRecoveryHarness(t)
	const human = "one last thing before I sign off"
	codexRolloutWithoutEventMsg(t, root, workspace, human, false)

	processors := map[string]*normalize.CodexRolloutProcessor{}

	pollCodexRollouts(session, resolvePath(workspace), cutoff, processors, false)
	if got := codexOutboxPromptTexts(t, outbox); len(got) != 0 {
		t.Fatalf("first poll emitted %d prompts %q, want 0 — an event_msg could still be unwritten", len(got), got)
	}

	pollCodexRollouts(session, resolvePath(workspace), cutoff, processors, false)
	got := codexOutboxPromptTexts(t, outbox)
	if len(got) != 1 {
		t.Fatalf("second poll produced %d prompts %q, want 1 — the final turn would be lost forever", len(got), got)
	}
	if got[0] != human {
		t.Errorf("prompt text = %q, want %q", got[0], human)
	}

	// A third poll must not re-emit it.
	pollCodexRollouts(session, resolvePath(workspace), cutoff, processors, false)
	if again := codexOutboxPromptTexts(t, outbox); len(again) != 1 {
		t.Errorf("prompt emitted %d times across polls, want exactly 1", len(again))
	}
}

// The regression that would be worse than the bug: on a normal rollout — where
// Codex writes BOTH the response_item copy and the event_msg — exactly one
// prompt must reach the outbox, across any number of polls.
func TestWatcherDoesNotDoubleCountWhenEventMsgIsPresent(t *testing.T) {
	root, workspace, outbox, session, cutoff := codexRecoveryHarness(t)
	const human = "add a retry to the webhook sender"

	dir := filepath.Join(root, "2026", "07", "30")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-07-30T16-38-12-019fb3e4-03e4-77f1-a022-c908046e4da4.jsonl")
	ts := time.Now().UTC().Format(time.RFC3339)
	body := codexSessionMetaLine(resolvePath(workspace), ts) +
		fmt.Sprintf(`{"timestamp":"%s","type":"response_item","payload":{"type":"message","role":"user",`+
			`"content":[{"type":"input_text","text":%q}]}}`+"\n", ts, human) +
		fmt.Sprintf(`{"timestamp":"%s","type":"event_msg","payload":{"type":"user_message","message":%q,"images":[]}}`+"\n", ts, human)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	processors := map[string]*normalize.CodexRolloutProcessor{}
	for i := 0; i < 3; i++ {
		pollCodexRollouts(session, resolvePath(workspace), cutoff, processors, false)
	}

	got := codexOutboxPromptTexts(t, outbox)
	if len(got) != 1 {
		t.Fatalf("got %d prompts %q across 3 polls, want exactly 1 — double-counting is worse than the bug this fixes",
			len(got), got)
	}
}
