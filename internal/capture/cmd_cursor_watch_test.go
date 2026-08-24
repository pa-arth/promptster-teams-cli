package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// cursorProjectsRoot points the watcher at a temp Cursor home and returns the
// projects root, so tests can build realistic transcript paths without touching
// the developer's real ~/.cursor.
func cursorProjectsRoot(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PROMPTSTER_CURSOR_HOME", home)
	root := filepath.Join(home, "projects")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// writeCursorTranscript creates a transcript at the given projects-relative
// path with the given lines.
func writeCursorTranscript(t *testing.T, root, rel string, lines ...string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func cursorShellLine(dir string) string {
	return `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"ls","working_directory":"` + dir + `"}}]}}`
}

// --- path recognition --------------------------------------------------------

// The projects tree also holds canvases, MCP state, terminals and agent-tool
// scratch files. Only the three agent-transcript layouts may be tailed —
// verified against real transcripts on disk and cross-checked against an
// independent implementation (kenn-io/agentsview cursor_paths.go), which is
// where the flat `agent-transcripts/<uuid>.jsonl` shape came from.
func TestIsCursorTranscriptPath(t *testing.T) {
	base := "/home/u/.cursor/projects/Users-u-repo"
	cases := []struct {
		path string
		want bool
	}{
		{base + "/agent-transcripts/abc.jsonl", true},
		{base + "/agent-transcripts/abc/abc.jsonl", true},
		{base + "/agent-transcripts/abc/subagents/def.jsonl", true},
		// Not transcripts.
		{base + "/canvases/node_modules/x.jsonl", false},
		{base + "/agent-tools/7a651070.jsonl", false},
		{base + "/mcps/user-supabase/log.jsonl", false},
		{base + "/agent-transcripts/abc/def/ghi/jkl.jsonl", false},
		{base + "/agent-transcripts/abc/notsubagents/def.jsonl", false},
		{base + "/agent-transcripts.jsonl", false},
	}
	for _, c := range cases {
		if got := isCursorTranscriptPath(c.path); got != c.want {
			t.Fatalf("isCursorTranscriptPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// A subagent's events must roll up to the session that SPAWNED it. Taking the
// child's own uuid would fragment every delegated task into a phantom session —
// the failure that once split one engineer's hour of Codex work into seven
// sessions.
func TestCursorSessionIDFromPath(t *testing.T) {
	base := "/home/u/.cursor/projects/Users-u-repo/agent-transcripts"
	cases := []struct{ path, want string }{
		{base + "/e74dfa4f-f66b-44fe-b10f-39ecccca2fa6.jsonl", "e74dfa4f-f66b-44fe-b10f-39ecccca2fa6"},
		{base + "/e74dfa4f/e74dfa4f.jsonl", "e74dfa4f"},
		{base + "/9a2fe3d8/subagents/6497de01.jsonl", "9a2fe3d8"},
	}
	for _, c := range cases {
		if got := cursorSessionIDFromPath(c.path); got != c.want {
			t.Fatalf("cursorSessionIDFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
	if !isCursorSidechainFile(base + "/9a2fe3d8/subagents/6497de01.jsonl") {
		t.Fatal("subagents/ file not detected as a sidechain")
	}
	if isCursorSidechainFile(base + "/9a2fe3d8/9a2fe3d8.jsonl") {
		t.Fatal("main transcript misdetected as a sidechain")
	}
}

func TestCursorProgressKeyIsProjectsRelativeAndSlashed(t *testing.T) {
	root := cursorProjectsRoot(t)
	p := filepath.Join(root, "Users-u-repo", "agent-transcripts", "abc", "abc.jsonl")
	got := cursorProgressKey(p)
	want := "Users-u-repo/agent-transcripts/abc/abc.jsonl"
	if got != want {
		t.Fatalf("cursorProgressKey = %q, want %q", got, want)
	}
	if filepath.IsAbs(got) || strings.Contains(got, root) {
		t.Fatalf("progress key leaks the absolute path: %q", got)
	}
}

// --- classification ----------------------------------------------------------

// Cursor records no cwd, so the workspace decision comes from absolute paths the
// agent actually touched. The transcript's DIRECTORY NAME is deliberately not
// used: two munging behaviours are observable on one machine (a full-length name
// and a stem truncated to 43 chars with a 7-hex suffix), and the truncated form
// is not reversible at all.
func TestCursorClassifyUsesObservedPathsNotDirectoryName(t *testing.T) {
	root := cursorProjectsRoot(t)
	ws := resolvePath(t.TempDir())
	outside := resolvePath(t.TempDir())

	// The project directory name is deliberately WRONG for this workspace (it is
	// the truncated-and-hashed form). Classification must still succeed on the
	// path the Shell call reveals.
	inside := writeCursorTranscript(t, root,
		"private-tmp-claude-502-Users-paarthjamdagne-e4e727c/agent-transcripts/a/a.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>go</user_query>"}]}}`,
		cursorShellLine(ws),
	)
	if got, matched := cursorClassify(inside, []string{ws}); got != cursorMatchYes {
		t.Fatalf("in-workspace transcript classified %v (root %q), want yes", got, matched)
	} else if matched != ws {
		t.Fatalf("matched root = %q, want %q", matched, ws)
	}

	other := writeCursorTranscript(t, root, "Users-u-other/agent-transcripts/b/b.jsonl",
		cursorShellLine(outside),
	)
	if got, _ := cursorClassify(other, []string{ws}); got != cursorMatchNo {
		t.Fatalf("out-of-workspace transcript classified %v, want no", got)
	}
}

// A transcript that has not revealed a path yet (a turn with no tool call) must
// stay UNDECIDED and be retried, never cached as a mismatch. Caching "no" on a
// still-growing file is the bug that silently dropped whole Codex sessions —
// the poll loop's `case "no": continue` never re-evaluates it.
func TestCursorClassifyUndecidedWhenNoPathYet(t *testing.T) {
	root := cursorProjectsRoot(t)
	ws := resolvePath(t.TempDir())

	proseOnly := writeCursorTranscript(t, root, "p/agent-transcripts/c/c.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>explain generics</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"Generics let you..."}]}}`,
	)
	if got, _ := cursorClassify(proseOnly, []string{ws}); got != cursorMatchUndecided {
		t.Fatalf("prose-only transcript classified %v, want undecided", got)
	}

	empty := writeCursorTranscript(t, root, "p/agent-transcripts/d/d.jsonl")
	if got, _ := cursorClassify(empty, []string{ws}); got != cursorMatchUndecided {
		t.Fatalf("empty transcript classified %v, want undecided", got)
	}

	missing := filepath.Join(root, "p", "agent-transcripts", "gone", "gone.jsonl")
	if got, _ := cursorClassify(missing, []string{ws}); got != cursorMatchUndecided {
		t.Fatalf("missing transcript classified %v, want undecided", got)
	}
}

// A worktree's cwd sits OUTSIDE the workspace directory, and its sessions belong
// to this capture session. workspaceMatchRoots supplies them; classification must
// honour every root, not just the first.
func TestCursorClassifyMatchesAnyRoot(t *testing.T) {
	root := cursorProjectsRoot(t)
	ws := resolvePath(t.TempDir())
	worktree := resolvePath(t.TempDir())

	p := writeCursorTranscript(t, root, "p/agent-transcripts/e/e.jsonl", cursorShellLine(worktree))
	got, matched := cursorClassify(p, []string{ws, worktree})
	if got != cursorMatchYes {
		t.Fatalf("worktree transcript classified %v, want yes", got)
	}
	if matched != worktree {
		t.Fatalf("matched root = %q, want the worktree %q", matched, worktree)
	}
}

// --- candidate discovery -----------------------------------------------------

func TestCandidateCursorTranscriptsFiltersByLayoutAndMtime(t *testing.T) {
	root := cursorProjectsRoot(t)
	cutoff := time.Now().Add(-1 * time.Hour)

	fresh := writeCursorTranscript(t, root, "p/agent-transcripts/a/a.jsonl", `{}`)
	stale := writeCursorTranscript(t, root, "p/agent-transcripts/b/b.jsonl", `{}`)
	notTranscript := writeCursorTranscript(t, root, "p/canvases/c.jsonl", `{}`)

	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	got := candidateCursorTranscripts(cutoff)
	if len(got) != 1 || got[0] != fresh {
		t.Fatalf("candidates = %v, want just %q", got, fresh)
	}
	for _, g := range got {
		if g == notTranscript {
			t.Fatal("a non-transcript .jsonl under the projects tree was picked up")
		}
	}

	// The mtime filter must cache NOTHING: a stale file touched later has to
	// re-enter classification. Caching a skip here is what dropped long and
	// restart-spanning sessions forever in the other two watchers.
	now := time.Now()
	if err := os.Chtimes(stale, now, now); err != nil {
		t.Fatal(err)
	}
	if len(candidateCursorTranscripts(cutoff)) != 2 {
		t.Fatal("a touched stale transcript did not re-enter the candidate set")
	}
}

// --- progress bookkeeping ----------------------------------------------------

// Bumping the schema version must drop every cached "no" exactly once, so a
// classification-rule change can re-evaluate files the poll loop would otherwise
// never look at again.
func TestLoadCursorWatchProgressDropsCachedNoOnSchemaBump(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	saveCursorWatchProgress(cursorWatchProgress{
		Offsets: map[string]int64{"a": 10, "b": 20},
		Match:   map[string]string{"a": "yes", "b": "no"},
		Roots:   map[string]string{"a": "/w"},
		V:       0,
	})

	got := loadCursorWatchProgress()
	if got.Match["a"] != "yes" {
		t.Fatal("a cached yes must survive the migration")
	}
	if _, present := got.Match["b"]; present {
		t.Fatal("a cached no must be dropped so it is re-classified")
	}
	if got.Offsets["b"] != 20 {
		t.Fatal("offsets must survive the migration — re-reading from 0 would re-emit")
	}
	if got.V != cursorProgressSchemaV {
		t.Fatalf("V = %d, want %d", got.V, cursorProgressSchemaV)
	}
	// Idempotent: a second load must not re-run the migration or lose anything.
	if again := loadCursorWatchProgress(); again.Match["a"] != "yes" || again.V != cursorProgressSchemaV {
		t.Fatalf("second load changed state: %#v", again)
	}
}

func TestLoadCursorWatchProgressHandlesMissingAndCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)

	p := loadCursorWatchProgress()
	if p.Offsets == nil || p.Match == nil || p.Roots == nil {
		t.Fatal("a missing progress file must yield initialised maps, not nils")
	}

	if err := os.WriteFile(cursorWatchProgressPath(), []byte("{{{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p = loadCursorWatchProgress()
	if p.Offsets == nil || p.Match == nil || p.Roots == nil {
		t.Fatal("a corrupt progress file must yield initialised maps, not nils")
	}
}

// --- go-forward capture ------------------------------------------------------

// Cursor transcripts carry NO timestamp anywhere, so there is no way to ask
// whether a session predates this watcher. Anything already on disk at the FIRST
// poll is therefore treated as pre-existing and seeded to EOF. Without this, the
// first daemon run would re-upload months of on-disk history.
func TestPollCursorTranscriptsSeedsPreExistingTranscriptsToEOF(t *testing.T) {
	root := cursorProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws := resolvePath(t.TempDir())

	path := writeCursorTranscript(t, root, "p/agent-transcripts/a/a.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>history we must not re-upload</user_query>"}]}}`,
		cursorShellLine(ws),
	)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	session := Session{TaskRoot: ws, DeviceID: "dev-test"}
	queued := pollCursorTranscripts(session, ws, time.Now().Add(-time.Hour), map[string]*normalize.CursorTranscriptProcessor{}, true, false)
	if queued != 0 {
		t.Fatalf("pre-existing history produced %d event(s) — capture must be go-forward", queued)
	}

	progress := loadCursorWatchProgress()
	key := cursorProgressKey(path)
	if progress.Match[key] != "yes" {
		t.Fatalf("transcript not classified: %#v", progress.Match)
	}
	if progress.Offsets[key] != info.Size() {
		t.Fatalf("offset = %d, want EOF %d", progress.Offsets[key], info.Size())
	}
	if progress.Roots[key] != ws {
		t.Fatalf("matched root not cached: %q", progress.Roots[key])
	}
}

// An undecided transcript must not be cached in either direction — the next poll
// has to look again, because the file is still growing toward its first path.
func TestPollCursorTranscriptsLeavesUndecidedUncached(t *testing.T) {
	root := cursorProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws := resolvePath(t.TempDir())

	path := writeCursorTranscript(t, root, "p/agent-transcripts/a/a.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>thinking out loud</user_query>"}]}}`,
	)

	session := Session{TaskRoot: ws, DeviceID: "dev-test"}
	pollCursorTranscripts(session, ws, time.Now().Add(-time.Hour), map[string]*normalize.CursorTranscriptProcessor{}, true, false)

	progress := loadCursorWatchProgress()
	key := cursorProgressKey(path)
	if v, present := progress.Match[key]; present {
		t.Fatalf("undecided transcript cached as %q — it must be retried", v)
	}
	if _, present := progress.Offsets[key]; present {
		t.Fatal("undecided transcript must not have an offset seeded yet")
	}
}

// The other half of the first-poll split, and the one that carries the actual
// capture value: a transcript that did not exist at startup is a NEW session, so
// it is tailed from byte 0 and its opening prompt survives.
//
// This is not a nicety. A Cursor transcript only becomes classifiable at its
// first TOOL CALL — that is the first record carrying an absolute path — which is
// already several records past the prompt that started the session. Seeding to
// EOF at the moment of classification would put every new session's prompt behind
// the seek, and capture would report file edits with nothing that asked for them.
func TestPollCursorTranscriptsTailsTranscriptsThatAppearAfterStartupFromZero(t *testing.T) {
	root := cursorProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws := resolvePath(t.TempDir())

	session := Session{TaskRoot: ws, DeviceID: "dev-test"}
	cutoff := time.Now().Add(-time.Hour)
	processors := map[string]*normalize.CursorTranscriptProcessor{}

	// First poll: nothing on disk yet. This is what marks the boundary.
	if queued := pollCursorTranscripts(session, ws, cutoff, processors, true, false); queued != 0 {
		t.Fatalf("empty first poll queued %d event(s)", queued)
	}

	// Now the engineer starts a Cursor session. The prompt lands first; the tool
	// call that reveals the workspace comes after it.
	path := writeCursorTranscript(t, root, "p/agent-transcripts/a/a.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>rename the handler</user_query>"}]}}`,
		cursorShellLine(ws),
	)

	queued := pollCursorTranscripts(session, ws, cutoff, processors, false, false)
	if queued < 2 {
		t.Fatalf("queued %d event(s) — a session that appeared after startup must be tailed whole (prompt + tool call)", queued)
	}

	progress := loadCursorWatchProgress()
	key := cursorProgressKey(path)
	if progress.Match[key] != "yes" {
		t.Fatalf("transcript not classified: %#v", progress.Match)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Offsets[key] != info.Size() {
		t.Fatalf("offset = %d, want the whole file consumed (%d)", progress.Offsets[key], info.Size())
	}
}

// --- review fixes ------------------------------------------------------------

// An agent working in the watched workspace routinely touches something outside
// it first — a global config, a doc under ~, a sibling repo it was asked to
// compare against. Deciding on the FIRST path made that one record cache a
// permanent "no", and every prompt, edit and command the session went on to make
// inside the workspace was skipped in silence.
func TestCursorClassifyDoesNotRejectOnASingleOutsidePath(t *testing.T) {
	root := cursorProjectsRoot(t)
	ws := resolvePath(t.TempDir())
	outside := resolvePath(t.TempDir())

	path := writeCursorTranscript(t, root, "p/agent-transcripts/a/a.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>compare these</user_query>"}]}}`,
		cursorShellLine(outside), // the agent looks somewhere else first
		cursorShellLine(ws),      // then works in ours
	)

	result, matched := cursorClassify(path, []string{ws})
	if result != cursorMatchYes {
		t.Fatalf("classify = %v, want yes — one outside path is not a mismatch", result)
	}
	if matched != ws {
		t.Fatalf("matched root = %q, want %q", matched, ws)
	}
}

