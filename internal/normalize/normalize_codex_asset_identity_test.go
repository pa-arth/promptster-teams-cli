package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// Asset IDENTITY on the Codex rail: WHICH subagent, WHICH MCP server, WHICH
// skill — not merely that one of each ran.
//
// Every shape asserted here was captured live from codex-cli 0.146.0, and every
// assertion in this file was verified to FAIL against the normalizer that
// predates it. That matters more than usual: the bug being fixed is three
// signals that were emitted with the right SHAPE and no NAME, so a test that
// only checks "an event came out" passes against the broken code.
//
// Measured on a live customer before this landed: 0 of 24,380 tool_use events
// carried `skill`, 0 of 731 subagent_usage events carried `attributionAgent`,
// and mcp_call was 0 outright.

func findEvents(events []event.Event, kind string) []event.Event {
	var out []event.Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// ── subagent name ──────────────────────────────────────────────────────────

func TestCodexSubagentCarriesItsName(t *testing.T) {
	events := runCodexRollout(t, codexSubThreadID, codexSubagentLines)

	usage := findEvents(events, "subagent_usage")
	if len(usage) != 1 {
		t.Fatalf("want 1 subagent_usage, got %d", len(usage))
	}
	got := codexData(usage[0])["attributionAgent"]
	if got != "guardian" {
		t.Fatalf("attributionAgent = %v, want %q — the name is in session_meta.source.subagent and without it the backend names every delegated span %q", got, "guardian", "subagent")
	}
}

func TestCodexSubagentNameOpenSet(t *testing.T) {
	// The inner key is a tagged-union variant. `other` is the fallback Codex uses
	// for a user-defined agent, so named variants exist that this build has never
	// seen; an unrecognized one must yield its NAME, not nothing.
	cases := map[string]string{
		`{"subagent":{"other":"guardian"}}`:          "guardian",
		`{"subagent":{"reviewer":"security-audit"}}`: "security-audit",
		`{"subagent":{"unheard_of":"future-agent"}}`: "future-agent",
		// Shapes that name nobody must yield "" so the field is omitted rather
		// than defaulted — "we don't know which agent" and "an agent called
		// subagent" are opposite claims.
		`"cli"`:                       "",
		`"exec"`:                      "",
		`{"subagent":{}}`:             "",
		`{"subagent":{"other":""}}`:   "",
		`{"subagent":{"other":null}}`: "",
		`{"subagent":"guardian"}`:     "",
		`{"other":{"other":"nope"}}`:  "",
	}
	for source, want := range cases {
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(`{"source":`+source+`}`), &payload); err != nil {
			t.Fatalf("bad fixture %s: %v", source, err)
		}
		if got := codexSubagentName(payload); got != want {
			t.Errorf("codexSubagentName(%s) = %q, want %q", source, got, want)
		}
	}
}

func TestCodexOrdinaryThreadNamesNoAgent(t *testing.T) {
	events := runCodexRollout(t, codexParentThreadID, codexParentLines)
	if got := findEvents(events, "subagent_usage"); len(got) != 0 {
		t.Fatalf("a `source:\"cli\"` thread produced %d subagent_usage events, want 0", len(got))
	}
	for _, e := range events {
		if _, ok := codexData(e)["attributionAgent"]; ok {
			t.Fatalf("%s carried attributionAgent on a non-delegated thread", e.Kind)
		}
	}
}

// ── MCP identity ───────────────────────────────────────────────────────────

// Captured verbatim from a live 0.146.0 run against a local stdio MCP server.
// The arguments and the result are REAL user data in production: whatever the
// engineer passed the tool, and the tool's full output.
const codexMCPLine = `{"timestamp":"2026-08-04T14:59:54.089Z","type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"exec-6150ea86-8029-405d-bd8b-b18655c2a220","invocation":{"server":"probeserver","tool":"probe_echo","arguments":{"message":"SECRET-ARGUMENT"}},"duration":{"secs":0,"nanos":5212667},"result":{"Ok":{"content":[{"type":"text","text":"SECRET-OUTPUT"}]}}}}`

