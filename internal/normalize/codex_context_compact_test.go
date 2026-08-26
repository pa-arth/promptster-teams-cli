package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// Real compaction lines, copied verbatim from a local rollout (codex-cli
// 0.146.0). The `compacted` record's `replacement_history` is truncated here to
// one entry — the point of the fixture is that BOTH lines are fed in and only
// ONE event comes out, so the shape of the payload we deliberately do not read
// matters less than its presence.
const (
	codexCompactedRecord     = `{"timestamp":"2026-08-03T22:15:03.710Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","id":"msg_x","role":"user","content":[{"type":"input_text","text":"the entire prior conversation, verbatim"}]}]}}`
	codexCompactedMarker     = `{"timestamp":"2026-08-03T22:15:03.833Z","type":"event_msg","payload":{"type":"context_compacted"}}`
	codexCompactMainMeta     = `{"timestamp":"2026-08-03T20:34:12.000Z","type":"session_meta","payload":{"id":"019fc955-853f-7793-bcd6-d725698b08d2","cwd":"/tmp/ws","originator":"codex_cli_rs","cli_version":"0.146.0","model_provider":"openai"}}`
	codexCompactSubagentMeta = `{"timestamp":"2026-08-03T20:34:12.000Z","type":"session_meta","payload":{"id":"019fc955-thread","parent_thread_id":"019fc955-853f-7793-bcd6-d725698b08d2","thread_source":"subagent","cwd":"/tmp/ws","originator":"codex_cli_rs","cli_version":"0.146.0","model_provider":"openai"}}`
)

func codexCompactEventsFor(t *testing.T, sessionID string, lines ...string) []event.Event {
	t.Helper()
	p := NewCodexRolloutProcessor(sessionID)
	var out []event.Event
	for _, l := range lines {
		out = append(out, p.Process([]byte(l))...)
	}
	return out
}

// A compaction in the human's own thread becomes exactly one context_compact.
// Before this, codex sessions carried ZERO of them — teams prod held 5,723
// codex-emitted timeline events and not one reset, which is what made
// `context_hygiene` unanswerable-but-answered on that rail.
func TestCodexCompactionEmitsOneResetEvent(t *testing.T) {
	events := codexCompactEventsFor(t, "sess-1", codexCompactMainMeta, codexCompactedRecord, codexCompactedMarker)

	var compacts []event.Event
	for _, e := range events {
		if e.Kind == "context_compact" {
			compacts = append(compacts, e)
		}
	}
	if len(compacts) != 1 {
		t.Fatalf("got %d context_compact events, want exactly 1 (the `compacted` record and the "+
			"`context_compacted` marker arrive 1:1; reading both double-counts every reset): %+v", len(compacts), compacts)
	}

	e := compacts[0]
	if e.Source != "codex" {
		t.Errorf("source = %q, want codex", e.Source)
	}
	if e.SessionID != "sess-1" {
		t.Errorf("sessionId = %q, want sess-1", e.SessionID)
	}
	if e.Actor.Type != event.SystemActor().Type {
		t.Errorf("actor = %+v, want the system actor (a compaction is not something the human typed, "+
			"and the Claude compact_boundary path stamps it the same way)", e.Actor)
	}
	// The marker's ts, not the `compacted` record's — the marker is what we key on.
	if got := e.Ts; got == "" {
		t.Error("event carries no timestamp")
	}
}

// The trigger is NOT invented. Claude reads auto-vs-manual off compactMetadata;
// the codex rollout has no equivalent and records no slash commands at all, so
// asserting one would be the same class of manufactured fact that produced the
// false `developing` this whole change exists to undo.
func TestCodexCompactionCarriesNoInventedTrigger(t *testing.T) {
	events := codexCompactEventsFor(t, "sess-1", codexCompactMainMeta, codexCompactedRecord, codexCompactedMarker)
	for _, e := range events {
		if e.Kind != "context_compact" {
			continue
		}
		data, _ := e.Data.(map[string]interface{})
		if v, ok := data["trigger"]; ok {
			t.Errorf("context_compact carries trigger=%v; codex cannot distinguish auto from manual", v)
		}
		for _, k := range []string{"preTokens", "postTokens", "contextPct"} {
			if v, ok := data[k]; ok {
				t.Errorf("context_compact carries %s=%v; codex reports a running SESSION total, "+
					"never the live context size", k, v)
			}
		}
	}
}

// The `compacted` record's replacement_history is the whole prior conversation.
// Keying off the marker is what keeps it out of the event — and out of the
// outbox — by construction rather than by redaction.
func TestCodexCompactionNeverCarriesReplacementHistory(t *testing.T) {
	events := codexCompactEventsFor(t, "sess-1", codexCompactMainMeta, codexCompactedRecord, codexCompactedMarker)
	for _, e := range events {
		if e.Kind != "context_compact" {
			continue
		}
		ev := e
		redact.ProjectEvent(&ev, false)
		blob, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(blob), "the entire prior conversation") {
			t.Fatalf("projected context_compact leaked replacement_history: %s", blob)
		}
	}
}

// A delegated thread's compaction is the subagent hitting ITS context wall.
// Attributing it to the engineer would credit them with a reset they never made,
// the mirror of the miss this change fixes.
func TestCodexSubagentCompactionIsDropped(t *testing.T) {
	events := codexCompactEventsFor(t, "sess-1", codexCompactSubagentMeta, codexCompactedRecord, codexCompactedMarker)
	for _, e := range events {
		if e.Kind == "context_compact" {
			t.Fatalf("delegated thread emitted a context_compact: %+v", e)
		}
	}
}

// Two watchers replaying the same rollout (restart, resume, fork) must not mint
// two rows for one reset — the same stable-id contract every other codex event
// is held to.
func TestCodexCompactionEventIDIsStable(t *testing.T) {
	first := codexCompactEventsFor(t, "sess-1", codexCompactMainMeta, codexCompactedRecord, codexCompactedMarker)
	second := codexCompactEventsFor(t, "sess-1", codexCompactMainMeta, codexCompactedRecord, codexCompactedMarker)

	idOf := func(evs []event.Event) string {
		for _, e := range evs {
			if e.Kind == "context_compact" {
				return e.ID
			}
		}
		return ""
	}
	a, b := idOf(first), idOf(second)
	if a == "" || b == "" {
		t.Fatalf("no context_compact emitted (a=%q b=%q)", a, b)
	}
	if a != b {
		t.Errorf("context_compact id is not stable across replays: %q vs %q", a, b)
	}
}
