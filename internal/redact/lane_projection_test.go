package redact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// laneBearingKinds is every kind the sidechain path can emit. Written out rather
// than derived from the allowlist on purpose: a test that reads the same table it
// is checking passes when a row is deleted, which is the failure it is here to
// catch.
var laneBearingKinds = []string{
	"prompt", "tool_use", "file_diff", "file_create", "file_delete",
	"command", "mcp_call", "task_dispatch", "subagent_usage",
}

// The lane id must survive projection on every kind that can carry it. Eight of
// these are new and one (subagent_usage) is not — and the subset is exactly the
// point: a delegate that only prompted and called one MCP tool has to be visible
// too, because an absent lane and a lane that ran nothing render identically.
func TestLaneIDSurvivesProjectionOnEveryLaneBearingKind(t *testing.T) {
	for _, kind := range laneBearingKinds {
		t.Run(kind, func(t *testing.T) {
			e := eventWithData(kind, map[string]interface{}{"agentId": "cd5866a6-d091-4188-bd14-3661e0ce0c8f"})
			ProjectEvent(&e, false)
			data, ok := e.Data.(map[string]interface{})
			if !ok {
				t.Fatalf("projected Data is %T, want map", e.Data)
			}
			if got, _ := data["agentId"].(string); got != "cd5866a6-d091-4188-bd14-3661e0ce0c8f" {
				t.Fatalf("agentId = %q, want it to survive projection", got)
			}
		})
	}
}

// THE LEAK CANARY, extended to the lane.
//
// The canary in project_test.go proves the projector drops what is not
// allowlisted. `agentId` now IS allowlisted, so that canary can no longer see
// this: an allowlisted key is one an emitter can fill with anything, and the
// cheapest identity to hand a lane is the file it came from.
//
// So the guarantee moved to where the allowlist is. A lane id that is not an
// opaque tool-minted token is dropped here — which means `meta` staying
// unallowlisted and raw `cwd` staying dropped cannot be routed around by
// putting a directory in the lane field instead.
func TestLaneIDThatLooksLikeAPathIsDropped(t *testing.T) {
	pathShaped := []struct {
		name  string
		value interface{}
	}{
		{"absolute posix path", "/Users/paarth/repos/promptster/agent.jsonl"},
		{"home-relative path", "~/repos/promptster"},
		{"relative path", "../subagents/" + leakCanary},
		{"windows path", `C:\Users\paarth\` + leakCanary},
		{"a bare filename with an extension", leakCanary + ".jsonl"},
		{"a url", "https://example.com/" + leakCanary},
		{"prose", "the agent that edited " + leakCanary},
		{"longer than any id we mint", strings.Repeat("a", 65)},
		{"empty", ""},
		{"not a string at all", map[string]interface{}{"raw": leakCanary}},
	}
	for _, tc := range pathShaped {
		t.Run(tc.name, func(t *testing.T) {
			e := eventWithData("file_diff", map[string]interface{}{
				"path": "src/a.go", "linesAdded": 3, "agentId": tc.value,
			})
			ProjectEvent(&e, false)
			data, ok := e.Data.(map[string]interface{})
			if !ok {
				t.Fatalf("projected Data is %T, want map", e.Data)
			}
			if got, present := data["agentId"]; present {
				t.Fatalf("a non-opaque agentId survived projection: %#v", got)
			}
			// Dropped to ABSENT, never to a sanitised stand-in: a lane we cannot
			// identify reports as NOT OBSERVED, and a placeholder would merge
			// every such lane into one.
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("projected event does not marshal: %v", err)
			}
			if strings.Contains(string(b), leakCanary) {
				t.Fatalf("canary survived in the lane field: %s", b)
			}
		})
	}
}

// The ids we actually mint must pass. Measured before the clamp was written:
// every agentId on the wire is lowercase hex, and every Cursor child transcript
// name is a hyphenated uuid. A clamp that rejects real traffic would take the
// lane away on the rails that already have one.
func TestLaneIDAcceptsTheShapesWeActuallyMint(t *testing.T) {
	real := []string{
		"a0c546fe2078d68f0",                    // Claude Code, 17-char hex, from buffer.jsonl
		"cd5866a6-d091-4188-bd14-3661e0ce0c8f", // Cursor child transcript uuid
		"019fb396-9280-7710-91d4-f36e7797376b", // Codex delegate thread id
		"agent_ABC-123_x",                      // underscores and mixed case
	}
	for _, id := range real {
		t.Run(id, func(t *testing.T) {
			e := eventWithData("command", map[string]interface{}{"command": "go test", "agentId": id})
			ProjectEvent(&e, false)
			data := e.Data.(map[string]interface{})
			if got, _ := data["agentId"].(string); got != id {
				t.Fatalf("agentId = %q, want %q — the clamp is rejecting real traffic", got, id)
			}
		})
	}
}

// A kind with no lane in its allowlist must not gain one by carrying the key.
// Default-deny is the mechanism; this pins that the eight additions did not
// quietly become "every kind".
func TestLaneIDDoesNotSurviveOnAKindThatDoesNotCarryIt(t *testing.T) {
	e := eventWithData("ai_response", map[string]interface{}{
		"model": "claude-opus-5", "agentId": "a0c546fe2078d68f0",
	})
	ProjectEvent(&e, false)
	if _, present := e.Data.(map[string]interface{})["agentId"]; present {
		t.Fatal("agentId survived on ai_response, which does not allowlist it")
	}
}

var _ = event.Event{}

// The lane's LABEL — what it was dispatched to do — must survive projection on
// the spend event, not merely be declared in the table. A field the device
// projector drops arrives nowhere and reads downstream exactly like a CLI that
// never sent it: no error, no telemetry, a `201`.
//
// This is the second half of the pair. `agentId` says WHICH invocation and
// `attributionAgent` says WHAT KIND, and neither separates concurrent delegates
// of the same kind — 48% of measured lanes sit in such a cluster, with an 11.3x
// cost spread inside one.
func TestLaneLabelSurvivesProjectionOnTheSpendEvent(t *testing.T) {
	e := eventWithData("subagent_usage", map[string]interface{}{
		"agentId":          "aaa9a3a70c738c722",
		"attributionAgent": "Explore",
		"summary":          "Scout backend ingest contract",
	})
	ProjectEvent(&e, false)
	data, ok := e.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("projected Data is %T, want map", e.Data)
	}
	if got, _ := data["summary"].(string); got != "Scout backend ingest contract" {
		t.Fatalf("summary = %q, want the dispatch label to survive projection", got)
	}
	if got, _ := data["agentId"].(string); got == "" {
		t.Fatal("the lane lost its id while gaining a label")
	}
}

// It is a NARROW widening. ai_response is the main chain: it has no dispatch
// behind it, so a summary arriving there is prose with no label semantics.
// Pinned so the next person adding a kind copies the narrow row.
func TestLaneLabelIsNotAllowedOnTheMainChain(t *testing.T) {
	e := eventWithData("ai_response", map[string]interface{}{"summary": "anything at all"})
	ProjectEvent(&e, false)
	data, _ := e.Data.(map[string]interface{})
	if v, ok := data["summary"]; ok {
		t.Fatalf("summary reached ai_response: %v", v)
	}
}