func TestCodexMCPCallCarriesServerAndTool(t *testing.T) {
	events := runCodexRollout(t, codexParentThreadID, []string{codexParentMeta, codexMCPLine})

	calls := findEvents(events, "mcp_call")
	if len(calls) != 1 {
		t.Fatalf("want 1 mcp_call, got %d — mcp_tool_call_end is the ONLY place MCP identity appears on the Codex wire", len(calls))
	}
	d := codexData(calls[0])
	// "<server>__<tool>": mcp_call's allowlist is {name,tool,status} on BOTH
	// default-deny rails, so the server has to ride inside the name. The backend's
	// mcpServerOf() splits on the first "__" and recovers `probeserver`.
	if d["tool"] != "probeserver__probe_echo" {
		t.Fatalf("tool = %v, want %q", d["tool"], "probeserver__probe_echo")
	}
	if d["status"] != "ok" {
		t.Fatalf("status = %v, want %q", d["status"], "ok")
	}
}

func TestCodexMCPCallEmitsNoArgumentsOrOutput(t *testing.T) {
	events := runCodexRollout(t, codexParentThreadID, []string{codexParentMeta, codexMCPLine})

	// Serialize the WHOLE event, not just Data: RawPayload is a field too, and it
	// is exactly where a "just keep the raw line" habit would put the output.
	for _, e := range findEvents(events, "mcp_call") {
		blob, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, forbidden := range []string{"SECRET-ARGUMENT", "SECRET-OUTPUT", "arguments", "content"} {
			if strings.Contains(string(blob), forbidden) {
				t.Fatalf("mcp_call leaked %q: %s", forbidden, blob)
			}
		}
	}
}

func TestCodexMCPCallFailureIsStillRecorded(t *testing.T) {
	errLine := `{"timestamp":"2026-08-04T14:59:55.000Z","type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"c2","invocation":{"server":"probeserver","tool":"probe_echo","arguments":{}},"result":{"Err":"boom"}}}`
	events := runCodexRollout(t, codexParentThreadID, []string{codexParentMeta, errLine})

	calls := findEvents(events, "mcp_call")
	if len(calls) != 1 {
		t.Fatalf("a failed MCP call must still be recorded; got %d events", len(calls))
	}
	if got := codexData(calls[0])["status"]; got != "error" {
		t.Fatalf("status = %v, want %q", got, "error")
	}
}

func TestCodexMCPCallRequiresBothNames(t *testing.T) {
	// A half-named invocation yields no event rather than a row keyed on "__x".
	for _, invocation := range []string{`{"server":"","tool":"t"}`, `{"server":"s"}`, `{}`} {
		line := `{"timestamp":"2026-08-04T14:59:56.000Z","type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"c3","invocation":` + invocation + `,"result":{"Ok":{}}}}`
		events := runCodexRollout(t, codexParentThreadID, []string{codexParentMeta, line})
		if got := findEvents(events, "mcp_call"); len(got) != 0 {
			t.Fatalf("invocation %s produced %d mcp_call events, want 0", invocation, len(got))
		}
	}
}

// ── skill identity ─────────────────────────────────────────────────────────

