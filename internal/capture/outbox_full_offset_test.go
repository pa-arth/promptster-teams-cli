package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
)

// The tests here pin ONE rule on both transcript rails: a record whose events
// the outbox REFUSED has not been consumed, so the durable read offset must not
// move past it.
//
// The rule is not decorative. Both watchers commit their offset from the bytes
// the reader consumed, and the full-queue drop used to return nil — "queued" —
// so a wedged ingest advanced the offset over records whose events had just been
// discarded. Every one of those events was still sitting in a transcript on
// disk, in a file the watcher had recorded as fully read; nothing would ever look
// at those bytes again. That is the ops.ai incident: a three-day wedge dropped
// ~54,000 events, and recovery needed a hand-rewound cursor because the CLI had
// no idea anything was missing.
//
// Both tests therefore assert TWO things, and the second is the one that matters:
// the offset did not advance, and the next poll re-reads and queues the same
// records once the queue has room.

// fillLiveOutbox grows the live lane to its cap so the next append is dropped.
// Sparse — no 64 MiB is ever written.
func fillLiveOutbox(t *testing.T) string {
	t.Helper()
	p := os.Getenv("PROMPTSTER_OUTBOX_PATH")
	if p == "" {
		t.Fatal("PROMPTSTER_OUTBOX_PATH must be set before filling the live lane")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open live outbox: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(outbox.OutboxMaxBytes); err != nil {
		t.Fatalf("grow live outbox: %v", err)
	}
	return p
}

// queuedLines counts the events sitting in the live lane, ignoring the sparse
// filler fillLiveOutbox wrote (which contains no newline-terminated records).
func queuedLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// TestClaudeTailDoesNotAdvancePastAnEventTheOutboxDropped is the Claude half.
func TestClaudeTailDoesNotAdvancePastAnEventTheOutboxDropped(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))
	outboxPath := fillLiveOutbox(t)

	workspace := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	line1 := fmt.Sprintf(`{"type":"user","uuid":"rec-1","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"first"}}`+"\n", workspace, ts)
	line2 := fmt.Sprintf(`{"type":"user","uuid":"rec-2","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"second"}}`+"\n", workspace, ts)
	path := filepath.Join(t.TempDir(), "wedged.jsonl")
	if err := os.WriteFile(path, []byte(line1+line2), 0o600); err != nil {
		t.Fatal(err)
	}
	total := int64(len(line1) + len(line2))

	key := claudeProgressKey(path)
	progress := claudeWatchProgress{Offsets: map[string]int64{}, Discarding: map[string]bool{}, Match: map[string]string{key: "yes"}, V: claudeProgressSchemaV}
	proc := normalize.NewClaudeTranscriptProcessor("wedged-claude")
	session := Session{DeviceID: "device", SessionToken: "PSE-TEST", TaskRoot: workspace}

	parsed, res := tailClaudeTranscript(path, progress, proc, session, false, false, total*4, true)
	if parsed == 0 {
		t.Fatal("no events parsed — the fixture must reach the outbox for this test to mean anything")
	}
	if got := progress.Offsets[key]; got != 0 {
		t.Fatalf("offset advanced to %d after the outbox dropped the first record's event; want 0 — those bytes were read but the event was discarded", got)
	}
	if res.consumed != 0 {
		t.Errorf("outcome reports %d bytes consumed, want 0: consumed is what the offset is committed from", res.consumed)
	}
	if !res.truncated {
		t.Error("outcome must report readable bytes left behind, or the caller reads a wedged file as fully drained")
	}
	if n := queuedLines(t, outboxPath); n != 0 {
		t.Fatalf("%d event(s) reached a full queue", n)
	}

	// The drain catches up. The SAME records must now be read and queued — that
	// is the whole point of not advancing: the wedge self-heals with no release
	// and no cursor surgery.
	if err := os.Truncate(outboxPath, 0); err != nil {
		t.Fatalf("empty the live outbox: %v", err)
	}
	parsed, res = tailClaudeTranscript(path, progress, proc, session, false, false, total*4, true)
	if parsed == 0 {
		t.Fatal("the deferred records were not re-read on the next poll")
	}
	if res.consumed != total || progress.Offsets[key] != total {
		t.Fatalf("recovery poll consumed %d bytes to offset %d, want the whole file (%d)", res.consumed, progress.Offsets[key], total)
	}
	if n := queuedLines(t, outboxPath); n < 2 {
		t.Errorf("live lane holds %d event(s) after recovery, want both records' events", n)
	}
}

