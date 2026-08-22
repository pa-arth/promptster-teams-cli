package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHooks drops a hooks.json into the sandbox home and returns its path.
func writeHooks(t *testing.T, home, body string) string {
	t.Helper()
	p := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readHooksRaw(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// THE REGRESSION. This is the production defect, reduced.
//
// A neighbour tool wrote `{"command": "…"}` — legal Cursor, `type` omitted,
// defaults to "command". Our enrollment round-tripped the file and wrote
// `"type": ""` back, which Cursor rejects, and it rejects the WHOLE file for it:
// 17 hooks across four tools went silent for two days on the machine where this
// was found, with no signal but a log nobody reads.
//
// Assert on the BYTES, not on a decoded struct. The struct is what lied: it
// decoded the absent type to "" perfectly happily, and the damage only existed
// in what got serialised back out.
func TestEnrollmentNeverManufacturesEmptyType(t *testing.T) {
	home := sandboxHome(t)
	path := writeHooks(t, home, `{
	  "version": 1,
	  "hooks": {
	    "sessionStart": [{"command": "bash '/Users/x/.cursor/herdr-agent-state.sh' session"}]
	  }
	}`)

	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}

	got := readHooksRaw(t, path)
	if strings.Contains(got, `"type": ""`) {
		t.Fatalf("we wrote an empty type — Cursor rejects the entire file for this:\n%s", got)
	}
	// And the neighbour's entry must still be there, still type-less.
	cfg, err := loadCursorHookConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var neighbour *cursorHookCmd
	for i, e := range cfg.Hooks["sessionStart"] {
		if strings.Contains(e.Command, "herdr-agent-state.sh") {
			neighbour = &cfg.Hooks["sessionStart"][i]
		}
	}
	if neighbour == nil {
		t.Fatalf("the neighbour's entry is gone:\n%s", got)
	}
	if neighbour.has("type") {
		t.Errorf("we added a type key to an entry that did not have one: %s", got)
	}
	if defects, _ := cursorHookConfigDefects(cfg); len(defects) > 0 {
		t.Errorf("we wrote a config Cursor would reject: %v", defects)
	}
}

// The second way the old struct destroyed a neighbour's config: it modelled four
// keys and marshalled exactly those, so every other key Cursor defines was
// deleted on write. `matcher` decides which files their hook fires on and
// `failClosed` decides whether its failure blocks their agent — this is their
// tool behaving differently, with nothing in their file to explain it.
func TestEnrollmentPreservesForeignHookFields(t *testing.T) {
	home := sandboxHome(t)
	path := writeHooks(t, home, `{
	  "version": 1,
	  "hooks": {
	    "afterFileEdit": [{
	      "type": "command",
	      "command": "/bin/theirs",
	      "timeout": 30,
	      "matcher": "\\.go$",
	      "loop_limit": 3,
	      "failClosed": true,
	      "somethingCursorAddedLater": {"nested": ["value"]}
	    }]
	  }
	}`)

	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Hooks map[string][]map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readHooksRaw(t, path)), &out); err != nil {
		t.Fatal(err)
	}
	var theirs map[string]json.RawMessage
	for _, e := range out.Hooks["afterFileEdit"] {
		if strings.Contains(string(e["command"]), "/bin/theirs") {
			theirs = e
		}
	}
	if theirs == nil {
		t.Fatal("the neighbour's entry did not survive enrollment")
	}
	for _, key := range []string{"timeout", "matcher", "loop_limit", "failClosed", "somethingCursorAddedLater"} {
		if _, ok := theirs[key]; !ok {
			t.Errorf("we deleted %q from a hook entry we do not own; entry is now %v", key, theirs)
		}
	}
}

