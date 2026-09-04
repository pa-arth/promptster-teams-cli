package capture

import (
	"fmt"
	"os"
	"path/filepath"
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
