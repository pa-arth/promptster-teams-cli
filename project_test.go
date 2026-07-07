package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const leakCanary = "PROMPTSTER_SOURCE_CANARY_51f3a9"

func eventWithData(kind string, data map[string]interface{}) Event {
	e := newEvent(kind, "sess-project-test")
	e.Data = data
	e.RawPayload = "raw preview containing " + leakCanary
	return e
}

func TestProjectEventStripsSourceFields(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		data        map[string]interface{}
		wantKept    map[string]interface{}
		wantDropped []string
	}{
		{
			name: "file_diff keeps path+counts, drops diff/oldString/newString/content",
			kind: "file_diff",
			data: map[string]interface{}{
				"path": "src/app.ts", "linesAdded": 3, "linesRemoved": 1,
				"diff": leakCanary, "oldString": leakCanary, "newString": leakCanary,
				"content": leakCanary, "contentLength": 512,
			},
			wantKept:    map[string]interface{}{"path": "src/app.ts", "linesAdded": 3, "linesRemoved": 1},
			wantDropped: []string{"diff", "oldString", "newString", "content", "contentLength"},
		},
		{
			name: "file_create keeps path+size, drops content",
			kind: "file_create",
			data: map[string]interface{}{
				"path": "src/new.ts", "sizeBytes": 42, "content": leakCanary,
			},
			wantKept:    map[string]interface{}{"path": "src/new.ts", "sizeBytes": 42},
			wantDropped: []string{"content"},
		},
		{
			name: "command keeps invocation metadata, drops stdout/stderr",
			kind: "command",
			data: map[string]interface{}{
				"command": "pnpm test", "exitCode": 0,
				"stdout": leakCanary, "stderr": leakCanary,
			},
			wantKept:    map[string]interface{}{"command": "pnpm test", "exitCode": 0},
			wantDropped: []string{"stdout", "stderr"},
		},
		{
			name: "ai_response keeps usage metadata, drops text and lastAssistantMessage",
			kind: "ai_response",
			data: map[string]interface{}{
				"text": leakCanary, "lastAssistantMessage": leakCanary,
				"model": "claude-sonnet-5", "inputTokens": 1200, "outputTokens": 340,
			},
			wantKept:    map[string]interface{}{"model": "claude-sonnet-5", "inputTokens": 1200, "outputTokens": 340},
			wantDropped: []string{"text", "lastAssistantMessage"},
		},
		{
			name: "prompt keeps text (the product) + slash-command name, drops smuggled fields",
			kind: "prompt",
			data: map[string]interface{}{
				"text": "fix the failing test", "command": "commit",
				"meta": map[string]interface{}{"raw": leakCanary},
			},
			wantKept:    map[string]interface{}{"text": "fix the failing test", "command": "commit"},
			wantDropped: []string{"meta"},
		},
		{
			name: "planning drops todo bodies",
			kind: "planning",
			data: map[string]interface{}{
				"todos": []interface{}{map[string]interface{}{"content": leakCanary}},
			},
			wantKept:    map[string]interface{}{},
			wantDropped: []string{"todos"},
		},
		{
			name: "plan_decision drops plan/response previews",
			kind: "plan_decision",
			data: map[string]interface{}{
				"planPreview": leakCanary, "responsePreview": leakCanary,
			},
			wantKept:    map[string]interface{}{},
			wantDropped: []string{"planPreview", "responsePreview"},
		},
		{
			name:        "unknown kind keeps nothing",
			kind:        "future_kind",
			data:        map[string]interface{}{"anything": leakCanary},
			wantKept:    map[string]interface{}{},
			wantDropped: []string{"anything"},
		},
		{
			name: "subagent_usage keeps usage + attribution names",
			kind: "subagent_usage",
			data: map[string]interface{}{
				"model": "claude-haiku-4-5-20251001", "inputTokens": 800,
				"attributionSkill": "deep-research", "attributionAgent": "Explore",
				"agentId": "agent-abc", "transcript": leakCanary,
			},
			wantKept: map[string]interface{}{
				"model": "claude-haiku-4-5-20251001", "inputTokens": 800,
				"attributionSkill": "deep-research", "attributionAgent": "Explore", "agentId": "agent-abc",
			},
			wantDropped: []string{"transcript"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := eventWithData(tc.kind, tc.data)
			projectEvent(&e)

			if e.RawPayload != "" {
				t.Errorf("RawPayload not cleared: %q", e.RawPayload)
			}
			data, ok := e.Data.(map[string]interface{})
			if !ok {
				t.Fatalf("projected Data is %T, want map", e.Data)
			}
			for _, key := range tc.wantDropped {
				if _, present := data[key]; present {
					t.Errorf("field %q survived projection", key)
				}
			}
			for key, want := range tc.wantKept {
				if got := data[key]; got != want {
					t.Errorf("field %q = %v, want %v", key, got, want)
				}
			}
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("projected event does not marshal: %v", err)
			}
			if strings.Contains(string(b), leakCanary) {
				t.Errorf("canary survived somewhere in the projected event: %s", b)
			}
		})
	}
}