// Damage already on disk. The fleet has been writing `"type": ""` for as long as
// the struct looked like that, so fixing the writer fixes nothing already broken.
func TestEnrollmentRepairsAnEmptyTypeAlreadyOnDisk(t *testing.T) {
	home := sandboxHome(t)
	path := writeHooks(t, home, `{
	  "version": 1,
	  "hooks": {
	    "sessionStart": [{"type": "", "command": "bash '/Users/x/.cursor/herdr-agent-state.sh' session"}]
	  }
	}`)

	changed, repairs, err := ensureCursorHooks()
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(repairs) != 1 {
		t.Fatalf("changed=%v repairs=%v, want one repair", changed, repairs)
	}
	if repairs[0].Step != "sessionStart" {
		t.Errorf("repair names step %q, want sessionStart", repairs[0].Step)
	}

	cfg, err := loadCursorHookConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if defects, _ := cursorHookConfigDefects(cfg); len(defects) > 0 {
		t.Fatalf("still rejected after repair: %v", defects)
	}
	// The repair is a KEY DELETION, not an invented value: Cursor documents a
	// type-less entry as "command", so removing the key restores the vendor's own
	// default instead of us deciding what a neighbour's hook is.
	got := readHooksRaw(t, path)
	if strings.Contains(got, `"type": ""`) {
		t.Errorf("empty type survived the repair:\n%s", got)
	}
	for _, e := range cfg.Hooks["sessionStart"] {
		if strings.Contains(e.Command, "herdr") && e.has("type") {
			t.Errorf("repair invented a type instead of removing the key: %s", got)
		}
	}

	// A repair must leave a trace: it fixes the file, and afterwards nothing on
	// disk admits it happened. That silence is what this defect was made of.
	if l := loadCursorHookRepairLog(); len(l.Repairs) != 1 {
		t.Errorf("repair log = %+v, want one entry", l)
	}
	if _, err := os.Stat(cursorHookBackupPath()); err != nil {
		t.Errorf("no pre-repair copy of a file we edited and do not own: %v", err)
	}
}

// The gate. saveCursorHookConfig refuses to persist a config Cursor would throw
// away, and leaves the file untouched when it refuses.
//
// A read-side check would not have caught the original defect: the file we READ
// was valid, and our own serialisation is what broke it. The assertion has to
// sit on the bytes about to land.
func TestSaveRefusesAConfigCursorWouldReject(t *testing.T) {
	home := sandboxHome(t)
	// `command` is a number: provably invalid, and NOT something we may guess at
	// on a neighbour's behalf, so the repair leaves it and the write is refused.
	body := `{"version":1,"hooks":{"sessionStart":[{"type":"command","command":42}]}}`
	path := writeHooks(t, home, body)

	_, err := EnsureCursorHooks()
	if err == nil {
		t.Fatal("enrolled into a file Cursor is rejecting, and reported success")
	}
	if !strings.Contains(err.Error(), "refusing to write") {
		t.Errorf("error = %v, want a refusal naming the reason", err)
	}
	if got := readHooksRaw(t, path); got != body {
		t.Errorf("we modified a file we refused to write:\ngot  %s\nwant %s", got, body)
	}
}

// The staleness guard, and the reason this validator is one-directional.
//
// Cursor 3.12.17 rejects any type outside {command, prompt}. We deliberately do
// NOT say so: the set of hook types is the vendor's to grow, and a hardcoded
// list would start condemning working entries the day Cursor ships a third kind.
// Unknown is a third verdict, not a synonym for invalid — being wrong this way
// only makes us quiet, while being wrong the other way has us "repairing" a hook
// that works.
func TestUnknownHookTypeIsUnknownNotInvalid(t *testing.T) {
	var entry cursorHookCmd
	if err := json.Unmarshal([]byte(`{"type":"agent","agent":"reviewer"}`), &entry); err != nil {
		t.Fatal(err)
	}
	verdict, reason := validateCursorHookEntry(entry)
	if verdict != cursorEntryUnknown {
		t.Fatalf("verdict = %v (%s), want unknown", verdict, reason)
	}

	// And an unknown entry must never be repaired, deleted, or block a write.
	cfg := cursorHookConfig{Version: 1, Hooks: map[string][]cursorHookCmd{"sessionStart": {entry}}}
	if defects, unknown := cursorHookConfigDefects(cfg); len(defects) != 0 || unknown != 1 {
		t.Fatalf("defects=%v unknown=%d, want none and one", defects, unknown)
	}
	if r := repairCursorHookConfig(&cfg); len(r) != 0 {
		t.Errorf("repaired an entry we cannot judge: %v", r)
	}
}

