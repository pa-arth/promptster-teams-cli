package redact

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// The three identity fields this repo's Codex normalizer now emits MUST survive
// the on-device projector.
//
// This is asserted rather than read off the allowlist table because there are
// TWO independent default-deny allowlists — this one, and the backend's
// eventFieldProjection/captureAllowlist — and a field present in only one is
// stripped SILENTLY, with no error and no telemetry. Downstream that reads as
// "the engineer is on an older CLI", which is indistinguishable from the bug it
// actually is. The backend half is pinned by its own test; this is the near half.
//
// It also pins the negative: adding an identity field must not become a licence
// to widen these kinds. Anything not named here is still dropped.
func TestCodexAssetIdentityFieldsSurviveProjection(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		data  map[string]interface{}
		keep  []string
		strip []string
	}{
		{
			name: "subagent_usage keeps the agent name",
			kind: "subagent_usage",
			data: map[string]interface{}{
				"sidechain":        true,
				"agentId":          "019fb396-9280-7710-91d4-f36e7797376b",
				"attributionAgent": "guardian",
				"inputTokens":      17718,
				"outputTokens":     237,
				"secretProse":      "the delegated agent's answer",
			},
			keep:  []string{"attributionAgent", "agentId", "sidechain"},
			strip: []string{"secretProse"},
		},
		{
			name: "tool_use keeps the skill slug",
			kind: "tool_use",
			data: map[string]interface{}{
				"tool":     "skill",
				"skill":    "promptster-probe",
				"status":   "ok",
				"skillDoc": "the SKILL.md body",
			},
			keep:  []string{"tool", "skill", "status"},
			strip: []string{"skillDoc"},
		},
		{
			name: "mcp_call keeps the server-qualified tool name",
			kind: "mcp_call",
			data: map[string]interface{}{
				"tool":        "probeserver__probe_echo",
				"status":      "ok",
				"argsPreview": "{\"message\":\"hello\"}",
			},
			keep: []string{"tool", "status"},
			// argsPreview is what the retired name-shaped branch used to emit. It
			// is tool ARGUMENTS and must not survive, whoever sets it.
			strip: []string{"argsPreview"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &event.Event{Kind: tc.kind, Data: tc.data}
			ProjectEvent(e, false)

			got, ok := e.Data.(map[string]interface{})
			if !ok {
				t.Fatalf("projected data is not a map: %#v", e.Data)
			}
			for _, k := range tc.keep {
				if _, present := got[k]; !present {
					t.Errorf("%s.%s was STRIPPED — the field the backend reads never arrives, and the surface renders as an older CLI", tc.kind, k)
				}
			}
			for _, k := range tc.strip {
				if _, present := got[k]; present {
					t.Errorf("%s.%s survived projection and must not", tc.kind, k)
				}
			}
		})
	}
}
