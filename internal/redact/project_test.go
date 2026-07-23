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
			name: "ai_response keeps usage metadata (incl. reasoningTokens), drops text and lastAssistantMessage",
			kind: "ai_response",
			data: map[string]interface{}{
				"text": leakCanary, "lastAssistantMessage": leakCanary,
				"model": "claude-sonnet-5", "inputTokens": 1200, "outputTokens": 340,
				"reasoningTokens": 37,
			},
			wantKept:    map[string]interface{}{"model": "claude-sonnet-5", "inputTokens": 1200, "outputTokens": 340, "reasoningTokens": 37},
			wantDropped: []string{"text", "lastAssistantMessage"},
		},
		{
			// meta stays pinned even though the emitter no longer builds one: this
			// test feeds hand-built maps, so it is emitter-independent and holds the
			// line if a meta map is ever smuggled back onto a prompt. Its cwd is the
			// canary — an absolute path is exactly what allowlisting the map whole
			// would have leaked.
			name: "prompt keeps text (the product) + command name + promptSource + workdir + repoRoot + repoHost + repoTracked, drops smuggled fields",
			kind: "prompt",
			data: map[string]interface{}{
				"text": "fix the failing test", "command": "commit", "promptSource": "system",
				// workdir is the home-collapsed session dir (kept); the raw absolute
				// cwd is the canary and must still be dropped.
				"workdir": "~/repos/foo/bar",
				// repoRoot is the canonical repo identity (git slug / opaque hash) —
				// kept, like workdir, on its own allowlisted key.
				"repoRoot": "acme/foo",
				// repoHost is the bare provider hostname the slug was parsed from.
				// Kept for the same reason and on the same terms: the normalizer
				// strips scheme/userinfo/port/path on-device, so what survives is a
				// provider name, not a URL and not a path.
				"repoHost": "github.com",
				// repoTracked is a single boolean about the filesystem — was the cwd
				// inside a git working tree at all. One bit cannot carry a path or a
				// name, so it is canary-safe by construction; it is kept because it
				// is the ONLY thing separating a real remoteless repo from a home or
				// container directory, both of which hash to the same repoRoot shape.
				"repoTracked": true,
				"meta":        map[string]interface{}{"raw": leakCanary},
				"cwd":         leakCanary,
			},
			wantKept: map[string]interface{}{
				"text": "fix the failing test", "command": "commit", "promptSource": "system",
				"workdir": "~/repos/foo/bar", "repoRoot": "acme/foo", "repoHost": "github.com",
				"repoTracked": true,
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
		{
			// repoTracked=false is the value the whole untracked-workdir change is
			// FOR, and it is the one a default-deny projector is most likely to eat:
			// a `false` that gets dropped arrives as ABSENT, which downstream reads
			// as "an older CLI that never looked" — the exact ambiguity being fixed,
			// re-created silently. The projector skips nil, not falsy; this pins it.
			name: "prompt keeps repoTracked=false (absent would read as an old CLI)",
			kind: "prompt",
			data: map[string]interface{}{
				"text": "run the migration", "workdir": "~/Projects/US",
				// A container folder: hashes to the same shape as a remoteless repo,
				// so only the boolean tells them apart.
				"repoRoot": "1e06276a18347f93", "repoTracked": false,
				"cwd": leakCanary,
			},
			wantKept: map[string]interface{}{
				"text": "run the migration", "workdir": "~/Projects/US",
				"repoRoot": "1e06276a18347f93", "repoTracked": false,
			},
			wantDropped: []string{"cwd"},
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

// TestProjectEventCommitAttributionNestedElements is the load-bearing privacy
// check for PR3: commit_attribution nests TWO array levels (files[] each with a
// lineRanges[]), and the top-level element projection is shallow. Without the
// recursive element-allowlist walk, a files[] element would carry its lineRanges
// VERBATIM — so a smuggled non-scalar (`text`) inside a nested lineRange element
// would ship. This pins that BOTH levels are clamped to their scalar keys.
func TestProjectEventCommitAttributionNestedElements(t *testing.T) {
	e := eventWithData("commit_attribution", map[string]interface{}{
		"commitSha":    "deadbeefcafe",
		"workspaceKey": "owner/name",
		// A private absolute path smuggled at the top level must not be kept
		// (only commitSha/workspaceKey/files are allowlisted).
		"absPath": "/home/alice/secret",
		"files": []interface{}{
			map[string]interface{}{
				"path": "src/app.ts",
				// A smuggled diff body at the files-element level must be stripped.
				"diff": leakCanary,
				"lineRanges": []interface{}{
					// A `text` canary NESTED inside a lineRange element is the leak
					// the recursive strip must kill.
					map[string]interface{}{"start": 10, "end": 12, "attribution": "likely_ai", "text": leakCanary},
					map[string]interface{}{"start": 30, "end": 30, "attribution": "unknown"},
				},
			},
		},
	})
	ProjectEvent(&e, false)

	b, _ := json.Marshal(e)
	if strings.Contains(string(b), leakCanary) {
		t.Fatalf("canary survived nested commit_attribution projection: %s", b)
	}
	data := e.Data.(map[string]interface{})
	if data["commitSha"] != "deadbeefcafe" || data["workspaceKey"] != "owner/name" {
		t.Errorf("commitSha/workspaceKey lost in projection: %+v", data)
	}
	if _, present := data["absPath"]; present {
		t.Errorf("non-allowlisted top-level field survived: %+v", data)
	}
	files, ok := data["files"].([]interface{})
	if !ok || len(files) != 1 {
		t.Fatalf("files did not survive projection: %T %+v", data["files"], data["files"])
	}
	file := files[0].(map[string]interface{})
	if _, present := file["diff"]; present {
		t.Errorf("smuggled diff survived files-element projection: %+v", file)
	}
	// A files element must strip to exactly {path, lineRanges}.
	if len(file) != 2 || file["path"] != "src/app.ts" {
		t.Errorf("files element not stripped to {path, lineRanges}: %+v", file)
	}
	ranges, ok := file["lineRanges"].([]interface{})
	if !ok || len(ranges) != 2 {
		t.Fatalf("nested lineRanges did not survive: %T %+v", file["lineRanges"], file["lineRanges"])
	}
	first := ranges[0].(map[string]interface{})
	if _, present := first["text"]; present {
		t.Errorf("smuggled text key survived nested lineRanges projection: %+v", first)
	}
	if len(first) != 3 || first["start"] != 10 || first["end"] != 12 || first["attribution"] != "likely_ai" {
		t.Errorf("nested lineRange not stripped to {start,end,attribution}: %+v", first)
	}
}

// TestProjectEventCommitAttributionAiTokens pins the aiTokens denominator: a
// scalar integer allowlisted on commit_attribution survives projection intact,
// while a smuggled content-bearing key on the same payload is dropped.
func TestProjectEventCommitAttributionAiTokens(t *testing.T) {
	e := eventWithData("commit_attribution", map[string]interface{}{
		"commitSha":    "deadbeefcafe",
		"workspaceKey": "owner/name",
		"aiTokens":     1234,
		// A content key smuggled alongside the scalar must not survive.
		"aiLineText": leakCanary,
		"files":      []interface{}{},
	})
	ProjectEvent(&e, false)

	b, _ := json.Marshal(e)
	if strings.Contains(string(b), leakCanary) {
		t.Fatalf("canary survived commit_attribution projection: %s", b)
	}
	data := e.Data.(map[string]interface{})
	if data["aiTokens"] != 1234 {
		t.Errorf("aiTokens lost or altered in projection: %+v", data["aiTokens"])
	}
	if _, present := data["aiLineText"]; present {
		t.Errorf("smuggled content key survived: %+v", data)
	}
}

// Privacy gate for the durability engine: a durability_verdict carries two
// range arrays (durableRanges/churnedRanges). The top-level projection is
// shallow, so without the element-allowlist walk a range element would ship
// VERBATIM — including a smuggled byte/text/fingerprint key. This pins that the
// top level keeps only its allowlisted fields AND every range element strips to
// exactly {start,end,ageDays,lineageId}. A fingerprint MUST NOT leave the device.
func TestProjectEventDurabilityVerdictNestedElements(t *testing.T) {
	e := eventWithData("durability_verdict", map[string]interface{}{
		"commitSha":    "deadbeefcafe",
		"workspaceKey": "owner/name",
		"path":         "src/app.ts",
		"measuredTsMs": 1000,
		// A content fingerprint smuggled at the top level must not be kept.
		"fingerprint": leakCanary,
		"durableRanges": []interface{}{
			// A `text` canary NESTED inside a range element is the leak the
			// recursive strip must kill.
			map[string]interface{}{"start": 3, "end": 5, "ageDays": 31, "lineageId": "abc:src/app.ts", "text": leakCanary},
		},
		"churnedRanges": []interface{}{
			map[string]interface{}{"start": 9, "end": 9, "ageDays": 4, "lineageId": "abc:src/app.ts", "content": leakCanary},
		},
	})
	ProjectEvent(&e, false)

	b, _ := json.Marshal(e)
	if strings.Contains(string(b), leakCanary) {
		t.Fatalf("canary survived durability_verdict projection: %s", b)
	}
	data := e.Data.(map[string]interface{})
	if data["commitSha"] != "deadbeefcafe" || data["workspaceKey"] != "owner/name" || data["path"] != "src/app.ts" {
		t.Errorf("allowlisted scalars lost in projection: %+v", data)
	}
	if _, present := data["fingerprint"]; present {
		t.Errorf("smuggled top-level fingerprint survived: %+v", data)
	}
	durable, ok := data["durableRanges"].([]interface{})
	if !ok || len(durable) != 1 {
		t.Fatalf("durableRanges did not survive: %T %+v", data["durableRanges"], data["durableRanges"])
	}
	first := durable[0].(map[string]interface{})
	if _, present := first["text"]; present {
		t.Errorf("smuggled text key survived durableRanges projection: %+v", first)
	}
	if len(first) != 4 || first["start"] != 3 || first["end"] != 5 || first["ageDays"] != 31 || first["lineageId"] != "abc:src/app.ts" {
		t.Errorf("durable range not stripped to {start,end,ageDays,lineageId}: %+v", first)
	}
}

// Privacy gate for the rework engine: a rework_verdict carries one range array
// (reworkedRanges). The top-level projection is shallow, so without the
// element-allowlist walk a range element would ship VERBATIM — including a
// smuggled byte/text/fingerprint key. This pins that the top level keeps only its
// allowlisted fields AND every range element strips to exactly
// {start,end,ageDays,lineageId}. A fingerprint MUST NOT leave the device.
func TestProjectEventReworkVerdictNestedElements(t *testing.T) {
	e := eventWithData("rework_verdict", map[string]interface{}{
		"commitSha":    "deadbeefcafe",
		"workspaceKey": "owner/name",
		"path":         "src/app.ts",
		"measuredTsMs": 1000,
		// A content fingerprint smuggled at the top level must not be kept.
		"fingerprint": leakCanary,
		"reworkedRanges": []interface{}{
			// A `text` canary NESTED inside a range element is the leak the
			// recursive strip must kill.
			map[string]interface{}{"start": 2, "end": 2, "ageDays": 1, "lineageId": "abc:src/app.ts", "text": leakCanary},
		},
	})
	ProjectEvent(&e, false)

	b, _ := json.Marshal(e)
	if strings.Contains(string(b), leakCanary) {
		t.Fatalf("canary survived rework_verdict projection: %s", b)
	}
	data := e.Data.(map[string]interface{})
	if data["commitSha"] != "deadbeefcafe" || data["workspaceKey"] != "owner/name" || data["path"] != "src/app.ts" {
		t.Errorf("allowlisted scalars lost in projection: %+v", data)
	}
	if _, present := data["fingerprint"]; present {
		t.Errorf("smuggled top-level fingerprint survived: %+v", data)
	}
	reworked, ok := data["reworkedRanges"].([]interface{})
	if !ok || len(reworked) != 1 {
		t.Fatalf("reworkedRanges did not survive: %T %+v", data["reworkedRanges"], data["reworkedRanges"])
	}
	first := reworked[0].(map[string]interface{})
	if _, present := first["text"]; present {
		t.Errorf("smuggled text key survived reworkedRanges projection: %+v", first)
	}
	if len(first) != 4 || first["start"] != 2 || first["end"] != 2 || first["ageDays"] != 1 || first["lineageId"] != "abc:src/app.ts" {
		t.Errorf("rework range not stripped to {start,end,ageDays,lineageId}: %+v", first)
	}
}

// ── file_read.credentialKeys — the array-of-STRING clamp ───────────────────
//
// The hole this closes: projectArrayElements handles arrays of OBJECTS and
// `continue`s past every scalar, so before the clamp an allowlisted string array
// was copied VERBATIM — the whole array trusted because its key was allowlisted.

func TestProjectEventCredentialKeysClamp(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want []interface{}
	}{
		{
			name: "identifier-shaped names survive, in order",
			in:   []interface{}{"STRIPE_SECRET_KEY", "DATABASE_URL", "_privateKey", "aws_access_key_id"},
			want: []interface{}{"STRIPE_SECRET_KEY", "DATABASE_URL", "_privateKey", "aws_access_key_id"},
		},
		{
			name: "value-shaped elements are DROPPED, not truncated",
			in: []interface{}{
				"GOOD_KEY",
				"sk_live_51H/abc+def=",     // `/`, `+`, `=` — a real key charset
				"ghp_" + leakCanary + "!!", // punctuation
				"has space",
				"9leading",
				"DASHED-KEY",
				"[REDACTED_ANTHROPIC_KEY]", // a marker is not a name
				"postgres://u:p@host/db",
			},
			want: []interface{}{"GOOD_KEY"},
		},
		{
			name: "an over-long element is dropped whole — a truncated secret is still a secret prefix",
			in:   []interface{}{"OK_KEY", strings.Repeat("A", 65)},
			want: []interface{}{"OK_KEY"},
		},
		{
			name: "non-string elements are dropped",
			in:   []interface{}{"OK_KEY", 42, nil, map[string]interface{}{"content": leakCanary}, []interface{}{leakCanary}},
			want: []interface{}{"OK_KEY"},
		},
		{
			name: "duplicates fold",
			in:   []interface{}{"A_KEY", "A_KEY", "B_KEY"},
			want: []interface{}{"A_KEY", "B_KEY"},
		},
		// A rejected harvest must leave the field ABSENT, not `[]`. An empty
		// array reads downstream as "we looked inside the file and it had no
		// keys" — a fabricated all-clear on the one surface whose job is naming
		// what to rotate. Absence reads as "no harvest", which is the truth.
		{
			name: "a non-array value cannot ride through as itself — and leaves NO field",
			in:   leakCanary,
			want: nil,
		},
		{
			name: "every element rejected leaves NO field, never an empty array",
			in:   []interface{}{"has space", "9leading", "postgres://u:p@h/db", 42, nil},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := eventWithData("file_read", map[string]interface{}{
				"path":           "acme-api/.env",
				"credentialKeys": tc.in,
			})
			ProjectEvent(&e, false)
			data := e.Data.(map[string]interface{})
			if tc.want == nil {
				if raw, present := data["credentialKeys"]; present {
					t.Fatalf("credentialKeys must be ABSENT when nothing survives, got %#v", raw)
				}
				encoded, err := json.Marshal(data)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if strings.Contains(string(encoded), "credentialKeys") {
					t.Fatalf("credentialKeys reached the wire: %s", encoded)
				}
				if strings.Contains(string(encoded), leakCanary) {
					t.Fatalf("canary survived projection: %s", encoded)
				}
				return
			}
			got, isArray := data["credentialKeys"].([]interface{})
			if !isArray {
				t.Fatalf("credentialKeys must project to an array, got %#v", data["credentialKeys"])
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %#v, want %#v", got, tc.want)
				}
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(encoded), leakCanary) {
				t.Fatalf("canary survived projection: %s", encoded)
			}
		})
	}
}

