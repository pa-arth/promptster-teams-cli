package redact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

const leakCanary = "PROMPTSTER_SOURCE_CANARY_51f3a9"

func eventWithData(kind string, data map[string]interface{}) event.Event {
	e := event.NewEvent(kind, "sess-project-test")
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
			// meta stays pinned even though the emitter no longer builds one: this
			// test feeds hand-built maps, so it is emitter-independent and holds the
			// line if a meta map is ever smuggled back onto a prompt. Its cwd is the
			// canary — an absolute path is exactly what allowlisting the map whole
			// would have leaked.
			name: "prompt keeps text (the product) + command name + promptSource + workdir, drops smuggled fields",
			kind: "prompt",
			data: map[string]interface{}{
				"text": "fix the failing test", "command": "commit", "promptSource": "system",
				// workdir is the home-collapsed session dir (kept); the raw absolute
				// cwd is the canary and must still be dropped.
				"workdir": "~/repos/foo/bar",
				"meta":    map[string]interface{}{"raw": leakCanary},
				"cwd":     leakCanary,
			},
			wantKept: map[string]interface{}{
				"text": "fix the failing test", "command": "commit", "promptSource": "system",
				"workdir": "~/repos/foo/bar",
			},
			wantDropped: []string{"meta", "cwd"},
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
			// The TaskCreate/TaskUpdate shape (post-rename). `title` carries the
			// task subject and `status` the resolved transition; the task
			// description is a prose BODY and must never survive.
			name: "planning keeps task title+status, drops description",
			kind: "planning",
			data: map[string]interface{}{
				"title": "Inspect & land rubric branch (Phase 0b)", "status": "in_progress",
				"description": leakCanary, "activeForm": leakCanary,
			},
			wantKept: map[string]interface{}{
				"title": "Inspect & land rubric branch (Phase 0b)", "status": "in_progress",
			},
			wantDropped: []string{"description", "activeForm"},
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
		{
			// Source-exclusion defense-in-depth: even if a future/buggy emitter put
			// a cutToolInput (a command body / file path) on an interrupt, the
			// default-deny projector must strip it. Safe metadata survives.
			name: "interrupt keeps subtype/cutTool/variant, drops any cutToolInput",
			kind: "interrupt",
			data: map[string]interface{}{
				"subtype": "action", "cutTool": "Bash", "variant": "generation",
				"cutToolInput": leakCanary,
			},
			wantKept:    map[string]interface{}{"subtype": "action", "cutTool": "Bash", "variant": "generation"},
			wantDropped: []string{"cutToolInput"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := eventWithData(tc.kind, tc.data)
			ProjectEvent(&e, false)

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
		// Capture-health counts must survive the default-deny projection.
		"claudeTranscriptsTotal":    7,
		"claudeTranscriptsActive7d": 3,
	})
	ProjectEvent(&e, false)
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
	// The two integer capture-health counts must pass through projection
	// (they're on the config_census allowlist), unchanged.
	if data["claudeTranscriptsTotal"] != 7 {
		t.Errorf("claudeTranscriptsTotal lost in projection: %v", data["claudeTranscriptsTotal"])
	}
	if data["claudeTranscriptsActive7d"] != 3 {
		t.Errorf("claudeTranscriptsActive7d lost in projection: %v", data["claudeTranscriptsActive7d"])
	}
}

