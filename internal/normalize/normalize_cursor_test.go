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
//
// `Task` and `CallMcpTool` were removed from this list DELIBERATELY. They were
// never no-signal calls — they are the delegation and MCP identity the asset
// boards are built on, and they sat here only because their allowlist review was
// outstanding. That review is done (see toolEvent), so they now emit
// `task_dispatch` and `mcp_call`, covered by
// normalize_cursor_asset_identity_test.go. The rest of this list stands.
func TestCursorReadAndSearchToolsAreNotEmitted(t *testing.T) {
	for _, name := range []string{"Read", "Grep", "Glob", "TodoWrite", "WebSearch", "UpdateCurrentStep"} {
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

// --- CallDynamicTool: the 2026-08-22 rename, and the built-in filter ---------
//
// Fixtures below use the real key-sets and the real namespace VALUES observed in
// the 142-transcript corpus. The values matter here in a way they do not
// elsewhere in this file: the whole point of the filter is which namespace a
// call carries, so inventing a namespace would test nothing.

// The rename itself. Without this case every post-2026-08-22 Cursor MCP call is
// dropped, and a dropped mcp_call reads downstream as "used no MCP" rather than
// as a gap.
func TestCursorDynamicToolEmitsMcpCall(t *testing.T) {
	p := procWith("s1")
	events := p.Process(cursorAssistantLine("CallDynamicTool", `{
		"namespace":"user-supabase",
		"toolName":"execute_sql",
		"arguments":{"query":"SECRET-SQL-BODY"}
	}`), 0)

	if len(events) != 1 {
		t.Fatalf("a CallDynamicTool emitted %d events, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.Kind != "mcp_call" {
		t.Fatalf("kind = %q, want mcp_call", e.Kind)
	}
	d := dataOf(t, e)
	// Identical composition to the CallMcpTool path: `namespace` is the exact
	// analogue of `server`, so the two names must produce the SAME tool string.
	if d["tool"] != "user-supabase__execute_sql" {
		t.Errorf("tool = %v, want user-supabase__execute_sql", d["tool"])
	}
	if _, present := d["status"]; present {
		t.Errorf("status present; Cursor records no tool results, so any value is invented")
	}
	if _, present := d["arguments"]; present {
		t.Errorf("the call arguments rode along: %#v", d)
	}
}

// CallDynamicTool is a SUPERSET of MCP: all 5 such calls in the corpus are
// Cursor's own chat management under `cursor-app-control`, not MCP at all. A
// naive one-line rename would have put 5 of 5 non-MCP calls on the MCP board.
func TestCursorDynamicToolDropsCursorBuiltins(t *testing.T) {
	for _, input := range []string{
		`{"namespace":"cursor-app-control","toolName":"rename_chat","arguments":{}}`,
		`{"namespace":"cursor-app-control","toolName":"move_agent_to_root","arguments":{}}`,
		`{"namespace":"cursor-app-control","toolName":"move_agent_to_cloned_root","arguments":{}}`,
	} {
		p := procWith("s1")
		if events := p.Process(cursorAssistantLine("CallDynamicTool", input), 0); len(events) != 0 {
			t.Errorf("input %s emitted %+v; Cursor's own tooling is not MCP", input, events)
		}
	}
}

// THE CORRECTION. The same pollution was already shipping on the OLD name: 42 of
// 62 corpus CallMcpTool calls are Cursor built-ins (cursor-app-control 40,
// cursor-ide-browser 2). Filtering them is a deliberate behaviour CHANGE that
// drops roughly two thirds of historical Cursor mcp_call volume — the board
// becoming true, not the rail breaking.
func TestCursorMcpCallDropsCursorBuiltins(t *testing.T) {
	for _, input := range []string{
		`{"server":"cursor-app-control","toolName":"move_agent_to_cloned_root","description":"d","arguments":{}}`,
		`{"server":"cursor-ide-browser","toolName":"browser_tabs","arguments":{}}`,
	} {
		p := procWith("s1")
		if events := p.Process(cursorAssistantLine("CallMcpTool", input), 0); len(events) != 0 {
			t.Errorf("input %s emitted %+v; Cursor's own tooling is not MCP", input, events)
		}
	}
}

// The filter is a DENYLIST on `cursor-`, not an allowlist on `user-`. A server
// under some third prefix must stay on the board: failing OPEN costs a visible
// row somebody can question, where failing closed drops it silently — which is
// the exact invisible-loss defect this change exists to fix.
func TestCursorMcpCallKeepsUnknownPrefixedServers(t *testing.T) {
	for _, tc := range []struct{ toolName, input, want string }{
		{"CallMcpTool", `{"server":"user-clerk","toolName":"list_clerk_sdk_snippets","arguments":{}}`, "user-clerk__list_clerk_sdk_snippets"},
		{"CallMcpTool", `{"server":"user-Railway","toolName":"list_projects","arguments":{}}`, "user-Railway__list_projects"},
		// Neither observed family. An allowlist would silently eat both.
		{"CallMcpTool", `{"server":"org-internal","toolName":"deploy","arguments":{}}`, "org-internal__deploy"},
		{"CallDynamicTool", `{"namespace":"project-scoped","toolName":"query","arguments":{}}`, "project-scoped__query"},
	} {
		p := procWith("s1")
		events := p.Process(cursorAssistantLine(tc.toolName, tc.input), 0)
		if len(events) != 1 {
			t.Fatalf("%s %s emitted %d events, want 1", tc.toolName, tc.input, len(events))
		}
		if d := dataOf(t, events[0]); d["tool"] != tc.want {
			t.Errorf("tool = %v, want %q", d["tool"], tc.want)
		}
	}
}

// Same "half a name is worse than none" rule as the old name. An empty namespace
// would compose `__foo` and open a nameless bucket on the MCP board. Note the
// empty namespace is checked BEFORE the built-in predicate, so an empty string
// is dropped for being half a name and not for accidentally not matching
// `cursor-`.
func TestCursorDynamicToolDroppedWhenHalfNamed(t *testing.T) {
	for _, input := range []string{
		`{"toolName":"execute_sql","arguments":{}}`,
		`{"namespace":"","toolName":"execute_sql"}`,
		`{"namespace":"user-supabase","arguments":{}}`,
		`{}`,
	} {
		p := procWith("s1")
		if events := p.Process(cursorAssistantLine("CallDynamicTool", input), 0); len(events) != 0 {
			t.Errorf("input %s emitted %+v; want it dropped", input, events)
		}
	}
}

// A CallDynamicTool must NOT read `server`, and a CallMcpTool must NOT read
// `namespace`. The resolve is keyed on the tool name rather than coalescing the
// two fields, because no corpus record carries both and a coalesce would be a
// guess about a shape nobody has seen.
func TestCursorInvocationNamespaceFieldIsNotCoalesced(t *testing.T) {
	for _, tc := range []struct{ label, toolName, input string }{
		{"dynamic ignores server", "CallDynamicTool", `{"server":"user-supabase","toolName":"execute_sql"}`},
		{"mcp ignores namespace", "CallMcpTool", `{"namespace":"user-supabase","toolName":"execute_sql"}`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			p := procWith("s1")
			if events := p.Process(cursorAssistantLine(tc.toolName, tc.input), 0); len(events) != 0 {
				t.Errorf("emitted %+v; the wrong field was read", events)
			}
		})
	}
}

// ID STABILITY ACROSS THE FILTER — the property that makes a re-read idempotent.
//
// `idx` is the content-array index, taken in Process's range loop BEFORE any
// tool is mapped or dropped. So a dropped CallDynamicTool must still consume its
// index: if the filter renumbered survivors instead, every event after a
// built-in call would mint a DIFFERENT deterministic id on re-read, and the
// backend — which dedupes on that id — would store the same work twice.
//
// IDs are asserted explicitly rather than merely compared, so a change that
// alters the derivation for both records at once cannot pass this by moving them
// together.
func TestCursorDynamicToolDoesNotShiftLaterEventIDs(t *testing.T) {
	const withoutDynamic = `{"role":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Shell","input":{"command":"go test ./..."}},` +
		`{"type":"tool_use","name":"Delete","input":{"path":"/w/old.go"}}` +
		`]}}`
	// The SAME two mapped tools, with a filtered-out built-in spliced in at
	// index 1 — so the Delete moves from content index 1 to index 2.
	const withDynamic = `{"role":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Shell","input":{"command":"go test ./..."}},` +
		`{"type":"tool_use","name":"CallDynamicTool","input":{"namespace":"cursor-app-control","toolName":"rename_chat","arguments":{}}},` +
		`{"type":"tool_use","name":"Delete","input":{"path":"/w/old.go"}}` +
		`]}}`

	base := procWith("s1").Process([]byte(withoutDynamic), 512)
	if len(base) != 2 {
		t.Fatalf("setup: want 2 events, got %d: %+v", len(base), base)
	}
	spliced := procWith("s1").Process([]byte(withDynamic), 512)
	if len(spliced) != 2 {
		t.Fatalf("the built-in was not filtered: want 2 events, got %d: %+v", len(spliced), spliced)
	}

	// The Shell sits at index 0 in both, so its id is unchanged.
	wantShell := event.DeterministicUUID("s1\x1fcommand\x1f512\x1f0")
	if base[0].ID != wantShell || spliced[0].ID != wantShell {
		t.Errorf("command id: base %q, spliced %q, want %q", base[0].ID, spliced[0].ID, wantShell)
	}

	// The Delete MOVED — index 1 without the built-in, index 2 with it — and its
	// id must move with it. Equal ids here would mean idx was assigned
	// post-filter, which is the bug: re-reading the same record after a mapping
	// change would then re-mint ids for work already stored.
	wantDeleteBase := event.DeterministicUUID("s1\x1ffile_delete\x1f512\x1f1")
	wantDeleteSpliced := event.DeterministicUUID("s1\x1ffile_delete\x1f512\x1f2")
	if base[1].ID != wantDeleteBase {
		t.Errorf("file_delete id at content index 1 = %q, want %q", base[1].ID, wantDeleteBase)
	}
	if spliced[1].ID != wantDeleteSpliced {
		t.Errorf("file_delete id at content index 2 = %q, want %q — idx must be the PRE-filter content index", spliced[1].ID, wantDeleteSpliced)
	}
	if wantDeleteBase == wantDeleteSpliced {
		t.Fatal("setup is degenerate: the two indices produced the same id")
	}
}

// Both invocation names must survive the on-device projector. `mcp_call`
// allowlists {name, tool, status, agentId}; this change emits no new key, so the
// dynamic-tool path rides the same already-sanctioned shape.
func TestCursorDynamicToolSurvivesProjection(t *testing.T) {
	p := procWith("s1")
	events := p.Process(cursorAssistantLine("CallDynamicTool",
		`{"namespace":"user-promptster","toolName":"decision_policy","arguments":{}}`), 0)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]
	redact.ProjectEvent(&ev, false)
	d := dataOf(t, ev)
	if d["tool"] != "user-promptster__decision_policy" {
		t.Errorf("tool = %#v after projection, want user-promptster__decision_policy — a STRIPPED field reads downstream as an older CLI", d["tool"])
	}
}