func TestCodexSkillSlugFromCommand(t *testing.T) {
	cases := map[string]string{
		// The real invocation shape, captured live.
		`sed -n '1,240p' /Users/x/.codex/skills/promptster-probe/SKILL.md`: "promptster-probe",
		// Bundled skills sit one level deeper and DO count — the engineer reached
		// for them just the same.
		`sed -n '1,240p' /Users/x/.codex/skills/.system/openai-docs/SKILL.md`: "openai-docs",
		// Other read verbs are the same act.
		`cat ~/.codex/skills/refactor/SKILL.md`:             "refactor",
		`head -50 /w/.cursor/skills-cursor/review/SKILL.md`: "review",
		// NOT a read: naming the path to a destructive or staging program is not
		// reaching for the skill.
		`rm /Users/x/.codex/skills/old/SKILL.md`:       "",
		`git add /Users/x/.codex/skills/old/SKILL.md`:  "",
		`mv /a/skills/x/SKILL.md /b/skills/y/SKILL.md`: "",
		// Not a skill file at all.
		`cat /Users/x/.codex/skills/README.md`:  "",
		`cat /Users/x/notes/SKILL.md`:           "",
		`cat /Users/x/.codex/skills/./SKILL.md`: "",
		``:                                      "",
	}
	for cmd, want := range cases {
		if got := codexSkillSlugFromCommand(cmd); got != want {
			t.Errorf("codexSkillSlugFromCommand(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// The exec wrapper as Codex actually writes it — a JS program calling
// tools.exec_command, which unwrapCodexExec lifts to a bare cmd string.
const codexSkillExecLines = `{"timestamp":"2026-08-04T09:49:20.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_K1","input":"const r = await tools.exec_command({cmd:\"sed -n '1,240p' /Users/x/.codex/skills/promptster-probe/SKILL.md\",\"workdir\":\"/tmp/ws\",\"yield_time_ms\":10000});\ntext(r.output);\n"}}`

func TestCodexSkillReadEmitsIdentityAlongsideCommand(t *testing.T) {
	events := runCodexRollout(t, codexParentThreadID, []string{
		codexParentMeta,
		codexSkillExecLines,
		`{"timestamp":"2026-08-04T09:49:21.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_K1","output":"---\nname: promptster-probe\n---\n"}}`,
	})

	// The command event is emitted UNCHANGED. Other consumers count it, and this
	// change must not quietly remove rows from someone else's denominator.
	if got := findEvents(events, "command"); len(got) != 1 {
		t.Fatalf("want the command event preserved, got %d", len(got))
	}

	tools := findEvents(events, "tool_use")
	if len(tools) != 1 {
		t.Fatalf("want 1 sibling tool_use, got %d", len(tools))
	}
	d := codexData(tools[0])
	if d["skill"] != "promptster-probe" {
		t.Fatalf("skill = %v, want %q — this is the field costSpans reads to build modelInvokedSkills", d["skill"], "promptster-probe")
	}
	if d["tool"] != "skill" {
		t.Fatalf("tool = %v, want %q", d["tool"], "skill")
	}
}

func TestCodexSkillEventIDNeverCollidesWithItsCommand(t *testing.T) {
	events := runCodexRollout(t, codexParentThreadID, []string{
		codexParentMeta,
		codexSkillExecLines,
		`{"timestamp":"2026-08-04T09:49:21.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_K1","output":"ok"}}`,
	})
	cmd := findEvents(events, "command")
	skill := findEvents(events, "tool_use")
	if len(cmd) != 1 || len(skill) != 1 {
		t.Fatalf("setup: got %d command / %d tool_use", len(cmd), len(skill))
	}
	// Both derive from the same call_id. Sharing a seed would make one silently
	// replace the other at the dedup boundary.
	if cmd[0].ID == skill[0].ID {
		t.Fatalf("command and skill events share id %q", cmd[0].ID)
	}
}

func TestCodexOrdinaryCommandEmitsNoSkill(t *testing.T) {
	events := runCodexRollout(t, codexParentThreadID, []string{
		codexParentMeta,
		`{"timestamp":"2026-08-04T09:49:20.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_K2","input":"const r = await tools.exec_command({cmd:\"go test ./...\",\"workdir\":\"/tmp/ws\"});\ntext(r.output);\n"}}`,
		`{"timestamp":"2026-08-04T09:49:21.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_K2","output":"ok"}}`,
	})
	if got := findEvents(events, "tool_use"); len(got) != 0 {
		t.Fatalf("a plain command produced %d skill events, want 0", len(got))
	}
}
