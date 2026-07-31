package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// Every payload in this file is a REAL Cursor record shape, taken from
// transcripts on disk (61 files, 2026-02 → 2026-07) and from one live
// `cursor-agent` run, not from documentation. The sibling hiring CLI carries a
// note about a speculative normalizer that read fields no live payload ever
// sent; the way not to repeat that is to keep the fixtures traceable to a file.

func procWith(sessionID string) *CursorTranscriptProcessor {
	return NewCursorTranscriptProcessor(sessionID)
}

func firstOfKind(evs []event.Event, kind string) (event.Event, bool) {
	for _, e := range evs {
		if e.Kind == kind {
			return e, true
		}
	}
	return event.Event{}, false
}

func dataOf(t *testing.T, e event.Event) map[string]interface{} {
	t.Helper()
	d, ok := e.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("Data is %T, not map[string]interface{}", e.Data)
	}
	return d
}

// --- prompts -----------------------------------------------------------------

func TestCursorPrompt_ExtractsUserQuery(t *testing.T) {
	// Verbatim from a live cursor-agent run on 2026-07-31.
	line := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Friday, Jul 31, 2026, 5:10 PM (UTC-5)</timestamp>\n<user_query>\nRead note.txt then reply DONE. Nothing else.\n</user_query>"}]}}`)

	p := procWith("sess-1")
	evs := p.Process(line, 0)
	e, ok := firstOfKind(evs, "prompt")
	if !ok {
		t.Fatalf("no prompt event, got %d event(s)", len(evs))
	}
	d := dataOf(t, e)
	if d["text"] != "Read note.txt then reply DONE. Nothing else." {
		t.Fatalf("text = %#v", d["text"])
	}
	if e.Source != "cursor" {
		t.Fatalf("source = %q, want cursor — this is what puts cursor into source_service", e.Source)
	}
	if e.Actor == nil || e.Actor.Type != "human" {
		t.Fatalf("actor = %#v, want human", e.Actor)
	}
	if e.Provenance == nil || e.Provenance.Attribution != "likely_human" {
		t.Fatalf("provenance = %#v", e.Provenance)
	}
}

// The <timestamp> envelope is the ONLY wall-clock signal Cursor persists, and
// only recent builds write it. Parsing it must produce the right UTC instant —
// 5:10 PM at UTC-5 is 22:10Z, not 17:10Z or 12:10Z.
func TestCursorPrompt_TimestampAnchorConvertsToUTC(t *testing.T) {
	line := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Friday, Jul 31, 2026, 5:10 PM (UTC-5)</timestamp>\n<user_query>hi</user_query>"}]}}`)

	p := procWith("sess-1")
	e, ok := firstOfKind(p.Process(line, 0), "prompt")
	if !ok {
		t.Fatal("no prompt event")
	}
	if !strings.HasPrefix(e.Ts, "2026-07-31T22:10:00") {
		t.Fatalf("ts = %q, want 2026-07-31T22:10:00Z (5:10 PM UTC-5)", e.Ts)
	}
}

// The anchor carries FORWARD to the assistant actions of the same turn. Cursor
// stamps no time on assistant records at all, so without this every action
// would land at the watcher's read time instead of the turn's.
func TestCursorTimestampAnchorCarriesToAssistantActions(t *testing.T) {
	p := procWith("sess-1")
	p.Process([]byte(`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Friday, Jul 31, 2026, 5:10 PM (UTC-5)</timestamp>\n<user_query>go</user_query>"}]}}`), 0)

	edit := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"/w/main.go","old_string":"a","new_string":"b"}}]}}`)
	e, ok := firstOfKind(p.Process(edit, 100), "file_diff")
	if !ok {
		t.Fatal("no file_diff event")
	}
	if !strings.HasPrefix(e.Ts, "2026-07-31T22:10:00") {
		t.Fatalf("assistant ts = %q, want the turn's anchor", e.Ts)
	}
}

// Older Cursor builds write <user_query> with no <timestamp>. That must still
// produce a prompt — with the read-time default — not be dropped.
func TestCursorPrompt_NoTimestampStillEmits(t *testing.T) {
	line := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nrefactor the parser\n</user_query>"}]}}`)

	p := procWith("sess-1")
	e, ok := firstOfKind(p.Process(line, 0), "prompt")
	if !ok {
		t.Fatal("no prompt event")
	}
	if dataOf(t, e)["text"] != "refactor the parser" {
		t.Fatalf("text = %#v", dataOf(t, e)["text"])
	}
	if e.Ts == "" {
		t.Fatal("ts is empty — should fall back to the read-time default")
	}
}

