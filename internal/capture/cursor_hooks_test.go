package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// sandboxHome points HOME at a throwaway dir with a ~/.cursor in it, so
// enrollment tests never touch the developer's own hooks.json. EnsureCursorHooks
// no-ops when ~/.cursor is absent, so the dir has to exist for the test to be
// testing anything.
func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func readHooksJSON(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("hooks.json is not valid JSON: %v\n%s", err, data)
	}
	return out
}

// --- enrollment --------------------------------------------------------------

// The scope is the whole reason this rail is allowed to exist. The user-level
// file is outside every repository; the project-level one is a tracked file in
// the customer's repo and enrolls per-repo. Writing the wrong one is not a
// degradation, it is the thing we told ourselves we would never do.
func TestEnsureCursorHooksWritesUserScopeOnly(t *testing.T) {
	home := sandboxHome(t)
	workspace := t.TempDir()

	changed, err := EnsureCursorHooks()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first enrollment reported no change")
	}

	raw := readHooksJSON(t, filepath.Join(home, ".cursor", "hooks.json"))
	var hooks map[string][]cursorHookCmd
	if err := json.Unmarshal(raw["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	for _, step := range cursorHookSteps {
		entries := hooks[step]
		if len(entries) != 1 || !isPromptsterCursorHook(entries[0]) {
			t.Fatalf("step %s = %#v, want exactly our entry", step, entries)
		}
		// The command must be the canonical managed path — the one self-update
		// swaps in place and npm/install.sh both target. os.Executable() is how
		// autostart ended up baking a node_modules path that the next `npm i -g`
		// deleted, after which capture silently never came back.
		if !strings.Contains(entries[0].Command, state.CanonicalInstallBin()) {
			t.Fatalf("step %s command = %q, want the canonical install path %q",
				step, entries[0].Command, state.CanonicalInstallBin())
		}
	}

	// Nothing may have been written inside a workspace.
	if _, err := os.Stat(filepath.Join(workspace, ".cursor")); !os.IsNotExist(err) {
		t.Fatal("enrollment created .cursor inside a workspace — project scope must never be written")
	}
}

// Enrollment runs on EVERY watch startup, so a second run that rewrote the file
// would churn its mtime and race other writers for no reason.
func TestEnsureCursorHooksIsIdempotent(t *testing.T) {
	sandboxHome(t)
	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}
	changed, err := EnsureCursorHooks()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second enrollment reported a change — it must be a no-op")
	}
}

