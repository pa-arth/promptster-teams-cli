package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeSessionIDFromPath pins the two transcript shapes, especially the
// subagent one: its filename is an AGENT id, so taking the filename would
// fragment every subagent into a phantom session instead of rolling its work up
// to the session that spawned it.
func TestClaudeSessionIDFromPath(t *testing.T) {
	const parent = "3c9d10eb-3391-44b6-908f-7bb3210ed5ce"
	projects := "/Users/x/.claude/projects/-Users-x-repos-thing"

	cases := []struct {
		name string
		path string
		want string
	}{
		{
			name: "top-level transcript is its own session",
			path: filepath.Join(projects, parent+".jsonl"),
			want: parent,
		},
		{
			name: "subagent rolls up to the parent session, not its agent id",
			path: filepath.Join(projects, parent, "subagents", "agent-a5b68e74b1330ca8f.jsonl"),
			want: parent,
		},
		{
			name: "unrecognised agent shape defers to the transcript's own sessionId",
			path: filepath.Join(projects, "agent-orphan.jsonl"),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeSessionIDFromPath(tc.path); got != tc.want {
				t.Errorf("claudeSessionIDFromPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestClaudeSubagentSharesParentSessionID is the roll-up contract stated
// directly: a subagent transcript and its parent must resolve to the SAME
// session id, or subagent work splinters off into sessions of its own.
func TestClaudeSubagentSharesParentSessionID(t *testing.T) {
	const parent = "951ef885-1df9-461b-b5aa-bbfcea20ff84"
	projects := "/Users/x/.claude/projects/-Users-x-repos-thing"

	top := claudeSessionIDFromPath(filepath.Join(projects, parent+".jsonl"))
	sub := claudeSessionIDFromPath(filepath.Join(projects, parent, "subagents", "agent-a590685ae575e7390.jsonl"))

	if top != sub {
		t.Errorf("subagent session %q != parent session %q — subagent work would not roll up", sub, top)
	}
	if top != parent {
		t.Errorf("session = %q, want %q", top, parent)
	}
}

// TestCodexSessionIDFromPath pins that the uuid is taken from the END of the
// filename: the leading timestamp is also dash-separated, so a naive split on
// "-" yields a fragment of a date.
func TestCodexSessionIDFromPath(t *testing.T) {
	const want = "019eb780-3081-7ce0-9ba0-8a0bad13b532"
	got := codexSessionIDFromPath("/Users/x/.codex/sessions/2026/06/11/rollout-2026-06-11T11-24-52-" + want + ".jsonl")
	if got != want {
		t.Errorf("codexSessionIDFromPath = %q, want %q", got, want)
	}
	if got := codexSessionIDFromPath("/Users/x/.codex/sessions/2026/06/11/rollout-garbage.jsonl"); got != "" {
		t.Errorf("unparseable rollout = %q, want \"\" so the normalizer falls back to session_meta", got)
	}
}

// TestPresenceDeviceIsIndependentOfEnvelopeSession is the seat-inflation
// regression, and the highest-risk line in this change.
//
// data.device backs seat utilization and per-device "last seen". It used to be
// read from the envelope's session id, which was safe only while that id WAS the
// device id. If it ever tracks a real per-session id, every watch restart looks
// like a brand-new device and seat counts inflate without bound — silently, and
// in the direction that overcounts a customer's usage.
func TestPresenceDeviceIsIndependentOfEnvelopeSession(t *testing.T) {
	const device = "dev-eaadff93e23fe6d4"
	e := buildPresenceEvent(Session{DeviceID: device, SessionToken: fakeTeamKey})

	data, ok := e.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("presence Data is %T, want map[string]interface{}", e.Data)
	}
	if got := data["device"]; got != device {
		t.Errorf("data.device = %v, want %q — seat utilization keys on this", got, device)
	}
	if e.DeviceID != device {
		t.Errorf("envelope deviceId = %q, want %q", e.DeviceID, device)
	}
	// Presence is device-scoped: its envelope session stays the device id so the
	// backend can skip minting a session row for it.
	if e.SessionID != device {
		t.Errorf("presence envelope sessionId = %q, want the device id %q", e.SessionID, device)
	}
	if !strings.HasPrefix(e.SessionID, "dev-") {
		t.Errorf("presence envelope sessionId = %q, want a dev-* shape", e.SessionID)
	}
}

// TestCensusIsDeviceScoped: same contract as presence — the census describes the
// machine, not a session.
func TestCensusIsDeviceScoped(t *testing.T) {
	const device = "dev-census-scope"
	e := buildConfigCensusEvent(Session{DeviceID: device, TaskRoot: t.TempDir()})

	if e.SessionID != device {
		t.Errorf("census envelope sessionId = %q, want the device id %q", e.SessionID, device)
	}
	if e.DeviceID != device {
		t.Errorf("census envelope deviceId = %q, want %q", e.DeviceID, device)
	}
}

// TestAiPathsLedgerKeepsSessionsApart pins the thrash regression. The ledger used
// to hold ONE session and wipe itself whenever a different id appeared —
// survivable only while every event carried the same device-wide id. With real
// per-session ids, concurrent Claude and Codex sessions would erase each other
// on every write.
func TestAiPathsLedgerKeepsSessionsApart(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	recordAiTouchedPath("sess-claude", "src/a.go")
	recordAiTouchedPath("sess-codex", "src/b.go")
	recordAiTouchedPath("sess-claude", "src/c.go")

	ledger := readAiPathsLedgerForTest(t)
	claude := ledger.Sessions["sess-claude"]
	codex := ledger.Sessions["sess-codex"]

	if !claude.Paths["src/a.go"] {
		t.Error("sess-claude lost src/a.go — a concurrent session's write wiped it")
	}
	if !claude.Paths["src/c.go"] {
		t.Error("sess-claude missing src/c.go")
	}
	if !codex.Paths["src/b.go"] {
		t.Error("sess-codex lost src/b.go")
	}
	if len(ledger.Sessions) != 2 {
		t.Errorf("ledger holds %d sessions, want 2", len(ledger.Sessions))
	}
}

// TestAiPathsLedgerPrunesButKeepsActive: the ledger stays bounded, and the
// session currently editing can never evict itself.
func TestAiPathsLedgerPrunesButKeepsActive(t *testing.T) {
	nowMs := int64(1_000_000_000_000)
	ledger := aiPathsLedger{V: aiPathsLedgerVersion, Sessions: map[string]aiPathsEntry{}}
	ledger.Sessions["stale"] = aiPathsEntry{Paths: map[string]bool{"x": true}, TsMs: nowMs - aiPathsTTL.Milliseconds() - 1}
	ledger.Sessions["active"] = aiPathsEntry{Paths: map[string]bool{"y": true}, TsMs: nowMs}

	pruneAiPaths(&ledger, nowMs)
	if _, ok := ledger.Sessions["stale"]; ok {
		t.Error("stale session survived TTL prune")
	}
	if _, ok := ledger.Sessions["active"]; !ok {
		t.Error("active session was pruned")
	}

	for i := 0; i < aiPathsMaxSessions+10; i++ {
		ledger.Sessions["old"+strings.Repeat("x", i)] = aiPathsEntry{Paths: map[string]bool{}, TsMs: nowMs - 1}
	}
	pruneAiPaths(&ledger, nowMs)
	if len(ledger.Sessions) > aiPathsMaxSessions {
		t.Errorf("ledger holds %d sessions, want <= %d", len(ledger.Sessions), aiPathsMaxSessions)
	}
	if _, ok := ledger.Sessions["active"]; !ok {
		t.Error("the actively-editing session was evicted by the cap")
	}
}

// readAiPathsLedgerForTest reads the on-disk ledger.
func readAiPathsLedgerForTest(t *testing.T) aiPathsLedger {
	t.Helper()
	data, err := os.ReadFile(aiPathsLedgerPath())
	if err != nil {
		t.Fatalf("read ai-paths ledger: %v", err)
	}
	var l aiPathsLedger
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatalf("unmarshal ai-paths ledger: %v", err)
	}
	return l
}