func TestProjectEventFileDiffLineRanges(t *testing.T) {
	// lineRanges survives projection, but each element must be stripped to
	// exactly {start,end,attribution} — a smuggled `text`/content key on an
	// element (a code leak) must not survive.
	e := eventWithData("file_diff", map[string]interface{}{
		"path": "src/app.ts", "linesAdded": 5, "linesRemoved": 2,
		"diff": leakCanary,
		"lineRanges": []interface{}{
			map[string]interface{}{"start": 10, "end": 11, "attribution": "likely_ai", "text": leakCanary},
			map[string]interface{}{"start": 31, "end": 33, "attribution": "likely_ai"},
		},
	})
	ProjectEvent(&e, false)

	b, _ := json.Marshal(e)
	if strings.Contains(string(b), leakCanary) {
		t.Fatalf("canary survived lineRanges projection: %s", b)
	}
	data := e.Data.(map[string]interface{})
	ranges, ok := data["lineRanges"].([]interface{})
	if !ok {
		t.Fatalf("lineRanges did not survive projection: %T %+v", data["lineRanges"], data["lineRanges"])
	}
	if len(ranges) != 2 {
		t.Fatalf("lineRanges len = %d, want 2", len(ranges))
	}
	first := ranges[0].(map[string]interface{})
	if _, present := first["text"]; present {
		t.Errorf("smuggled text key survived element projection: %+v", first)
	}
	if len(first) != 3 || first["start"] != 10 || first["end"] != 11 || first["attribution"] != "likely_ai" {
		t.Errorf("element not stripped to {start,end,attribution}: %+v", first)
	}
}

func TestProjectEventLineRangeScalarClamp(t *testing.T) {
	// The element allowlist limits key NAMES, not value TYPES. A map/slice
	// smuggled into a scalar key (start/end/attribution) must be dropped, not
	// copied verbatim — otherwise source could ride through a "scalar" field.
	e := eventWithData("file_diff", map[string]interface{}{
		"path": "src/app.ts", "linesAdded": 1, "linesRemoved": 0,
		"lineRanges": []interface{}{
			map[string]interface{}{
				"start":       map[string]interface{}{"leak": "secret"},
				"end":         11,
				"attribution": "likely_ai",
			},
		},
	})
	ProjectEvent(&e, false)

	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "secret") {
		t.Fatalf("non-scalar smuggled into start survived projection: %s", b)
	}
	data := e.Data.(map[string]interface{})
	ranges := data["lineRanges"].([]interface{})
	first := ranges[0].(map[string]interface{})
	if _, present := first["start"]; present {
		t.Errorf("non-scalar start should be dropped, got: %+v", first)
	}
	if first["end"] != 11 || first["attribution"] != "likely_ai" {
		t.Errorf("scalar siblings should survive: %+v", first)
	}
}