func TestValidateCursorHookEntry(t *testing.T) {
	cases := []struct {
		name string
		json string
		want cursorHookVerdict
	}{
		{"type omitted defaults to command", `{"command":"/bin/x"}`, cursorEntryValid},
		{"explicit command", `{"type":"command","command":"/bin/x"}`, cursorEntryValid},
		{"empty command string is legal to Cursor", `{"command":""}`, cursorEntryValid},
		{"empty type", `{"type":"","command":"/bin/x"}`, cursorEntryInvalid},
		{"whitespace type", `{"type":" ","command":"/bin/x"}`, cursorEntryInvalid},
		{"non-string type", `{"type":7,"command":"/bin/x"}`, cursorEntryInvalid},
		{"command hook with no command", `{"type":"command"}`, cursorEntryInvalid},
		{"non-string command", `{"command":42}`, cursorEntryInvalid},
		{"prompt hook", `{"type":"prompt","prompt":"review this"}`, cursorEntryValid},
		{"prompt hook with empty prompt", `{"type":"prompt","prompt":"  "}`, cursorEntryInvalid},
		{"prompt hook with no prompt", `{"type":"prompt"}`, cursorEntryInvalid},
		{"prompt hook with empty model", `{"type":"prompt","prompt":"x","model":""}`, cursorEntryInvalid},
		{"good optional fields", `{"command":"/bin/x","timeout":30,"loop_limit":3,"failClosed":true,"matcher":"*"}`, cursorEntryValid},
		{"null loop_limit means no limit", `{"command":"/bin/x","loop_limit":null}`, cursorEntryValid},
		{"string timeout", `{"command":"/bin/x","timeout":"30"}`, cursorEntryInvalid},
		{"zero timeout", `{"command":"/bin/x","timeout":0}`, cursorEntryInvalid},
		{"fractional loop_limit", `{"command":"/bin/x","loop_limit":1.5}`, cursorEntryInvalid},
		{"non-bool failClosed", `{"command":"/bin/x","failClosed":"yes"}`, cursorEntryInvalid},
		// A matcher is checked for stringness ONLY. Cursor compiles it as a
		// JavaScript RegExp; Go's RE2 rejects lookahead and backreferences, so
		// compiling it here would condemn matchers Cursor loads happily.
		{"matcher Go regexp cannot compile", `{"command":"/bin/x","matcher":"(?=foo)bar"}`, cursorEntryValid},
		{"entry is not an object", `"just a string"`, cursorEntryInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e cursorHookCmd
			if err := json.Unmarshal([]byte(tc.json), &e); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, reason := validateCursorHookEntry(e)
			if got != tc.want {
				t.Fatalf("verdict = %v (%q), want %v", got, reason, tc.want)
			}
		})
	}
}

// Cursor rejects the file over its `version` exactly as harshly as over an
// entry, so a foreign write that drops or corrupts it takes every hook down.
func TestVersionIsRepaired(t *testing.T) {
	home := sandboxHome(t)
	path := writeHooks(t, home, `{"hooks":{"sessionStart":[{"command":"/bin/theirs"}]}}`)

	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCursorHookConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version < 1 {
		t.Fatalf("version = %d, want a positive integer", cfg.Version)
	}
	if defects, _ := cursorHookConfigDefects(cfg); len(defects) > 0 {
		t.Fatalf("still rejected: %v", defects)
	}
}

