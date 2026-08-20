package normalize

import (
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// The Claude rail's context window comes from the status-line blob, not the
// transcript — the transcript carries no such field anywhere. These tests pin
// the three things that make that safe: it is stamped when it describes the
// turn, it is ABSENT rather than wrong when it does not, and it survives the
// on-device allowlist.

const cwTurnTs = "2026-08-20T15:00:00.000Z"

// cwAssistantLine is one assistant record with usage — the line that becomes an
// ai_response.
const cwAssistantLine = `{"type":"assistant","message":{"id":"msg-cw","model":"claude-opus-5","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":12,"output_tokens":34,"cache_read_input_tokens":181017,"cache_creation_input_tokens":3558}},"requestId":"req-cw","timestamp":"2026-08-20T15:00:00.000Z","sessionId":"sess-cw"}`

// cwWindowAt builds a reading observed skew away from the turn above.
func cwWindowAt(tokens int64, skew time.Duration) ClaudeContextWindow {
	turn, err := time.Parse(time.RFC3339, cwTurnTs)
	if err != nil {
		panic(err)
	}
	return ClaudeContextWindow{Tokens: tokens, ObservedAt: turn.Add(skew).Unix()}
}

func cwAiResponse(t *testing.T, p *ClaudeTranscriptProcessor) event.Event {
	t.Helper()
	for _, e := range processAll(t, p, cwAssistantLine) {
		if e.Kind == "ai_response" {
			return e
		}
	}
	t.Fatal("no ai_response event")
	return event.Event{}
}

// TestClaudeCarriesContextWindow: the whole point. 1_000_000 is not a stand-in —
// it is what Claude Code 2.1.237 reports for model id `claude-opus-5` on a
// 1M-context session, the SAME id that reports 200_000 elsewhere. That is why
// the window is read from the vendor and never derived from the model id: the id
// is not a key a lookup table could be built on, even in principle.
func TestClaudeCarriesContextWindow(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-cw")
	p.ContextWindow = cwWindowAt(1_000_000, 0)
	e := cwAiResponse(t, p)
	if got := dm(e)["contextWindowTokens"]; got != int64(1_000_000) {
		t.Fatalf("contextWindowTokens = %v (%T), want int64(1000000)", got, got)
	}
}

// TestClaudeContextWindowAbsentNotZero: no reading (no shim installed, or the
// shim shadowed by a higher settings layer) must OMIT the key, never send 0. A 0
// window downstream is a division by zero or a session drawn as 100% full.
func TestClaudeContextWindowAbsentNotZero(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-cw")
	e := cwAiResponse(t, p)
	if _, present := dm(e)["contextWindowTokens"]; present {
		t.Fatal("contextWindowTokens must be ABSENT with no reading, not 0")
	}
}

// TestClaudeContextWindowRejectsStaleReading: the failure this bound exists for
// is a watcher catching up on a backlog, or a session resumed hours later under
// a different model, stamping TODAY's window onto a turn that ran against a
// different one. Absent renders as "ceiling unknown"; wrong renders a confident
// ratio nobody can tell was invented.
//
// Both directions, because a reading from the future is as unrelated to a turn
// as one from the past.
func TestClaudeContextWindowRejectsStaleReading(t *testing.T) {
	for _, tc := range []struct {
		name string
		skew time.Duration
	}{
		{"observed long before the turn", -90 * time.Minute},
		{"observed long after the turn", 90 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewClaudeTranscriptProcessor("sess-cw")
			p.ContextWindow = cwWindowAt(200_000, tc.skew)
			if _, present := dm(cwAiResponse(t, p))["contextWindowTokens"]; present {
				t.Fatal("a reading outside the skew bound must not be stamped")
			}
		})
	}
}

// TestClaudeContextWindowAcceptsNearbyReading: the bound must not be so tight
// that it rejects normal operation. The status line redraws continuously while a
// session is live, so a real reading is seconds old; a minute of slack proves the
// bound is a stale-pairing guard and not an accidental off switch.
func TestClaudeContextWindowAcceptsNearbyReading(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-cw")
	p.ContextWindow = cwWindowAt(200_000, -time.Minute)
	if got := dm(cwAiResponse(t, p))["contextWindowTokens"]; got != int64(200_000) {
		t.Fatalf("contextWindowTokens = %v, want int64(200000) for a one-minute-old reading", got)
	}
}

// TestClaudeContextWindowNotOnSubagentUsage: the blob reports the MAIN-LOOP
// model's window. A delegate may run a different model against a different
// ceiling, so stamping the parent's window on subagent_usage would be the same
// population error the engineer rollup was fixed for, one level down — a peak
// paired with a window that was never its own.
func TestClaudeContextWindowNotOnSubagentUsage(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-cw")
	p.UsageOnly = true
	// Set it anyway: the watcher declines to populate this on sidechain
	// processors, and the emitter must decline independently. Two guards,
	// because one of them is a call site somebody can move.
	p.ContextWindow = cwWindowAt(1_000_000, 0)
	var sub *event.Event
	for _, e := range processAll(t, p, cwAssistantLine) {
		if e.Kind == "subagent_usage" {
			ev := e
			sub = &ev
		}
		if e.Kind == "ai_response" {
			t.Fatal("UsageOnly must never emit ai_response")
		}
	}
	if sub == nil {
		t.Fatal("no subagent_usage event")
	}
	if _, present := dm(*sub)["contextWindowTokens"]; present {
		t.Fatal("subagent_usage must NOT carry the main loop's context window")
	}
}

// TestClaudeContextWindowSurvivesProjection: the on-device allowlist is
// default-deny, so a field the normalizer emits and the allowlist does not name
// is stripped SILENTLY — and downstream that is indistinguishable from "the
// vendor doesn't send it". Emitter and boundary are asserted together for the
// same reason the Codex side does it: this exact failure has cost this package
// two fields for ten days each.
func TestClaudeContextWindowSurvivesProjection(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-cw")
	p.ContextWindow = cwWindowAt(1_000_000, 0)
	ev := cwAiResponse(t, p)
	redact.ProjectEvent(&ev, false)
	got, present := dm(ev)["contextWindowTokens"]
	if !present {
		t.Fatal("contextWindowTokens was STRIPPED by the on-device allowlist — the silent-strip failure this test exists for")
	}
	if got != int64(1_000_000) {
		t.Fatalf("contextWindowTokens = %v (%T) after projection, want int64(1000000)", got, got)
	}
}

// TestClaudeContextWindowStampedWithoutUsage: the window is a property of the
// SESSION, not of this turn's counters. A turn whose usage block is missing or
// unparseable still ran against a real ceiling, and dropping the ceiling with
// the counters would blind the denominator exactly where the numerator is
// already weakest. Same rule as the Codex rail, where it is enforced by placing
// the stamp outside the input>0||output>0 gate.
func TestClaudeContextWindowStampedWithoutUsage(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-cw")
	p.ContextWindow = cwWindowAt(200_000, 0)
	var got interface{}
	for _, e := range processAll(t, p,
		`{"type":"assistant","message":{"id":"msg-nousage","model":"claude-opus-5","content":[{"type":"text","text":"done"}]},"requestId":"req-nousage","timestamp":"2026-08-20T15:00:00.000Z","sessionId":"sess-cw"}`,
	) {
		if e.Kind == "ai_response" {
			got = dm(e)["contextWindowTokens"]
		}
	}
	if got != int64(200_000) {
		t.Fatalf("contextWindowTokens = %v, want int64(200000) on a turn with no usage block", got)
	}
}