func TestProjectEventNonMapDataKeepsNothing(t *testing.T) {
	e := event.NewEvent("command", "sess-project-test")
	e.Data = "raw string payload " + leakCanary
	ProjectEvent(&e, false)
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
			name: "escaped double quotes inside body mask fully, not partially",
			in:   `python -c "print(\"secret-body\")" && echo ok`,
			want: `python -c "<inline-code-redacted>" && echo ok`,
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

// proseCanary / proseMarker mirror the constants in the backend's
// scrubAssistantProse.test.ts LOCKSTEP table. proseCanary is a fake leak-detection
// sentinel, not a credential — the trailing directive keeps gitleaks from flagging it.
const (
	proseCanary = "PROMPTSTER_LEAK_CANARY_prose_4c1" //gitleaks:allow
	proseMarker = "<code-redacted>"
)

// TestScrubAssistantProse pins the on-device scrubber to the SAME input->output
// table the backend runs (packages/shared/src/eventFieldProjection.ts). These
// two tables must stay byte-for-byte identical — a divergence means the
// on-device scrub and the backend's re-scrub disagree.
func TestScrubAssistantProse(t *testing.T) {
	j := func(lines ...string) string { return strings.Join(lines, "\n") }

	cases := []struct {
		name, in, want string
	}{
		{
			name: "plain prose untouched",
			in:   "I'll use the frontend-design skill, then clear context before continuing.",
			want: "I'll use the frontend-design skill, then clear context before continuing.",
		},
		{
			name: "fenced block collapses to marker",
			in:   j("I'll edit `auth.ts` then run:", "```ts", `const x = "`+proseCanary+`"`, "```", "Done."),
			want: j("I'll edit `auth.ts` then run:", "```ts", proseMarker, "```", "Done."),
		},
		{
			name: "tilde fence collapses",
			in:   j("~~~python", `print("`+proseCanary+`")`, "~~~"),
			want: j("~~~python", proseMarker, "~~~"),
		},
		{
			name: "unterminated fence collapses",
			in:   j("text", "```js", `evil("`+proseCanary+`")`),
			want: j("text", "```js", proseMarker),
		},
		{
			name: "short inline refs kept",
			in:   "Use `useState` and edit `src/hooks/useAuth.ts` now.",
			want: "Use `useState` and edit `src/hooks/useAuth.ts` now.",
		},
		{
			name: "over-long inline span redacted",
			in:   "Set `const apiKey = process.env." + proseCanary + "_LONG_ENV_NAME` in config.",
			want: "Set `" + proseMarker + "` in config.",
		},
		{
			name: "unfenced diff run collapses",
			in: j("Here's the patch:", "diff --git a/x.ts b/x.ts", "@@ -1,2 +1,2 @@",
				"-const a = 1", `+const a = "`+proseCanary+`"`, "Looks good."),
			want: j("Here's the patch:", proseMarker, "Looks good."),
		},
		{
			name: "markdown bullets survive (no anchor)",
			in:   j("Steps:", "- first", "- second", "+ bonus", "Done."),
			want: j("Steps:", "- first", "- second", "+ bonus", "Done."),
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubAssistantProse(tc.in)
			if got != tc.want {
				t.Errorf("scrubAssistantProse(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, proseCanary) {
				t.Errorf("secret survived scrub: %q", got)
			}
		})
	}
}

// TestProjectEventAssistantProseGate pins the org-policy gate on ai_response
// text: dropped when off (default), kept-but-code-scrubbed when on.
func TestProjectEventAssistantProseGate(t *testing.T) {
	proseText := j2(
		"I'll edit `auth.ts` then run:",
		"```ts",
		`const k = "`+proseCanary+`"`,
		"```",
		"Done.",
	)

	newAIResponse := func() event.Event {
		return eventWithData("ai_response", map[string]interface{}{
			"text":                 proseText,
			"lastAssistantMessage": proseText,
			"model":                "claude-sonnet-5",
			"inputTokens":          1200,
			"outputTokens":         340,
		})
	}

	t.Run("policy off drops text, keeps usage", func(t *testing.T) {
		e := newAIResponse()
		ProjectEvent(&e, false)
		data := e.Data.(map[string]interface{})
		if _, present := data["text"]; present {
			t.Errorf("text survived with policy off")
		}
		if _, present := data["lastAssistantMessage"]; present {
			t.Errorf("lastAssistantMessage survived with policy off")
		}
		if data["model"] != "claude-sonnet-5" {
			t.Errorf("usage model lost: %v", data["model"])
		}
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), proseCanary) {
			t.Errorf("secret survived with policy off: %s", b)
		}
	})

	t.Run("policy on keeps scrubbed text", func(t *testing.T) {
		e := newAIResponse()
		ProjectEvent(&e, true)
		data := e.Data.(map[string]interface{})
		text, ok := data["text"].(string)
		if !ok {
			t.Fatalf("text dropped with policy on")
		}
		if strings.Contains(text, proseCanary) {
			t.Errorf("secret survived the on-device scrub: %q", text)
		}
		if !strings.Contains(text, "auth.ts") {
			t.Errorf("short inline ref was lost: %q", text)
		}
		if !strings.Contains(text, proseMarker) {
			t.Errorf("code block was not replaced with marker: %q", text)
		}
		// lastAssistantMessage stays dropped even when prose is kept.
		if _, present := data["lastAssistantMessage"]; present {
			t.Errorf("lastAssistantMessage survived (should stay dropped)")
		}
		if data["model"] != "claude-sonnet-5" {
			t.Errorf("usage model lost: %v", data["model"])
		}
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), proseCanary) {
			t.Errorf("secret survived somewhere in the event: %s", b)
		}
	})
}

// j2 joins lines with '\n' (helper local to the prose-gate test).
func j2(lines ...string) string { return strings.Join(lines, "\n") }
