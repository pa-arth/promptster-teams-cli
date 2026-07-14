package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
)

// These tests cover the seam the per-half unit tests left open.
//
// census_test.go asserts on the struct buildConfigCensus returns; project_test.go
// asserts on hand-built maps fed to ProjectEvent. Both passed while production
// shipped an empty payload, because nothing exercised the actual emit path:
// build the event, then run it through the real redact/sign funnel and look at
// what is left. That composition is what these tests pin — build via the real
// buildXEvent, funnel via the real sign.AppendEventToLocalBuffer (which calls
// redact.ProjectEvent in place, exactly as emitConfigCensus/emitPresenceEvent
// do), then assert the payload is still there.

// fakeTeamKey is a syntactically-valid but entirely fake team key. It is auth
// (an ingest header) and must never ride in an event body — the presence test
// below asserts exactly that. Not a credential.
const fakeTeamKey = "PSE-ABCD-2345" // gitleaks:allow

// sandboxEmitEnv points state, the buffer, and the Claude config root at a temp
// dir so an emit test neither reads the developer's real config nor writes to
// their real state dir.
func sandboxEmitEnv(t *testing.T) (tmp, claudeDir, ws string) {
	t.Helper()
	tmp = t.TempDir()
	claudeDir = filepath.Join(tmp, "claude")
	ws = filepath.Join(tmp, "ws")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	// defaultCensusEnv resolves ~/.claude.json via os.UserHomeDir ($HOME on
	// unix, %USERPROFILE% on Windows) — pin both so the test is hermetic on CI.
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	return tmp, claudeDir, ws
}

func writeEmitFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// projectedData returns e.Data as the map the wire carries, failing the test if
// it is not a map (the projector's post-condition).
func projectedData(t *testing.T, data interface{}) map[string]interface{} {
	t.Helper()
	m, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("projected Data is %T, want map[string]interface{}", data)
	}
	return m
}

// numField reads a numeric payload field. Values arrive as float64: the payload
// is a JSON round-trip of the census struct, so every integer is a JSON number.
func numField(t *testing.T, data map[string]interface{}, key string) float64 {
	t.Helper()
	v, present := data[key]
	if !present {
		t.Fatalf("field %q missing from projected payload (keys: %v)", key, keysOf(data))
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("field %q = %#v (%T), want a number", key, v, v)
	}
	return n
}

func keysOf(data map[string]interface{}) []string {
	out := make([]string, 0, len(data))
	for k := range data {
		out = append(out, k)
	}
	return out
}