// An engineer's hooks.json is theirs. We add entries to it; we do not get to
// drop the parts we do not model.
func TestEnsureCursorHooksPreservesTheEngineersOwnConfig(t *testing.T) {
	home := sandboxHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	existing := `{
	  "version": 1,
	  "someFutureKeyWeDoNotModel": {"a": 1},
	  "hooks": {
	    "beforeSubmitPrompt": [{"type": "command", "command": "/usr/local/bin/their-own-hook"}],
	    "preCompact": [{"type": "command", "command": "/usr/local/bin/another"}]
	  }
	}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}

	raw := readHooksJSON(t, path)
	if _, ok := raw["someFutureKeyWeDoNotModel"]; !ok {
		t.Fatal("an unmodelled top-level key was dropped")
	}
	var hooks map[string][]cursorHookCmd
	if err := json.Unmarshal(raw["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	if len(hooks["preCompact"]) != 1 || hooks["preCompact"][0].Command != "/usr/local/bin/another" {
		t.Fatalf("a step we do not register was disturbed: %#v", hooks["preCompact"])
	}
	var theirs, ours int
	for _, e := range hooks["beforeSubmitPrompt"] {
		if e.Command == "/usr/local/bin/their-own-hook" {
			theirs++
		}
		if isPromptsterCursorHook(e) {
			ours++
		}
	}
	if theirs != 1 || ours != 1 {
		t.Fatalf("beforeSubmitPrompt = %#v, want their hook kept alongside exactly one of ours", hooks["beforeSubmitPrompt"])
	}
}

// Overwriting a config we cannot read would destroy an engineer's setup in order
// to install telemetry. Capture degrades to transcript-only instead.
func TestEnsureCursorHooksRefusesToOverwriteUnparseableConfig(t *testing.T) {
	home := sandboxHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	garbage := []byte(`{"hooks": {  <-- an engineer mid-edit`)
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureCursorHooks()
	if err == nil {
		t.Fatal("an unparseable hooks.json must be an error, not a silent overwrite")
	}
	if changed {
		t.Fatal("reported a change against a file it must not have touched")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(garbage) {
		t.Fatalf("the file was modified:\n%s", after)
	}
}

// A machine without Cursor gets no config for a product its owner does not use.
func TestEnsureCursorHooksNoOpsWhenCursorIsNotInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	changed, err := EnsureCursorHooks()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("enrolled on a machine with no ~/.cursor")
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor")); !os.IsNotExist(err) {
		t.Fatal("created ~/.cursor for a product the engineer does not use")
	}
}

// A stale-path entry is precisely the entry that must be REPLACED. Appending
// beside it would leave Cursor invoking a missing binary AND double-emitting
// every event once the new one starts working.
func TestEnsureCursorHooksReplacesAStalePathRatherThanAppending(t *testing.T) {
	home := sandboxHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	stale := `{"version":1,"hooks":{"afterFileEdit":[{"type":"command","command":"\"/old/node_modules/.bin/promptster-teams\" cursor-hook"}]}}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}

	raw := readHooksJSON(t, path)
	var hooks map[string][]cursorHookCmd
	if err := json.Unmarshal(raw["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	if len(hooks["afterFileEdit"]) != 1 {
		t.Fatalf("afterFileEdit = %#v, want exactly one entry", hooks["afterFileEdit"])
	}
	if strings.Contains(hooks["afterFileEdit"][0].Command, "/old/node_modules") {
		t.Fatal("the stale path survived")
	}
}

// RemoveCursorHooks is what an uninstall path must call: a removed CLI that left
// its entries behind would have Cursor exec a missing binary on every event.
func TestRemoveCursorHooksLeavesEverythingElse(t *testing.T) {
	home := sandboxHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.WriteFile(path, []byte(
		`{"version":1,"hooks":{"beforeSubmitPrompt":[{"type":"command","command":"/usr/local/bin/theirs"}]}}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}

	changed, err := RemoveCursorHooks()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("removal reported no change")
	}

	raw := readHooksJSON(t, path)
	var hooks map[string][]cursorHookCmd
	if err := json.Unmarshal(raw["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	for step, entries := range hooks {
		for _, e := range entries {
			if isPromptsterCursorHook(e) {
				t.Fatalf("our entry survived removal on %s", step)
			}
		}
	}
	if len(hooks["beforeSubmitPrompt"]) != 1 || hooks["beforeSubmitPrompt"][0].Command != "/usr/local/bin/theirs" {
		t.Fatalf("the engineer's own hook did not survive: %#v", hooks["beforeSubmitPrompt"])
	}
	// A step that only ever held our entry is deleted, not left as an empty
	// array, so an uninstall gives back a file the engineer recognises.
	if _, present := hooks["afterFileEdit"]; present {
		t.Fatalf("afterFileEdit left behind as %#v after removal", hooks["afterFileEdit"])
	}
}

// --- rail handoff ------------------------------------------------------------

// MUST-DO #5: the same work must never be captured twice. Both rails watch the
// same session — the hook fires as the agent works, the watcher tails the file
// that agent is appending to.
func TestClaimedTranscriptIsSkippedByTheWatcherButAdvancedToEOF(t *testing.T) {
	root := cursorProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws := resolvePath(t.TempDir())
	session := Session{TaskRoot: ws, DeviceID: "dev-test"}
	cutoff := time.Now().Add(-time.Hour)
	processors := map[string]*normalize.CursorTranscriptProcessor{}

	// First poll establishes the startup boundary with nothing on disk, so the
	// transcript below is a NEW session the watcher would otherwise tail from 0.
	pollCursorTranscripts(session, ws, cutoff, processors, true, false)

	path := writeCursorTranscript(t, root, "p/agent-transcripts/a/a.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>the hook rail already sent this</user_query>"}]}}`,
		cursorShellLine(ws),
	)
	recordCursorHookClaim(path, "a", "grok-4.5")

	// A prompt and a shell command are both kinds the HOOK rail emits, so the
	// watcher must stay silent on them. (It is no longer silent about
	// everything — see the hook-blind kinds test below.)
	if queued := pollCursorTranscripts(session, ws, cutoff, processors, false, false); queued != 0 {
		t.Fatalf("watcher queued %d event(s) for a hook-claimed transcript — that is double capture", queued)
	}

	// Skipping without advancing would mean the moment the claim lapsed the
	// watcher replayed the whole session from byte 0.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	progress := loadCursorWatchProgress()
	if got := progress.Offsets[cursorProgressKey(path)]; got != info.Size() {
		t.Fatalf("claimed transcript offset = %d, want EOF %d", got, info.Size())
	}
}

// Claims are evicted on WRITE, and only the hook process writes. So the one case
// where a stale claim matters is the case where no write is ever coming again:
// hooks uninstalled, binary moved, Cursor dropped its config. Read-side TTL is
// what stops those transcripts being skipped forever and captured by neither
// rail.
func TestAnExpiredClaimReleasesTheTranscriptBackToTheWatcher(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	key := "p/agent-transcripts/a/a.jsonl"

	fresh := cursorHookClaims{V: cursorHookClaimsVersion, Claims: map[string]cursorHookClaim{
		key: {SessionID: "a", TsMs: time.Now().UnixMilli()},
	}}
	if !isCursorHookClaimed(fresh, key) {
		t.Fatal("a fresh claim was not honoured — both rails would emit")
	}

	stale := cursorHookClaims{V: cursorHookClaimsVersion, Claims: map[string]cursorHookClaim{
		key: {SessionID: "a", TsMs: time.Now().Add(-cursorHookClaimTTL - time.Hour).UnixMilli()},
	}}
	if isCursorHookClaimed(stale, key) {
		t.Fatal("an expired claim still held the transcript — with hooks gone it would be captured by neither rail")
	}
}

// afterAgentThought fires many times per turn and every one resolves the same
// model. Without suppression a five-thought turn queues five copies of one fact.
func TestRepeatModelIsSuppressedWithinASession(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	path := "/home/u/.cursor/projects/p/agent-transcripts/a/a.jsonl"

	if cursorHookModelAlreadyReported(path, "a", "grok-4.5") {
		t.Fatal("reported before anything was recorded")
	}
	recordCursorHookClaim(path, "a", "grok-4.5")
	if !cursorHookModelAlreadyReported(path, "a", "grok-4.5") {
		t.Fatal("the same model in the same session was not suppressed")
	}
	// A model CHANGE inside one session is a real fact and must still be emitted.
	if cursorHookModelAlreadyReported(path, "a", "claude-opus-5") {
		t.Fatal("a model switch mid-session was suppressed — that is a real event")
	}
	// A different session is a different fact.
	if cursorHookModelAlreadyReported(path, "b", "grok-4.5") {
		t.Fatal("suppressed across sessions")
	}
}

// A later claim with an empty model (afterFileEdit etc.) must not wipe the real
// model afterAgentThought already recorded — that field gates suppression.
func TestCursorHookClaimPreservesModelWhenLaterStepHasNone(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	path := "/home/u/.cursor/projects/p/agent-transcripts/a/a.jsonl"

	recordCursorHookClaim(path, "a", "grok-4.5")
	recordCursorHookClaim(path, "a", "") // file edit / shell — no model on payload
	if !cursorHookModelAlreadyReported(path, "a", "grok-4.5") {
		t.Fatal("empty-model claim wiped the remembered model")
	}
}

// The handoff used to be whole-transcript: a claimed session was seeked to EOF
// unread. But Cursor exposes no hook for an MCP call or a subagent dispatch —
// the seven steps we register carry neither — so on every hook-enrolled machine,
// which is the recommended install, those two identities were captured by
// NOTHING and the asset boards read a zero that was never a measurement.
func TestClaimedTranscriptStillYieldsTheKindsHooksCannotSee(t *testing.T) {
	root := cursorProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws := resolvePath(t.TempDir())
	session := Session{TaskRoot: ws, DeviceID: "dev-test"}
	cutoff := time.Now().Add(-time.Hour)
	processors := map[string]*normalize.CursorTranscriptProcessor{}
	pollCursorTranscripts(session, ws, cutoff, processors, true, false)

	// One of each: a kind only this rail can see, and a prompt the hook rail
	// already sent. Exactly one of them may be emitted.
	path := writeCursorTranscript(t, root, "p/agent-transcripts/b/b.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>the hook rail already sent this</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"CallMcpTool","input":{"server":"user-clerk","toolName":"list_apps"}}]}}`,
		// Also a `command`, which the hook rail sends: it both anchors the
		// transcript to this workspace and proves the filter drops it.
		cursorShellLine(ws),
	)
	recordCursorHookClaim(path, "b", "grok-4.5")

	if queued := pollCursorTranscripts(session, ws, cutoff, processors, false, false); queued != 1 {
		t.Fatalf("claimed transcript queued %d event(s), want exactly 1 (the mcp_call, not the prompt)", queued)
	}
}

// The safety property that makes the narrowing sound rather than a partial
// re-opening of the double-capture hole: every kind the watcher still emits on a
// claimed transcript must be one the HOOK normalizer structurally cannot
// produce. Widening the set without that argument is the regression.
func TestHookBlindKindsAreDisjointFromWhatTheHookRailEmits(t *testing.T) {
	// The kinds normalize_cursor_hook.go maps its seven registered steps onto.
	hookEmits := map[string]bool{
		"session_start": true, "session_end": true, "prompt": true,
		"file_diff": true, "command": true, "tool_result": true, "ai_response": true,
	}
	for kind := range cursorHookBlindKinds {
		if hookEmits[kind] {
			t.Fatalf("%q is emitted by BOTH rails on a claimed transcript — double capture", kind)
		}
	}
	if len(cursorHookBlindKinds) == 0 {
		t.Fatal("the hook-blind set is empty — the narrowing does nothing")
	}
}
