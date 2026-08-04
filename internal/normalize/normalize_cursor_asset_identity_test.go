package normalize

import (
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// Cursor invocation identity: delegations and MCP calls were dropped on the
// floor, so a Cursor engineer's asset boards read as "built and used nothing".
//
// Every fixture below is the SHAPE of a real record — key-sets taken from 89
// real transcripts on a live machine (the standing rule in normalize_cursor.go:
// check a real file before assuming a field exists). The values are invented so
// the fixtures carry no operator data; the KEYS are not.

func cursorAssistantLine(toolName, input string) []byte {
	return []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"` +
		toolName + `","input":` + input + `}]}}`)
}

func TestCursorTaskEmitsDispatchWithAgentName(t *testing.T) {
	p := NewCursorTranscriptProcessor("s1")
	// Real key-set: description, model, prompt, subagent_type.
	events := p.Process(cursorAssistantLine("Task", `{
		"description":"Find the flaky test",
		"model":"composer-1",
		"prompt":"SECRET-DELEGATED-INSTRUCTION",
		"subagent_type":"generalPurpose"
	}`), 0)

	if len(events) != 1 {
		t.Fatalf("a Task delegation emitted %d events, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.Kind != "task_dispatch" {
		t.Fatalf("kind = %q, want task_dispatch", e.Kind)
	}
	d, _ := e.Data.(map[string]interface{})
	if d["name"] != "generalPurpose" {
		t.Errorf("name = %v, want generalPurpose — without it every Cursor delegation collapses into one nameless bucket", d["name"])
	}
	if d["summary"] != "Find the flaky test" {
		t.Errorf("summary = %v", d["summary"])
	}
	// The delegated instruction is free text the human may have typed.
	for k, v := range d {
		if s, ok := v.(string); ok && strings.Contains(s, "SECRET-DELEGATED-INSTRUCTION") {
			t.Errorf("the delegated prompt rode along in %q", k)
		}
	}
	if e.Source != "cursor" {
		t.Errorf("source = %q, want cursor — this is what puts cursor in source_service", e.Source)
	}
}

// subagent_type is genuinely absent on one of the 27 real Task calls in the
// corpus, so this path runs in the field and not just in theory.
func TestCursorTaskOmitsNameWhenAbsentOrBlank(t *testing.T) {
	for _, tc := range []struct{ label, input string }{
		{"absent", `{"description":"d","prompt":"p","resume":"x","run_in_background":true}`},
		{"blank", `{"description":"d","prompt":"p","subagent_type":"   "}`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			p := NewCursorTranscriptProcessor("s1")
			events := p.Process(cursorAssistantLine("Task", tc.input), 0)
			if len(events) != 1 {
				t.Fatalf("want 1 event, got %d", len(events))
			}
			d, _ := events[0].Data.(map[string]interface{})
			// ABSENT, not empty. `name: ""` opens a nameless agent-type bucket
			// on the board rather than being skipped — the exact absent-vs-empty
			// collapse the trim exists to avoid.
			if v, present := d["name"]; present {
				t.Errorf("name present as %#v; want the key ABSENT so the row is skipped, not bucketed under \"\"", v)
			}
		})
	}
}