// The caching that keeps the 3s poll cheap has to survive: a transcript whose
// every revealed path is outside really is another workspace's.
func TestCursorClassifyStillRejectsWhenEveryPathIsOutside(t *testing.T) {
	root := cursorProjectsRoot(t)
	ws := resolvePath(t.TempDir())
	outside := resolvePath(t.TempDir())

	path := writeCursorTranscript(t, root, "p/agent-transcripts/a/a.jsonl",
		cursorShellLine(outside),
		cursorShellLine(outside),
	)
	if result, _ := cursorClassify(path, []string{ws}); result != cursorMatchNo {
		t.Fatalf("classify = %v, want no", result)
	}
}

// A transcript being WRITTEN as the watcher comes up is live, not history — its
// mtime after the watcher's own start is the proof, and it is the one case where
// the absence of timestamps inside the file does not leave us guessing. Seeding
// it to EOF would seek past the opening prompt of a session happening right now.
func TestPollCursorTranscriptsTailsATranscriptStillBeingWrittenAtStartup(t *testing.T) {
	root := cursorProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws := resolvePath(t.TempDir())

	// The watcher started a minute ago; this transcript was touched just now.
	session := Session{TaskRoot: ws, DeviceID: "dev-test", StartedAt: time.Now().Add(-time.Minute)}
	writeCursorTranscript(t, root, "p/agent-transcripts/live/live.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>the prompt that must survive</user_query>"}]}}`,
		cursorShellLine(ws),
	)

	queued := pollCursorTranscripts(session, ws, session.StartedAt.Add(-2*time.Minute),
		map[string]*normalize.CursorTranscriptProcessor{}, true, false)
	if queued < 2 {
		t.Fatalf("queued %d event(s) for a transcript written after the watcher started — its opening prompt was seeked past", queued)
	}
}

// The other side of the same rule, and the one the split exists for: a
// transcript that predates the watcher is history and must not be re-uploaded.
func TestPollCursorTranscriptsStillSeedsTranscriptsOlderThanTheWatcher(t *testing.T) {
	root := cursorProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws := resolvePath(t.TempDir())

	path := writeCursorTranscript(t, root, "p/agent-transcripts/old/old.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>months of history</user_query>"}]}}`,
		cursorShellLine(ws),
	)
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	session := Session{TaskRoot: ws, DeviceID: "dev-test", StartedAt: time.Now()}
	queued := pollCursorTranscripts(session, ws, old.Add(-time.Hour),
		map[string]*normalize.CursorTranscriptProcessor{}, true, false)
	if queued != 0 {
		t.Fatalf("re-uploaded %d event(s) of pre-existing history", queued)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loadCursorWatchProgress().Offsets[cursorProgressKey(path)]; got != info.Size() {
		t.Fatalf("offset = %d, want EOF %d", got, info.Size())
	}
}

