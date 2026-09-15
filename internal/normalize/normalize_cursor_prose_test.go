package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// Real record shapes: text-only assistant records and text followed by
// tool_use in one record are both common in local transcripts (2026-09-15).
var cursorProseLines = [][]byte{
	[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>fix the bug</user_query>"}]}}`),
	[]byte(`{"role":"assistant","message":{"content":[{"type":"text","text":"Looking at the handler."},{"type":"text","text":"It swallows the error."},{"type":"tool_use","name":"Shell","input":{"command":"go test ./..."}},{"type":"text","text":"Tests pass now."}]}}`),
	[]byte(`{"type":"turn_ended","status":"success"}`),
}

func runCursorProse(p *CursorTranscriptProcessor) []event.Event {
	var out []event.Event
	for i, l := range cursorProseLines {
		out = append(out, p.Process(l, int64(i*1000))...)
	}
	return out
}

func TestCursorProseOffIsUnchanged(t *testing.T) {
	off := runCursorProse(procWith("sess-prose"))
	if _, has := firstOfKind(off, "ai_response"); has {
		t.Fatal("captureProse=false emitted an ai_response")
	}
	on := procWith("sess-prose")
	on.CaptureProse = true
	var onNonProse []event.Event
	for _, e := range runCursorProse(on) {
		if e.Kind != "ai_response" {
			onNonProse = append(onNonProse, e)
		}
	}
	if len(off) != len(onNonProse) {
		t.Fatalf("off emitted %d events, on emitted %d non-prose", len(off), len(onNonProse))
	}
	for i := range off {
		if off[i].ID != onNonProse[i].ID || off[i].Kind != onNonProse[i].Kind {
			t.Fatalf("event %d differs: %s/%s vs %s/%s", i, off[i].Kind, off[i].ID, onNonProse[i].Kind, onNonProse[i].ID)
		}
	}
}

func TestCursorProseOnCoalescesRunsTextOnly(t *testing.T) {
	p := procWith("sess-prose")
	p.CaptureProse = true
	var prose []event.Event
	for _, e := range runCursorProse(p) {
		if e.Kind == "ai_response" {
			prose = append(prose, e)
		}
	}
	if len(prose) != 2 {
		t.Fatalf("ai_response count = %d, want 2 (one per text run)", len(prose))
	}
	if d := dataOf(t, prose[0]); len(d) != 1 || d["text"] != "Looking at the handler.\n\nIt swallows the error." {
		t.Fatalf("first run data = %#v, want text only", d)
	}
	if d := dataOf(t, prose[1]); d["text"] != "Tests pass now." {
		t.Fatalf("second run data = %#v", d)
	}
	if prose[0].Actor == nil || prose[0].Actor.Type != "ai" || prose[0].Source != "cursor" {
		t.Fatalf("envelope = %#v / %q", prose[0].Actor, prose[0].Source)
	}
	// Offset-based: re-reading the same file mints the same ids.
	again := procWith("sess-prose")
	again.CaptureProse = true
	ids := map[string]bool{}
	for _, e := range runCursorProse(again) {
		ids[e.ID] = true
	}
	if !ids[prose[0].ID] || !ids[prose[1].ID] || prose[0].ID == prose[1].ID {
		t.Fatal("prose ids are not stable and distinct")
	}
}

func TestCursorProseSidechainSkipped(t *testing.T) {
	p := procWith("sess-prose")
	p.CaptureProse = true
	p.Sidechain = true
	if _, has := firstOfKind(runCursorProse(p), "ai_response"); has {
		t.Fatal("sidechain prose emitted")
	}
}

func TestCursorProseSurvivesProjectionScrubbed(t *testing.T) {
	p := procWith("sess-prose")
	p.CaptureProse = true
	line := []byte(`{"role":"assistant","message":{"content":[{"type":"text","text":"Patched it:\n` + "```go\\nconst key = \\\"SOURCECANARY\\\"\\n```" + `\nDone."}]}}`)
	e, ok := firstOfKind(p.Process(line, 0), "ai_response")
	if !ok {
		t.Fatal("no ai_response")
	}
	redact.ProjectEvent(&e, true)
	d := dataOf(t, e)
	text, _ := d["text"].(string)
	if len(d) != 1 || !strings.Contains(text, "<code-redacted>") || !strings.Contains(text, "Patched it:") {
		t.Fatalf("projected = %#v", d)
	}
	if blob, _ := json.Marshal(e); strings.Contains(string(blob), "SOURCECANARY") {
		t.Fatalf("code survived projection: %s", blob)
	}
}
