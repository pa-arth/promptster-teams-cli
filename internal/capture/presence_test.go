package capture

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
)

// TestPresenceEventCarriesNoTranscriptContent is the guardrail for the privacy
// promise: a presence event must carry only benign device/environment metadata
// and never any captured transcript content. If someone widens presenceData
// with a transcript-bearing field, this fails.
func TestPresenceEventCarriesNoTranscriptContent(t *testing.T) {
	sess := Session{DeviceID: "dev-0123456789abcdef", SessionToken: fakeTeamKey}
	e := buildPresenceEvent(sess)

	if e.Kind != "presence" {
		t.Errorf("kind = %q, want presence", e.Kind)
	}
	if e.Source != presenceSource {
		t.Errorf("source = %q, want %q", e.Source, presenceSource)
	}
	if e.RawPayload != "" {
		t.Errorf("presence event carries rawPayload %q — must be empty", e.RawPayload)
	}
	if e.SessionID != sess.DeviceID {
		t.Errorf("sessionID = %q, want %q", e.SessionID, sess.DeviceID)
	}

	// The Data payload must contain EXACTLY the closed allow-list of keys.
	raw, err := json.Marshal(e.Data)
	if err != nil {
		t.Fatalf("marshal presence data: %v", err)
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal presence data: %v", err)
	}
	// `pendingEvents` is the device's own outbox backlog — a COUNT of undelivered
	// events, never anything about what is in them. It joins the closed shape
	// because the alternative (a manager being told an actively-working engineer
	// has zero active sessions, 2026-08-04) is worse than one more integer.
	//
	// `pendingOldestEventAt` is deliberately NOT in this required set: it is
	// omitted when the queue is empty, which is the state this test's fixture is
	// in. Its presence-when-non-empty is pinned by presence_pending_test.go.
	//
	// `cursorHooks` and its two counters are the device's own Cursor hook rail
	// health — one CLOSED enum word and two integers ABOUT ~/.cursor/hooks.json,
	// never anything out of it. That file is shared with every other tool on the
	// machine, so this rail must never widen to a reason string: a neighbour's
	// command line would ride out with it. The doctor's prose stays on the
	// machine, where it is safe. Pinned on the emitted bytes by
	// presence_cursor_hooks_test.go.
	//
	// The five `cursorStop*` / `cursorHook*` counters join on the same terms and
	// for a sharper reason: whether the rail is INSTALLED and whether the rail
	// WORKS are different questions, and only the first had an answer while 38%
	// of one machine's Cursor turns went missing. They are integers about OUR
	// behaviour — how many stop hooks we were handed, how many we answered, how
	// many we abandoned — and they must never widen to a cause string, which
	// would be the engineer's payload.
	// `droppedEvents` / `outboxBytes` / `outboxCapacityBytes` join on exactly the
	// same terms as `pendingEvents`, one question further on. That field says how
	// far BEHIND delivery is; it cannot say whether the queue has begun
	// DISCARDING, and on 2026-08-31..09-02 a customer lost ~15.6k events while
	// every surface we had showed `pendingEvents: 63,965` — which at ~1KB/event is
	// exactly the 64 MiB per-lane cap. Three integers ABOUT the queue: how many
	// events it threw away, how full its fullest lane is, and the cap it is being
	// measured against. Nothing about what was in them, and no reason string —
	// a drop's context is the engineer's payload, a count of drops is a fact about
	// us. Pinned on the emitted bytes by presence_drops_test.go.
	allowed := map[string]bool{
		"device": true, "cliVersion": true, "os": true, "arch": true, "watching": true,
		"pendingEvents": true, "pendingOldestEventAt": true,
		"droppedEvents": true, "outboxBytes": true, "outboxCapacityBytes": true,
		"cursorHooks": true, "cursorHookRepairs": true, "cursorHookUnverifiable": true,
		"cursorStopSeen": true, "cursorStopUsageRows": true, "cursorStopEmpty": true,
		"cursorHookOverruns": true, "cursorHookUnparsed": true,
	}
	for k := range data {
		if !allowed[k] {
			t.Errorf("presence data has unexpected field %q — presence must stay metadata-only", k)
		}
	}
	for k := range allowed {
		if k == "pendingOldestEventAt" {
			continue // omitempty: absent on an empty queue, by design
		}
		if _, ok := data[k]; !ok {
			t.Errorf("presence data missing expected field %q", k)
		}
	}

	// Belt-and-braces: no transcript-shaped field names anywhere in the full
	// signed envelope. The team key is auth (a header), never in the body.
	full, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal presence event: %v", err)
	}
	lower := strings.ToLower(string(full))
	// Banned as quoted JSON keys/values so the brand name ("promptster-...")
	// doesn't trip the bare word "prompt".
	for _, banned := range []string{`"prompt"`, `"response"`, `"assistant"`, `"content"`, `"command"`, `"diff"`, `"rawpayload"`, `"text"`, `"token"`} {
		if strings.Contains(lower, banned) {
			t.Errorf("presence envelope contains banned key %q: %s", banned, full)
		}
	}
	if strings.Contains(string(full), sess.SessionToken) {
		t.Errorf("presence envelope leaks the team key: %s", full)
	}

	// Sanity: the benign metadata is actually populated.
	if data["os"] == "" || data["arch"] == "" {
		t.Errorf("presence data missing os/arch: %v", data)
	}
}

// TestPresenceEventIsSignable confirms a presence event round-trips through the
// signing path (canonicalization must not choke on the struct payload).
func TestPresenceEventIsSignable(t *testing.T) {
	e := buildPresenceEvent(Session{DeviceID: "dev-abc", SessionToken: fakeTeamKey})
	if _, err := sign.BuildSigningMessage(e, ""); err != nil {
		t.Fatalf("presence event not signable: %v", err)
	}
}