func TestCursorMcpCallCarriesServerAndTool(t *testing.T) {
	p := NewCursorTranscriptProcessor("s1")
	// Real key-set: arguments, description, server, toolName.
	events := p.Process(cursorAssistantLine("CallMcpTool", `{
		"server":"user-supabase",
		"toolName":"execute_sql",
		"description":"run it",
		"arguments":{"query":"SECRET-SQL-BODY"}
	}`), 0)

	if len(events) != 1 {
		t.Fatalf("a CallMcpTool emitted %d events, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.Kind != "mcp_call" {
		t.Fatalf("kind = %q, want mcp_call", e.Kind)
	}
	d, _ := e.Data.(map[string]interface{})
	// One `server__tool` string, so mcpServerOf — which splits on the first
	// `__` — resolves the server for all three tools with no per-tool branch.
	if d["tool"] != "user-supabase__execute_sql" {
		t.Errorf("tool = %v, want user-supabase__execute_sql", d["tool"])
	}
	// Cursor records NO tool results anywhere. A fabricated "ok" would report
	// every failed MCP call as a success.
	if v, present := d["status"]; present {
		t.Errorf("status present as %#v; Cursor has no tool results, so any value here is invented", v)
	}
	if _, present := d["arguments"]; present {
		t.Errorf("the MCP call arguments rode along: %#v", d)
	}
}

// A half-named call would split into an empty server and open a nameless bucket
// on the MCP board — worse than dropping the row.
func TestCursorMcpCallDroppedWhenHalfNamed(t *testing.T) {
	for _, input := range []string{
		`{"toolName":"execute_sql"}`,
		`{"server":"user-supabase"}`,
		`{}`,
	} {
		p := NewCursorTranscriptProcessor("s1")
		if events := p.Process(cursorAssistantLine("CallMcpTool", input), 0); len(events) != 0 {
			t.Errorf("input %s emitted %+v; want it dropped", input, events)
		}
	}
}

// THE ONE THAT GUARDS THE SPEND BOARDS. Cursor carries no token usage at all,
// so neither new mapping may introduce a usage claim — an absent count reads
// downstream as a measured zero, and a zero-cost delegation on a spend board is
// a fabrication, not a gap.
func TestCursorInvocationsClaimNoTokenUsage(t *testing.T) {
	p := NewCursorTranscriptProcessor("s1")
	var events []interface{}
	for _, line := range [][]byte{
		cursorAssistantLine("Task", `{"description":"d","prompt":"p","subagent_type":"explore"}`),
		cursorAssistantLine("CallMcpTool", `{"server":"user-clerk","toolName":"list_snippets","arguments":{}}`),
	} {
		for _, e := range p.Process(line, 0) {
			events = append(events, e)
			if e.Kind == "subagent_usage" {
				t.Fatalf("emitted subagent_usage — that is a SPEND event and Cursor has no tokens to put in it")
			}
			d, _ := e.Data.(map[string]interface{})
			for _, k := range []string{
				"inputTokens", "outputTokens", "cacheReadTokens",
				"cacheCreationTokens", "totalTokens", "costUsd", "model",
			} {
				if v, present := d[k]; present {
					t.Errorf("%s carries %s=%#v; Cursor reports no usage, so this is synthesized", e.Kind, k, v)
				}
			}
		}
	}
	if len(events) != 2 {
		t.Fatalf("setup: want 2 events, got %d", len(events))
	}
}

// Both kinds must survive the on-device projector. A key missing from the
// default-deny allowlist is stripped with no error and no telemetry, which
// downstream is indistinguishable from an older CLI — the same failure this
// change exists to fix.
func TestCursorInvocationIdentitySurvivesProjection(t *testing.T) {
	p := NewCursorTranscriptProcessor("s1")
	lines := [][]byte{
		cursorAssistantLine("Task", `{"description":"Find the flaky test","prompt":"P","subagent_type":"generalPurpose"}`),
		cursorAssistantLine("CallMcpTool", `{"server":"user-supabase","toolName":"execute_sql","arguments":{}}`),
	}
	want := map[string]map[string]string{
		"task_dispatch": {"name": "generalPurpose", "summary": "Find the flaky test"},
		"mcp_call":      {"tool": "user-supabase__execute_sql"},
	}
	seen := map[string]bool{}
	for _, line := range lines {
		for _, e := range p.Process(line, 0) {
			ev := e
			redact.ProjectEvent(&ev, false)
			d, ok := ev.Data.(map[string]interface{})
			if !ok {
				t.Fatalf("%s projected to a non-map: %#v", ev.Kind, ev.Data)
			}
			for k, v := range want[ev.Kind] {
				if d[k] != v {
					t.Errorf("%s.%s = %#v after projection, want %q — STRIPPED fields read downstream as an older CLI", ev.Kind, k, d[k], v)
				}
			}
			seen[ev.Kind] = true
		}
	}
	for kind := range want {
		if !seen[kind] {
			t.Errorf("no %s event was produced at all", kind)
		}
	}
}