// THE PRIVACY CASE. Cursor rides <attached_files> — actual file contents — in
// the same user-turn text as the prompt. `prompt.text` is allowlisted on both
// sides, so shipping the whole text would push source through the one kind that
// keeps free text. Only the inside of <user_query> may survive.
func TestCursorPrompt_DropsAttachedFileContents(t *testing.T) {
	secret := "func Withdraw(acct string) { /* PROPRIETARY BODY */ }"
	raw := map[string]interface{}{
		"role": "user",
		"message": map[string]interface{}{
			"content": []interface{}{map[string]interface{}{
				"type": "text",
				"text": "<attached_files>\nsrc/bank.go\n" + secret + "\n</attached_files>\n<user_query>\nfix the withdraw bug\n</user_query>",
			}},
		},
	}
	line, _ := json.Marshal(raw)

	p := procWith("sess-1")
	e, ok := firstOfKind(p.Process(line, 0), "prompt")
	if !ok {
		t.Fatal("no prompt event")
	}
	d := dataOf(t, e)
	if d["text"] != "fix the withdraw bug" {
		t.Fatalf("text = %#v, want only the user_query", d["text"])
	}
	blob, _ := json.Marshal(e)
	if strings.Contains(string(blob), "PROPRIETARY") {
		t.Fatalf("attached file contents leaked into the event: %s", blob)
	}
}

// Fail CLOSED on an envelope we do not recognise. A user record with no
// <user_query> must emit nothing rather than ship whatever text was in it —
// a new Cursor envelope tag must degrade to silence, not to a leak.
func TestCursorPrompt_NoUserQueryEmitsNothing(t *testing.T) {
	line := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<some_future_envelope>internal source code</some_future_envelope>"}]}}`)

	p := procWith("sess-1")
	if evs := p.Process(line, 0); len(evs) != 0 {
		t.Fatalf("expected no events, got %d: %#v", len(evs), evs)
	}
}

// A subagent transcript's "user" record is the PARENT AGENT's task brief, not
// the engineer's prompt. Crediting it to the human would inflate prompt counts
// and pollute the prompt-quality signal with machine-written text.
func TestCursorSidechain_SuppressesAgentAuthoredPrompts(t *testing.T) {
	line := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>Investigate the failing test and report back</user_query>"}]}}`)

	p := procWith("parent-sess")
	p.Sidechain = true
	if evs := p.Process(line, 0); len(evs) != 0 {
		t.Fatalf("sidechain must emit no prompt, got %d: %#v", len(evs), evs)
	}
}

// A sidechain still has to ADVANCE the clock: its brief carries the only
// timestamp in the file, and dropping the record must not drop the anchor its
// own tool calls depend on.
func TestCursorSidechain_StillAnchorsTime(t *testing.T) {
	p := procWith("parent-sess")
	p.Sidechain = true
	p.Process([]byte(`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Friday, Jul 31, 2026, 5:10 PM (UTC-5)</timestamp>\n<user_query>go</user_query>"}]}}`), 0)

	edit := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"/w/a.go","old_string":"x","new_string":"y"}}]}}`)
	e, ok := firstOfKind(p.Process(edit, 50), "file_diff")
	if !ok {
		t.Fatal("no file_diff from sidechain")
	}
	if !strings.HasPrefix(e.Ts, "2026-07-31T22:10:00") {
		t.Fatalf("ts = %q, want the anchor recovered from the suppressed brief", e.Ts)
	}
}

// --- file edits: COUNTS ONLY -------------------------------------------------

// The load-bearing privacy test. StrReplace carries the code on both sides;
// only the line COUNTS may survive, and the strings must appear nowhere in the
// event — not in Data, not in RawPayload.
func TestCursorFileDiff_CountsOnlyNeverCode(t *testing.T) {
	line := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"/home/u/proj/main.go","old_string":"alpha\nbravo","new_string":"charlie","replace_all":false}}]}}`)

	p := procWith("sess-1")
	e, ok := firstOfKind(p.Process(line, 0), "file_diff")
	if !ok {
		t.Fatal("no file_diff event")
	}
	d := dataOf(t, e)
	if d["path"] != "/home/u/proj/main.go" {
		t.Fatalf("path = %#v", d["path"])
	}
	if d["linesRemoved"] != 2 {
		t.Fatalf("linesRemoved = %#v, want 2 (alpha, bravo)", d["linesRemoved"])
	}
	if d["linesAdded"] != 1 {
		t.Fatalf("linesAdded = %#v, want 1 (charlie)", d["linesAdded"])
	}
	for _, forbidden := range []string{"old_string", "new_string", "diff", "oldString", "newString"} {
		if _, present := d[forbidden]; present {
			t.Fatalf("event data carries %q — teams never emits code", forbidden)
		}
	}
	if e.RawPayload != "" {
		t.Fatalf("RawPayload is set (%q) — it can carry source and must stay empty", e.RawPayload)
	}
	blob, _ := json.Marshal(e)
	for _, forbidden := range []string{"alpha", "bravo", "charlie"} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("edit content %q leaked into the marshalled event: %s", forbidden, blob)
		}
	}
	if e.Actor == nil || e.Actor.Type != "ai" {
		t.Fatalf("actor = %#v, want ai", e.Actor)
	}
}