// TestCodexTailDoesNotAdvancePastAnEventTheOutboxDropped is the Codex half, and
// additionally pins the PREFIX shape of the rewind. The leading turn_context
// record queues nothing (it only stashes the turn's model), so the offset is
// free to advance over it — and must then stop exactly at the first record whose
// event the queue refused, leaving every later record unread too. An offset is a
// single number and cannot record a hole, which is the same constraint
// outbox.advanceOver lives under.
func TestCodexTailDoesNotAdvancePastAnEventTheOutboxDropped(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))
	outboxPath := fillLiveOutbox(t)

	workspace := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	line1 := fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5-codex"}}`+"\n", ts)
	line2 := codexSessionMetaLine(workspace, ts)
	line3 := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":"first","images":[]}}`+"\n", ts)
	line4 := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":"second","images":[]}}`+"\n", ts)
	path := filepath.Join(t.TempDir(), "rollout-wedged.jsonl")
	if err := os.WriteFile(path, []byte(line1+line2+line3+line4), 0o600); err != nil {
		t.Fatal(err)
	}
	total := int64(len(line1) + len(line2) + len(line3) + len(line4))

	progress := codexWatchProgress{Offsets: map[string]int64{}, Discarding: map[string]bool{}, Match: map[string]string{path: "yes"}, V: codexProgressSchemaV}
	proc := normalize.NewCodexRolloutProcessor("wedged-codex")
	session := Session{DeviceID: "device", SessionToken: "PSE-TEST", TaskRoot: workspace}

	_, res := tailCodexRollout(path, progress, proc, session, false, total*4, true)
	if got := progress.Offsets[path]; got != int64(len(line1)) {
		t.Fatalf("offset = %d after the outbox dropped this rollout's first event; want %d — the record ahead of it queued nothing and may be passed, the refused record may not", got, len(line1))
	}
	if res.consumed != int64(len(line1)) {
		t.Errorf("outcome reports %d bytes consumed, want %d", res.consumed, len(line1))
	}
	if !res.truncated {
		t.Error("outcome must report readable bytes left behind, or the caller reads a wedged rollout as fully drained")
	}
	if n := queuedLines(t, outboxPath); n != 0 {
		t.Fatalf("%d event(s) reached a full queue", n)
	}

	if err := os.Truncate(outboxPath, 0); err != nil {
		t.Fatalf("empty the live outbox: %v", err)
	}
	queued, res := tailCodexRollout(path, progress, proc, session, false, total*4, true)
	if queued == 0 {
		t.Fatal("the deferred records were not re-read on the next poll")
	}
	if progress.Offsets[path] != total {
		t.Fatalf("recovery poll left the offset at %d, want the whole file (%d); consumed=%d", progress.Offsets[path], total, res.consumed)
	}
	if n := queuedLines(t, outboxPath); n < 3 {
		t.Errorf("live lane holds %d event(s) after recovery, want the session start and both user messages", n)
	}
}

// queuedKinds lists the event kinds sitting in the live lane, in order. The
// sparse filler fillLiveOutbox wrote holds no newline-terminated records, so it
// contributes nothing.
func queuedKinds(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var kinds []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Kind != "" {
			kinds = append(kinds, ev.Kind)
		}
	}
	return kinds
}