// A zero StartedAt makes every mtime "after" it. Without a guard that would flip
// the live-transcript rule from "tail the one session happening now" to "tail
// every transcript on disk from byte 0" — the months-of-history replay the
// first-poll split exists to prevent, reintroduced by an unset field.
func TestPollCursorTranscriptsSeedsWhenTheWatcherHasNoStartTime(t *testing.T) {
	root := cursorProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws := resolvePath(t.TempDir())

	writeCursorTranscript(t, root, "p/agent-transcripts/a/a.jsonl",
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>history</user_query>"}]}}`,
		cursorShellLine(ws),
	)

	session := Session{TaskRoot: ws, DeviceID: "dev-test"} // StartedAt deliberately unset
	if queued := pollCursorTranscripts(session, ws, time.Now().Add(-time.Hour),
		map[string]*normalize.CursorTranscriptProcessor{}, true, false); queued != 0 {
		t.Fatalf("an unset StartedAt replayed %d event(s) of history", queued)
	}
}

// The session and the lane are two different questions about the same file, and
// answering one with the other is what made within-session parallelism
// unmeasurable on this rail. The child's uuid is the LANE; the parent directory
// is the SESSION. Note the last two rows: for one sidechain file these two
// functions must disagree, and for a main transcript the lane must be empty
// rather than the session — a lane id that is really a session id reads
// downstream as a tool that runs exactly one lane, which is the specific wrong
// answer, not an obvious bug.
func TestCursorLaneIDFromPath(t *testing.T) {
	base := "/home/u/.cursor/projects/Users-u-repo/agent-transcripts"
	cases := []struct{ path, want string }{
		{base + "/e74dfa4f-f66b-44fe-b10f-39ecccca2fa6.jsonl", ""},
		{base + "/e74dfa4f/e74dfa4f.jsonl", ""},
		{base + "/9a2fe3d8/subagents/6497de01.jsonl", "6497de01"},
	}
	for _, c := range cases {
		if got := cursorLaneIDFromPath(c.path); got != c.want {
			t.Fatalf("cursorLaneIDFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
	sidechain := base + "/9a2fe3d8/subagents/6497de01.jsonl"
	if cursorLaneIDFromPath(sidechain) == cursorSessionIDFromPath(sidechain) {
		t.Fatal("lane and session resolved to the same value for a sidechain file")
	}
}

// The lane is derived from a PATH, and it is emitted into an allowlisted field —
// so the derivation must yield something the projector will accept as opaque.
// Taking the basename is what makes that true, and this is the assertion that
// notices if it ever stops being: a lane id carrying a directory would be a path
// reaching the wire through the one key that is allowed through.
func TestCursorLaneIDCarriesNoPath(t *testing.T) {
	base := "/home/u/.cursor/projects/Users-u-repo/agent-transcripts"
	lane := cursorLaneIDFromPath(base + "/9a2fe3d8/subagents/6497de01.jsonl")
	for _, frag := range []string{"/", `\`, ".", "~", "home", "Users", "agent-transcripts", "subagents"} {
		if strings.Contains(lane, frag) {
			t.Fatalf("lane id %q carries path fragment %q", lane, frag)
		}
	}
}