// Cursor's own hooks payload uses edits[]{old_string,new_string}; the
// transcript uses a single flat old_string/new_string on StrReplace. Same
// counting rule, and the empty side must count 0, not 1 — a pure insertion has
// no lines on the old side.
func TestCursorLineCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"one", 1},
		{"one\n", 1},
		{"one\ntwo", 2},
		{"one\ntwo\n", 2},
		{"\n", 1},
	}
	for _, c := range cases {
		if got := cursorLineCount(c.in); got != c.want {
			t.Fatalf("cursorLineCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCursorFileCreate_CountsAndSizeOnly(t *testing.T) {
	line := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"path":"/w/new.go","contents":"package main\nfunc main(){}"}}]}}`)

	p := procWith("sess-1")
	e, ok := firstOfKind(p.Process(line, 0), "file_create")
	if !ok {
		t.Fatal("no file_create event")
	}
	d := dataOf(t, e)
	if d["path"] != "/w/new.go" {
		t.Fatalf("path = %#v", d["path"])
	}
	if d["linesAdded"] != 2 {
		t.Fatalf("linesAdded = %#v, want 2", d["linesAdded"])
	}
	if d["sizeBytes"] != 26 {
		t.Fatalf("sizeBytes = %#v, want 26", d["sizeBytes"])
	}
	blob, _ := json.Marshal(e)
	if strings.Contains(string(blob), "package main") {
		t.Fatalf("file contents leaked: %s", blob)
	}
}

func TestCursorShell_CommandOnlyNoFabricatedExitCode(t *testing.T) {
	line := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"go test ./...","description":"run tests","working_directory":"/w"}}]}}`)

	p := procWith("sess-1")
	e, ok := firstOfKind(p.Process(line, 0), "command")
	if !ok {
		t.Fatal("no command event")
	}
	d := dataOf(t, e)
	if d["command"] != "go test ./..." {
		t.Fatalf("command = %#v", d["command"])
	}
	// Cursor's transcript records NO tool results, so there is no exit code.
	// Fabricating 0 would report every failed command as a success.
	if _, present := d["exitCode"]; present {
		t.Fatalf("exitCode present (%#v) — Cursor reports none and we must not invent one", d["exitCode"])
	}
	for _, forbidden := range []string{"stdout", "stderr", "output"} {
		if _, present := d[forbidden]; present {
			t.Fatalf("command event carries %q", forbidden)
		}
	}
}

func TestCursorDelete(t *testing.T) {
	line := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Delete","input":{"path":"/w/old.go"}}]}}`)
	p := procWith("sess-1")
	e, ok := firstOfKind(p.Process(line, 0), "file_delete")
	if !ok {
		t.Fatal("no file_delete event")
	}
	if dataOf(t, e)["path"] != "/w/old.go" {
		t.Fatalf("path = %#v", dataOf(t, e)["path"])
	}
}