// A hand-written entry pointing at our binary with no `type` is OURS. Failing to
// recognise it appends a second copy beside it and double-emits every event on
// that step.
func TestTypelessEntryPointingAtUsIsRecognisedAsOurs(t *testing.T) {
	var e cursorHookCmd
	if err := json.Unmarshal([]byte(`{"command":"\"/x/.promptster-teams/bin/promptster-teams\" cursor-hook"}`), &e); err != nil {
		t.Fatal(err)
	}
	if !isPromptsterCursorHook(e) {
		t.Fatal("did not recognise our own binary in a type-less entry")
	}
}

// Doctor must lead with the whole-file verdict. While Cursor is rejecting the
// file, "enrolled for all 8 steps" is a false green: none of those 8 run.
func TestDoctorReportsWholeFileRejection(t *testing.T) {
	home := sandboxHome(t)
	writeHooks(t, home, `{"version":1,"hooks":{"sessionStart":[{"type":"command","command":42}]}}`)
	_, _ = EnsureCursorHooks() // refused; the file stays broken

	lines := CursorHooksDoctor()
	if len(lines) == 0 || !lines[0].Err {
		t.Fatalf("first doctor line = %+v, want an error about the rejected file", lines)
	}
	if !strings.Contains(lines[0].Text, "rejecting ALL") {
		t.Errorf("doctor text = %q, want it to say the whole file is off", lines[0].Text)
	}
	// Other tools' hooks are down too, and the engineer should hear that from us.
	if !strings.Contains(lines[0].Text, "other tools") {
		t.Errorf("doctor text = %q, want the blast radius named", lines[0].Text)
	}
}

func TestDoctorReportsARepairAfterTheFact(t *testing.T) {
	home := sandboxHome(t)
	writeHooks(t, home, `{"version":1,"hooks":{"sessionStart":[{"type":"","command":"/bin/theirs"}]}}`)
	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}

	var found string
	for _, l := range CursorHooksDoctor() {
		if strings.Contains(l.Text, "repaired") {
			found = l.Text
		}
	}
	if found == "" {
		t.Fatal("doctor said nothing about the neighbour entry we rewrote")
	}
	if !strings.Contains(found, "original kept at") {
		t.Errorf("repair line = %q, want it to name the untouched copy", found)
	}
}

// A repair record must not cost the machine its health verdict.
//
// The repair note is informational, and the tail of CursorHooksDoctor gates the
// healthy lines on "did I find a problem?" — asked, at the time, as "is the list
// non-empty?". Appending the note to that same list silently satisfied the gate,
// so every machine that had ever been repaired lost BOTH its enrollment verdict
// and its usage-coverage numbers from then on. Permanently: the repair record is
// permanent. Two questions, one condition; caught by review on PR #173.
func TestARepairRecordDoesNotSuppressTheHealthVerdict(t *testing.T) {
	home := sandboxHome(t)
	// A neighbour entry we will repair, so the record exists...
	writeHooks(t, home, `{"version":1,"hooks":{"sessionStart":[{"type":"","command":"/bin/theirs"}]}}`)
	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}
	// ...and an otherwise perfectly healthy machine.
	bin := cursorHookCommandBinary(cursorHookCommand())
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o700); err != nil { // #nosec G306 -- test fixture.
		t.Fatal(err)
	}

	lines := CursorHooksDoctor()
	var repaired, enrolled bool
	for _, l := range lines {
		if l.Err || l.Warn {
			t.Errorf("healthy machine reported a problem: %+v", l)
		}
		if strings.Contains(l.Text, "repaired") {
			repaired = true
		}
		if strings.Contains(l.Text, "enrolled for all") {
			enrolled = true
		}
	}
	if !repaired {
		t.Error("the repair note is gone")
	}
	if !enrolled {
		t.Errorf("the enrollment verdict was suppressed by the repair note; got %+v", lines)
	}
}