// TestClaudeTailRewindsPastTheRecordsThatBuiltAFlushedEvent covers the half a
// per-record rewind alone does NOT close, and it is the half that carries
// ai_response.
//
// Not every event belongs to the record that produced it. The processor
// accumulates an assistant message across many lines and mints it when a LATER
// line closes the turn — and that flush releases the accumulator and memoizes
// the message id as emitted before the event ever reaches the queue. So a rewind
// that stops at the refusing record puts none of the bytes that BUILT the event
// back in play: the next poll re-reads the closing line, the flush finds nothing
// buffered, and the ai_response is gone exactly as permanently as if the offset
// had advanced. Rewinding to the last record the processor was clean at, and
// dropping what it holds, is what makes the replay reproduce it.
//
// The fixture is the minimum that shows it: an assistant line that only
// accumulates, then a user line whose flush is what the full queue refuses.
func TestClaudeTailRewindsPastTheRecordsThatBuiltAFlushedEvent(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))
	outboxPath := fillLiveOutbox(t)

	workspace := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	// Accumulates only — no event leaves the processor on this record.
	assistant := fmt.Sprintf(`{"type":"assistant","uuid":"asst-1","cwd":%q,"timestamp":%q,"message":{"id":"msg_1","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"the answer"}],"usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", workspace, ts)
	// Closes the turn: flushes msg_1 as ai_response, THEN mints its own prompt.
	user := fmt.Sprintf(`{"type":"user","uuid":"rec-2","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"and next"}}`+"\n", workspace, ts)
	path := filepath.Join(t.TempDir(), "flush-wedged.jsonl")
	if err := os.WriteFile(path, []byte(assistant+user), 0o600); err != nil {
		t.Fatal(err)
	}
	total := int64(len(assistant) + len(user))

	key := claudeProgressKey(path)
	progress := claudeWatchProgress{Offsets: map[string]int64{}, Discarding: map[string]bool{}, Match: map[string]string{key: "yes"}, V: claudeProgressSchemaV}
	proc := normalize.NewClaudeTranscriptProcessor("wedged-flush")
	session := Session{DeviceID: "device", SessionToken: "PSE-TEST", TaskRoot: workspace}

	if _, res := tailClaudeTranscript(path, progress, proc, session, false, false, total*4, true); !res.truncated {
		t.Fatal("outcome must report readable bytes left behind after the queue refused the flushed event")
	}
	// The assistant record queued nothing, but it is not passable: it is the only
	// copy of the response the refused event was built from.
	if got := progress.Offsets[key]; got != 0 {
		t.Fatalf("offset = %d after the queue refused a flush-derived event; want 0 — the accumulator was built from byte 0 and has just been discarded", got)
	}
	if n := queuedLines(t, outboxPath); n != 0 {
		t.Fatalf("%d event(s) reached a full queue", n)
	}

	// The drain catches up: the replay must reproduce the ai_response, not just
	// the prompt. Getting the prompt alone back is the exact silent loss.
	if err := os.Truncate(outboxPath, 0); err != nil {
		t.Fatalf("empty the live outbox: %v", err)
	}
	if _, res := tailClaudeTranscript(path, progress, proc, session, false, false, total*4, true); res.consumed != total {
		t.Fatalf("recovery poll consumed %d bytes, want the whole file (%d)", res.consumed, total)
	}
	kinds := queuedKinds(t, outboxPath)
	var sawResponse bool
	for _, k := range kinds {
		if k == "ai_response" {
			sawResponse = true
		}
	}
	if !sawResponse {
		t.Fatalf("live lane holds %v after recovery — the flushed ai_response was never rebuilt, which is the permanent loss this rewind exists to stop", kinds)
	}
}

// TestCodexTailRewindsPastTheRecordsThatBuiltAFlushedEvent is the Codex half of
// the same rule, on its own buffered shape: a function_call sits in the
// processor until the record carrying its output arrives, and pairing them
// DELETES the pending call before the event reaches the queue. A rewind that
// stopped at the output record would replay it against a processor that no
// longer holds the call, and the tool event would never be rebuilt.
//
// The two records are split across polls deliberately — a budget-capped first
// poll commits the call as read while the queue is healthy — so the refusal
// lands on the output record alone. That is the shape a real wedge takes: the
// bytes that built the event were consumed long before the queue filled.
func TestCodexTailRewindsPastTheRecordsThatBuiltAFlushedEvent(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	outboxPath := filepath.Join(stateDir, "outbox.jsonl")
	t.Setenv("PROMPTSTER_OUTBOX_PATH", outboxPath)

	workspace := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	meta := codexSessionMetaLine(workspace, ts)
	// Buffers the call; emits nothing.
	call := fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"echo done\"}","call_id":"call_B"}}`+"\n", ts)
	// Pairs with it, emits the tool event, and deletes the pending call.
	output := fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call_output","call_id":"call_B","output":"Process exited with code 0\nOutput:\ndone\n"}}`+"\n", ts)
	path := filepath.Join(t.TempDir(), "rollout-flush-wedged.jsonl")
	if err := os.WriteFile(path, []byte(meta+call+output), 0o600); err != nil {
		t.Fatal(err)
	}
	head := int64(len(meta) + len(call))
	total := head + int64(len(output))

	progress := codexWatchProgress{Offsets: map[string]int64{}, Discarding: map[string]bool{}, Match: map[string]string{path: "yes"}, V: codexProgressSchemaV}
	proc := normalize.NewCodexRolloutProcessor("wedged-codex-flush")
	session := Session{DeviceID: "device", SessionToken: "PSE-TEST", TaskRoot: workspace}

	// Poll 1, healthy queue, budget stops exactly after the call record.
	if _, res := tailCodexRollout(path, progress, proc, session, false, head, true); res.consumed != head {
		t.Fatalf("setup poll consumed %d bytes, want %d (session_meta + the call record)", res.consumed, head)
	}
	if progress.Offsets[path] != head {
		t.Fatalf("setup poll left the offset at %d, want %d", progress.Offsets[path], head)
	}

	// Poll 2, wedged queue: the only record read is the output, and the call it
	// pairs with is already behind the committed offset.
	fillLiveOutbox(t)
	if _, res := tailCodexRollout(path, progress, proc, session, false, total*4, true); !res.truncated {
		t.Fatal("outcome must report readable bytes left behind after the queue refused the paired tool event")
	}
	if got := progress.Offsets[path]; got != int64(len(meta)) {
		t.Fatalf("offset = %d after the queue refused a pair-derived event; want %d — the call record is the only copy of what built it, and the processor has just been emptied", got, len(meta))
	}

	// Poll 3, drained: the replay must rebuild the tool event, not just re-read
	// the output line into a processor with nothing to pair it against.
	if err := os.Truncate(outboxPath, 0); err != nil {
		t.Fatalf("empty the live outbox: %v", err)
	}
	if _, res := tailCodexRollout(path, progress, proc, session, false, total*4, true); progress.Offsets[path] != total {
		t.Fatalf("recovery poll left the offset at %d, want the whole file (%d); consumed=%d", progress.Offsets[path], total, res.consumed)
	}
	kinds := queuedKinds(t, outboxPath)
	var sawTool bool
	for _, k := range kinds {
		if k == "command" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatalf("live lane holds %v after recovery — the tool event whose call record was already consumed was never rebuilt", kinds)
	}
}