// Read/Grep/Glob are 782 of the corpus's 1,809 tool calls and say only that the
// agent looked at something. They are deliberately unmapped — pinned here so
// "we started shipping 780 no-signal events" cannot happen by accident.
func TestCursorReadAndSearchToolsAreNotEmitted(t *testing.T) {
	for _, name := range []string{"Read", "Grep", "Glob", "TodoWrite", "Task", "CallMcpTool", "WebSearch", "UpdateCurrentStep"} {
		line := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"` + name + `","input":{"path":"/w/x.go","pattern":"foo"}}]}}`)
		p := procWith("sess-1")
		if evs := p.Process(line, 0); len(evs) != 0 {
			t.Fatalf("tool %s emitted %d event(s), want 0: %#v", name, len(evs), evs)
		}
	}
}

// --- envelope ----------------------------------------------------------------

// Event ids must be derived from the record's BYTE OFFSET, so a re-tail after a
// watcher restart, or a resend after a transport error, produces the SAME id
// and the backend collapses the duplicate. A counter would restart at 0
// mid-file and mint ids colliding with earlier, different records — dedupe
// would then eat real events, which is worse than duplicates.
func TestCursorEventIDsAreDeterministicPerOffset(t *testing.T) {
	line := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"/w/a.go","old_string":"a","new_string":"b"}}]}}`)

	a, _ := firstOfKind(procWith("sess-1").Process(line, 4096), "file_diff")
	b, _ := firstOfKind(procWith("sess-1").Process(line, 4096), "file_diff")
	if a.ID != b.ID {
		t.Fatalf("same record at same offset produced different ids: %s vs %s", a.ID, b.ID)
	}
	c, _ := firstOfKind(procWith("sess-1").Process(line, 8192), "file_diff")
	if a.ID == c.ID {
		t.Fatal("different offsets produced the same id — distinct records would be deduped away")
	}
	d, _ := firstOfKind(procWith("sess-2").Process(line, 4096), "file_diff")
	if a.ID == d.ID {
		t.Fatal("different sessions produced the same id")
	}
}

// Two content items in ONE record are two distinct events and must not collide
// on the record's offset alone.
func TestCursorMultipleToolUsesInOneRecordGetDistinctIDs(t *testing.T) {
	line := []byte(`{"role":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"StrReplace","input":{"path":"/w/a.go","old_string":"a","new_string":"b"}},` +
		`{"type":"tool_use","name":"StrReplace","input":{"path":"/w/b.go","old_string":"c","new_string":"d"}}]}}`)

	evs := procWith("sess-1").Process(line, 0)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if evs[0].ID == evs[1].ID {
		t.Fatal("two edits in one record share an id — one would be deduped away")
	}
}

func TestCursorPromptCarriesRepoIdentity(t *testing.T) {
	p := procWith("sess-1")
	p.Workdir = "~/repos/proj"
	p.RepoRoot = "acme/proj"
	p.RepoHost = "github.com"
	p.RepoTracked = true

	line := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>go</user_query>"}]}}`)
	e, _ := firstOfKind(p.Process(line, 0), "prompt")
	d := dataOf(t, e)
	if d["workdir"] != "~/repos/proj" || d["repoRoot"] != "acme/proj" || d["repoHost"] != "github.com" {
		t.Fatalf("repo identity not stamped: %#v", d)
	}
	if d["repoTracked"] != true {
		t.Fatalf("repoTracked = %#v, want true", d["repoTracked"])
	}
}

// repoTracked must be stamped EXPLICITLY false, never omitted, whenever
// repoRoot is present — otherwise "not a git repo" is indistinguishable from
// "a CLI too old to have looked", which is the ambiguity the field removes.
func TestCursorPromptStampsRepoTrackedFalseExplicitly(t *testing.T) {
	p := procWith("sess-1")
	p.RepoRoot = "sha256:abc123"
	p.RepoTracked = false

	line := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>go</user_query>"}]}}`)
	e, _ := firstOfKind(p.Process(line, 0), "prompt")
	d := dataOf(t, e)
	v, present := d["repoTracked"]
	if !present {
		t.Fatal("repoTracked omitted while repoRoot is set")
	}
	if v != false {
		t.Fatalf("repoTracked = %#v, want false", v)
	}
}

