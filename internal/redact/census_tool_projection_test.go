package redact

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// The census's tool discriminator must survive projection — and nothing else may
// start surviving alongside it.
//
// `skills`, `plugins` and `mcpServers` are array-of-object fields with their own
// ELEMENT allowlists, which is a second place a field can be dropped silently.
// The top-level projection is key-only, so an element field absent from
// projectArrayElementAllowlist is stripped with no error and no telemetry —
// downstream that reads as "an older CLI", which is indistinguishable from the
// bug. Asserting the table by reading it would prove nothing; this runs it.
func TestCensusToolFieldSurvivesElementProjection(t *testing.T) {
	e := &event.Event{
		Kind: "config_census",
		Data: map[string]interface{}{
			"workspaceKey":  "acme/thing",
			"toolsExamined": []interface{}{"claude-code", "codex"},
			"skills": []interface{}{
				map[string]interface{}{
					"slug": "review", "name": "review", "descTokens": 10,
					"tool":   "codex",
					"source": "plugin-cache", "plugin": "superpowers",
					// Must NOT survive: the skill's own text, or where it lives.
					"body":    "SECRET-SKILL-BODY",
					"absPath": "/Users/x/.codex/plugins/cache/mkt/superpowers/6.2.0/skills/review",
				},
			},
			"plugins": []interface{}{
				map[string]interface{}{"name": "superpowers", "listingTokens": 40, "tool": "claude-code", "installPath": "/Users/x/.claude/plugins/superpowers"},
			},
			"mcpServers": []interface{}{
				map[string]interface{}{"name": "probeserver", "deferred": false, "tool": "codex", "command": "npx", "env": "SECRET-MCP-ENV"},
			},
		},
	}
	ProjectEvent(e, false)

	got, ok := e.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("projected data is not a map: %#v", e.Data)
	}
	if _, present := got["toolsExamined"]; !present {
		t.Errorf("toolsExamined was STRIPPED — the backend cannot then tell examined-and-empty from never-looked-at")
	}

	for _, field := range []string{"skills", "plugins", "mcpServers"} {
		arr, ok := got[field].([]interface{})
		if !ok || len(arr) != 1 {
			t.Fatalf("%s did not survive as a one-element array: %#v", field, got[field])
		}
		elem, ok := arr[0].(map[string]interface{})
		if !ok {
			t.Fatalf("%s element is not a map: %#v", field, arr[0])
		}
		if _, present := elem["tool"]; !present {
			t.Errorf("%s[0].tool was STRIPPED by the ELEMENT allowlist — the asset arrives with no owner and the backend folds two tools' assets into one identity", field)
		}
		// The element allowlist is the load-bearing privacy line for these arrays.
		// Adding fields must not have opened them up. `absPath` is here because a
		// skill's provenance is one careless struct tag away from being its path.
		for _, forbidden := range []string{"body", "installPath", "command", "env", "absPath"} {
			if _, present := elem[forbidden]; present {
				t.Errorf("%s[0].%s survived projection and must not", field, forbidden)
			}
		}
	}

	// Provenance rides the same element allowlist and drops the same silent way.
	// Without `source` a `plugin-cache` carry figure arrives indistinguishable
	// from a confirmed one, so an upper bound is read as a settled cost; without
	// `plugin`, twenty skills from one install read as twenty separate decisions.
	skill := got["skills"].([]interface{})[0].(map[string]interface{})
	for _, field := range []string{"source", "plugin"} {
		if _, present := skill[field]; !present {
			t.Errorf("skills[0].%s was STRIPPED by the ELEMENT allowlist", field)
		}
	}
}