// TestConfigCensusEventSurvivesEmitPath is the regression guard for the bug
// where buildConfigCensusEvent assigned the census STRUCT to Data: a struct in
// an interface{} never asserts to map[string]interface{}, so ProjectEvent's
// default-deny replaced the whole census with {} before signing and before the
// wire. Every production config_census landed with skills:[], mcpServers:[] and
// all token counts 0. Pre-fix this test fails on the first assertion.
func TestConfigCensusEventSurvivesEmitPath(t *testing.T) {
	tmp, claudeDir, ws := sandboxEmitEnv(t)

	// Global CLAUDE.md: 400 chars → 100 tokens.
	writeEmitFixture(t, filepath.Join(claudeDir, "CLAUDE.md"), strings.Repeat("g", 400))
	// Workspace CLAUDE.md: 80 chars → 20 tokens.
	writeEmitFixture(t, filepath.Join(ws, "CLAUDE.md"), strings.Repeat("w", 80))
	// One skill: 40-char description → 10 tokens. Body must never survive.
	writeEmitFixture(t, filepath.Join(claudeDir, "skills", "deploy-check", "SKILL.md"),
		"---\nname: deploy-check\ndescription: "+strings.Repeat("d", 40)+"\n---\n\nSECRET-SKILL-BODY\n")
	// One MCP server in the global registry.
	writeEmitFixture(t, filepath.Join(tmp, ".claude.json"),
		`{"mcpServers":{"posthog":{"type":"http","url":"https://x"}}}`)

	ev := buildConfigCensusEvent(Session{SessionID: "dev-census-emit", TaskRoot: ws})

	// The REAL funnel: mutates ev in place (project → scrub → sign), and
	// emitConfigCensus ships this same ev afterwards.
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		t.Fatalf("append census to buffer: %v", err)
	}
	data := projectedData(t, ev.Data)

	if got := numField(t, data, "skillCount"); got != 1 {
		t.Errorf("skillCount = %v, want 1 — the census payload was stripped on the emit path", got)
	}
	if got := numField(t, data, "skillListingTokens"); got != 10 {
		t.Errorf("skillListingTokens = %v, want 10", got)
	}
	if got := numField(t, data, "globalClaudeMdTokens"); got != 100 {
		t.Errorf("globalClaudeMdTokens = %v, want 100", got)
	}
	if got := numField(t, data, "projectClaudeMdTokens"); got != 20 {
		t.Errorf("projectClaudeMdTokens = %v, want 20", got)
	}

	// Array-of-object fields must survive AND keep their element allowlist. This
	// is a second reason Data must be a JSON round-trip: projectArrayElements
	// walks []interface{} of map[string]interface{}, so a []censusSkill would
	// project to [] even if the top-level assertion had somehow passed.
	skills, ok := data["skills"].([]interface{})
	if !ok || len(skills) != 1 {
		t.Fatalf("skills = %#v, want 1 element", data["skills"])
	}
	skill := projectedData(t, skills[0])
	if skill["slug"] != "deploy-check" || skill["name"] != "deploy-check" {
		t.Errorf("skill identity lost in projection: %#v", skill)
	}
	if servers, ok := data["mcpServers"].([]interface{}); !ok || len(servers) != 1 {
		t.Errorf("mcpServers = %#v, want 1 element", data["mcpServers"])
	}

	// workspaceKey is load-bearing for backend workspace de-dupe and was absent
	// from the config_census allowlist, so it was stripped independently of the
	// struct bug.
	key, _ := data["workspaceKey"].(string)
	if key == "" {
		t.Error("workspaceKey missing from projected payload — it must be allowlisted for config_census")
	}
	if strings.Contains(key, string(os.PathSeparator)) && strings.Contains(key, tmp) {
		t.Errorf("workspaceKey leaks a filesystem path: %q", key)
	}

	// The privacy rule still holds on the real emit path: counts and names only.
	full, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal census event: %v", err)
	}
	if strings.Contains(string(full), "SECRET-SKILL-BODY") {
		t.Errorf("skill body survived the emit path: %s", full)
	}
}

// TestPresenceEventSurvivesEmitPath is the same regression guard for presence,
// which had the identical struct-to-Data bug. Its symptom was fleet-wide:
// engineer_keys.last_heartbeat_at and latest_cli_version stayed NULL for every
// engineer, because the backend only stamps them when the presence payload
// actually carries a version. Pre-fix this test fails on cliVersion.
func TestPresenceEventSurvivesEmitPath(t *testing.T) {
	sandboxEmitEnv(t)

	ev := buildPresenceEvent(Session{SessionID: "dev-presence-emit", SessionToken: fakeTeamKey})
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		t.Fatalf("append presence to buffer: %v", err)
	}
	data := projectedData(t, ev.Data)

	if got, _ := data["cliVersion"].(string); got == "" {
		t.Errorf("cliVersion = %#v, want the build version — presence payload was stripped on the emit path", data["cliVersion"])
	}
	if got, _ := data["device"].(string); got != "dev-presence-emit" {
		t.Errorf("device = %#v, want dev-presence-emit", data["device"])
	}
	if got, _ := data["os"].(string); got == "" {
		t.Errorf("os = %#v, want runtime.GOOS", data["os"])
	}
	if got, _ := data["arch"].(string); got == "" {
		t.Errorf("arch = %#v, want runtime.GOARCH", data["arch"])
	}
	// watching is allowlisted and must survive as an array (it may legitimately
	// be empty on a machine with no tool dirs — the sandboxed HOME here).
	if _, ok := data["watching"].([]interface{}); !ok {
		t.Errorf("watching = %#v, want an array", data["watching"])
	}
	// The team key is auth (a header) and must never ride in the body.
	full, _ := json.Marshal(ev)
	if strings.Contains(string(full), fakeTeamKey) {
		t.Errorf("presence envelope leaks the team key: %s", full)
	}
}