func TestCursorMalformedLineEmitsNothing(t *testing.T) {
	p := procWith("sess-1")
	for _, line := range [][]byte{
		[]byte(`not json`),
		[]byte(`{}`),
		[]byte(`{"role":"assistant"}`),
		[]byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":"not an object"}]}}`),
		[]byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"old_string":"a","new_string":"b"}}]}}`),
	} {
		if evs := p.Process(line, 0); len(evs) != 0 {
			t.Fatalf("line %q emitted %d event(s), want 0", line, len(evs))
		}
	}
}

// --- workspace classification ------------------------------------------------

func TestCursorObservedPath(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			"shell working_directory",
			`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"ls","working_directory":"/Users/u/repo"}}]}}`,
			"/Users/u/repo",
		},
		{
			"read path",
			`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/Users/u/repo/main.go"}}]}}`,
			"/Users/u/repo/main.go",
		},
		{
			"glob target_directory",
			`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Glob","input":{"glob_pattern":"**/*.go","target_directory":"/Users/u/repo"}}]}}`,
			"/Users/u/repo",
		},
		{
			"windows drive path",
			`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"C:\\Users\\u\\repo\\main.go"}}]}}`,
			`C:\Users\u\repo\main.go`,
		},
		{
			// A relative path tells us nothing about which workspace this is.
			"relative path is not a signal",
			`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"main.go"}}]}}`,
			"",
		},
		{
			"user turn reveals nothing",
			`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>hi</user_query>"}]}}`,
			"",
		},
		{"malformed", `not json`, ""},
	}
	for _, c := range cases {
		if got := CursorObservedPath([]byte(c.line)); got != c.want {
			t.Fatalf("%s: CursorObservedPath = %q, want %q", c.name, got, c.want)
		}
	}
}

// The Windows-drive check must be HOST-INDEPENDENT: a Windows transcript has to
// classify identically on a unix CI runner, which filepath.IsAbs would not do.
func TestCursorHasWindowsDrive(t *testing.T) {
	for _, p := range []string{`C:\x`, `c:/x`, `Z:\`} {
		if !cursorHasWindowsDrive(p) {
			t.Fatalf("%q should be a windows drive path", p)
		}
	}
	for _, p := range []string{"", "C", "C:", "/usr", "1:\\x", "main.go"} {
		if cursorHasWindowsDrive(p) {
			t.Fatalf("%q should NOT be a windows drive path", p)
		}
	}
}

// --- end-to-end through the real privacy boundary ----------------------------

// The normalizer is only half the guarantee; redact.ProjectEvent is the
// default-deny boundary every event crosses before it is signed or queued. This
// drives a Cursor event through it and asserts what survives — so a future
// field added here without an allowlist entry shows up as a failing test rather
// than as silently-dropped data that reads like "an older CLI".
func TestCursorEventsSurviveProjectionWithExactlyTheAllowlistedFields(t *testing.T) {
	p := procWith("sess-1")
	p.Workdir = "~/repos/proj"
	p.RepoRoot = "acme/proj"
	p.RepoHost = "github.com"
	p.RepoTracked = true

	promptLine := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>fix the bug</user_query>"}]}}`)
	editLine := []byte(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"/w/a.go","old_string":"secretcode","new_string":"newsecret"}}]}}`)

	prompt, _ := firstOfKind(p.Process(promptLine, 0), "prompt")
	edit, _ := firstOfKind(p.Process(editLine, 100), "file_diff")

	for _, tc := range []struct {
		ev   event.Event
		want []string
	}{
		{prompt, []string{"text", "workdir", "repoRoot", "repoHost", "repoTracked"}},
		{edit, []string{"path", "linesAdded", "linesRemoved"}},
	} {
		ev := tc.ev
		redact.ProjectEvent(&ev, false)
		d, ok := ev.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("%s: Data is %T after projection", ev.Kind, ev.Data)
		}
		if len(d) != len(tc.want) {
			t.Fatalf("%s: projected to %d field(s) %#v, want exactly %v", ev.Kind, len(d), d, tc.want)
		}
		for _, k := range tc.want {
			if _, present := d[k]; !present {
				t.Fatalf("%s: field %q did not survive projection — it is missing from the CLI allowlist", ev.Kind, k)
			}
		}
		blob, _ := json.Marshal(ev)
		if strings.Contains(string(blob), "secret") {
			t.Fatalf("%s: edit content survived projection: %s", ev.Kind, blob)
		}
	}
}

// Secrets are scrubbed pre-parse by the watcher, exactly as the Codex watcher
// does. Proven here at the seam the watcher actually uses so the ordering
// (redact THEN parse) cannot regress unnoticed.
func TestCursorRedactionRunsBeforeParse(t *testing.T) {
	line := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>deploy with sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA</user_query>"}]}}`)

	e, ok := firstOfKind(procWith("sess-1").Process(redact.RedactBytes(line), 0), "prompt")
	if !ok {
		t.Fatal("no prompt event")
	}
	if strings.Contains(dataOf(t, e)["text"].(string), "sk-ant-api03-AAAA") {
		t.Fatalf("secret survived pre-parse redaction: %#v", dataOf(t, e)["text"])
	}
}