func TestProjectEventConfigCensusArrayElements(t *testing.T) {
	e := eventWithData("config_census", map[string]interface{}{
		"skillCount": 2,
		"skills": []interface{}{
			map[string]interface{}{"slug": "deep-research", "name": "Deep Research", "descTokens": 120, "body": leakCanary},
			"not-an-object",
		},
		"mcpServers": []interface{}{
			map[string]interface{}{"name": "github", "deferred": true, "config": leakCanary},
		},
	})
	projectEvent(&e)
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), leakCanary) {
		t.Fatalf("canary survived array-element projection: %s", b)
	}
	data := e.Data.(map[string]interface{})
	skills := data["skills"].([]interface{})
	if len(skills) != 1 {
		t.Fatalf("skills = %v, want 1 projected object element", skills)
	}
	if skills[0].(map[string]interface{})["slug"] != "deep-research" {
		t.Errorf("skill slug lost in projection: %v", skills[0])
	}
}

func TestProjectEventNonMapDataKeepsNothing(t *testing.T) {
	e := newEvent("command", "sess-project-test")
	e.Data = "raw string payload " + leakCanary
	projectEvent(&e)
	data, ok := e.Data.(map[string]interface{})
	if !ok || len(data) != 0 {
		t.Fatalf("non-map Data must project to empty map, got %#v", e.Data)
	}
}

func TestScrubInlineCommand(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "python -c single-quoted",
			in:   `python -c 'import os; print(os.environ)'`,
			want: `python -c '<inline-code-redacted>'`,
		},
		{
			name: "node -e double-quoted",
			in:   `node -e "console.log(require('./secrets'))"`,
			want: `node -e "<inline-code-redacted>"`,
		},
		{
			name: "node --eval",
			in:   `node --eval "process.exit(1)"`,
			want: `node --eval "<inline-code-redacted>"`,
		},
		{
			name: "bash -c ANSI-C quoting",
			in:   `bash -c $'echo hi'`,
			want: `bash -c $'<inline-code-redacted>'`,
		},
		{
			name: "no-space flag form",
			in:   `python -c'print(1)'`,
			want: `python -c'<inline-code-redacted>'`,
		},
		{
			name: "heredoc body dropped, marker + terminator kept",
			in:   "cat > config.ts <<'EOF'\nexport const KEY = \"sk-live-123\";\nEOF",
			want: "cat > config.ts <<'EOF'\n<inline-code-redacted>\nEOF",
		},
		{
			name: "unquoted heredoc tag",
			in:   "cat <<EOF\nsecret body\nEOF\necho done",
			want: "cat <<EOF\n<inline-code-redacted>\nEOF\necho done",
		},
		// Non-redacted cases: signal must survive.
		{name: "plain test command untouched", in: "npm test -- --watch=false", want: "npm test -- --watch=false"},
		{name: "git commit message untouched", in: `git commit -m "fix: races"`, want: `git commit -m "fix: races"`},
		{name: "gcc -c object compile untouched (unquoted)", in: "gcc -c foo.c -o foo.o", want: "gcc -c foo.c -o foo.o"},
		{name: "codex argv join untouched (quoting lost)", in: "bash -lc npm test", want: "bash -lc npm test"},
		{name: "here-string untouched", in: "grep foo <<<bar", want: "grep foo <<<bar"},
		{name: "unterminated heredoc left as-is", in: "cat <<EOF\nno terminator here", want: "cat <<EOF\nno terminator here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scrubInlineCommand(tc.in); got != tc.want {
				t.Errorf("scrubInlineCommand(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestProjectedEventSignsAndVerifies pins that projection happens BEFORE
// signing: the buffered/POSTed event is signed over its projected data, so the
// backend's signature check passes on exactly the bytes it receives.
func TestProjectedEventSignsAndVerifies(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	e := eventWithData("file_diff", map[string]interface{}{
		"path": "a.ts", "diff": leakCanary, "linesAdded": 1,
	})
	if err := appendEventToLocalBuffer(&e); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The mutated event (what gets POSTed) must be projected.
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), leakCanary) {
		t.Fatalf("canary survived into the signed event: %s", b)
	}
	// The buffered line (the on-disk audit chain) must be projected too.
	buffered, err := os.ReadFile(filepath.Join(tmp, "buffer.jsonl"))
	if err != nil {
		t.Fatalf("read buffer: %v", err)
	}
	if strings.Contains(string(buffered), leakCanary) {
		t.Fatalf("canary survived into the local buffer: %s", buffered)
	}
}

// TestWireBodyCarriesNoSource is the end-to-end "never sent" proof: run a
// source-bearing event through the real funnel (buffer + POST) and assert the
// HTTP body the server receives carries no source fields.
func TestWireBodyCarriesNoSource(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	session := Session{SessionID: "sess-wire-test", SessionToken: "PSE-TEST", TaskRoot: tmp}
	e := eventWithData("command", map[string]interface{}{
		"command":  `python -c 'print("` + leakCanary + `")'`,
		"exitCode": 1,
		"stdout":   "output " + leakCanary,
		"stderr":   "error " + leakCanary,
	})
	if !ingestClaudeWatchEvent(e, session, srv.Client()) {
		t.Fatal("event was not sent")
	}
	if len(received) == 0 {
		t.Fatal("server received no body")
	}
	body := string(received)
	if strings.Contains(body, leakCanary) {
		t.Fatalf("source canary reached the wire: %s", body)
	}
	for _, field := range []string{`"stdout"`, `"stderr"`, `"rawPayload"`} {
		if strings.Contains(body, field) {
			t.Errorf("field %s reached the wire: %s", field, body)
		}
	}
	var sent Event
	if err := json.Unmarshal(received, &sent); err != nil {
		t.Fatalf("wire body is not a valid event: %v", err)
	}
	data := sent.Data.(map[string]interface{})
	if data["command"] != `python -c '<inline-code-redacted>'` {
		t.Errorf("command not scrubbed on the wire: %v", data["command"])
	}
}