func TestProjectEventCredentialKeysCap(t *testing.T) {
	in := make([]interface{}, 0, 60)
	for i := 0; i < 60; i++ {
		in = append(in, "KEY_"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	e := eventWithData("file_read", map[string]interface{}{"path": ".env", "credentialKeys": in})
	ProjectEvent(&e, false)
	got := e.Data.(map[string]interface{})["credentialKeys"].([]interface{})
	if len(got) != 40 {
		t.Fatalf("expected the 40-item cap, got %d", len(got))
	}
}

// ABSENT ≠ EMPTY at the projection boundary too: an older CLI (or an ordinary
// read) emits no credentialKeys, and projection must NOT manufacture an empty
// array — downstream, `[]` claims "we looked inside and there were no keys".
func TestProjectEventLeavesCredentialKeysAbsent(t *testing.T) {
	e := eventWithData("file_read", map[string]interface{}{
		"path":          "src/app.ts",
		"contentLength": 512,
		"content":       leakCanary,
	})
	ProjectEvent(&e, false)
	data := e.Data.(map[string]interface{})
	if _, present := data["credentialKeys"]; present {
		t.Fatalf("projection must not invent credentialKeys, got %#v", data)
	}
	if _, present := data["content"]; present {
		t.Fatalf("file_read must never keep content, got %#v", data)
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

// livingRanges is the third range array (derivationV 2): a daily inventory of AI
// spans still tracked and undecided, so survival is measurable before the 30-day
// harvest can emit its first durable verdict. It is subject to the SAME two-level
// allowlist as durable/churned — and, being new, is the one most likely to be
// forgotten in the element walk and ship a range VERBATIM.
//
// LOCKSTEP: the backend's TEAMS_FIELD_ALLOWLIST must also name `livingRanges`
// (packages/shared/src/eventFieldProjection.ts) or it is stripped again at ingest
// and this whole array silently never arrives.
func TestProjectEventDurabilityVerdictLivingRanges(t *testing.T) {
	e := eventWithData("durability_verdict", map[string]interface{}{
		"commitSha":    "deadbeefcafe",
		"workspaceKey": "owner/name",
		"path":         "src/app.ts",
		"measuredTsMs": 1000,
		"livingRanges": []interface{}{
			map[string]interface{}{"start": 1, "end": 3, "ageDays": 4, "lineageId": "abc:src/app.ts", "text": leakCanary},
		},
	})
	ProjectEvent(&e, false)

	b, _ := json.Marshal(e)
	if strings.Contains(string(b), leakCanary) {
		t.Fatalf("canary survived livingRanges projection: %s", b)
	}
	data := e.Data.(map[string]interface{})
	living, ok := data["livingRanges"].([]interface{})
	if !ok || len(living) != 1 {
		t.Fatalf("livingRanges did not survive projection (survival would be unmeasurable): %T %+v", data["livingRanges"], data["livingRanges"])
	}
	first := living[0].(map[string]interface{})
	if len(first) != 4 || first["start"] != 1 || first["end"] != 3 || first["ageDays"] != 4 || first["lineageId"] != "abc:src/app.ts" {
		t.Errorf("living range not stripped to {start,end,ageDays,lineageId}: %+v", first)
	}
}
