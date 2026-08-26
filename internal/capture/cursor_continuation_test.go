package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// appendCursorLines appends complete records to an existing transcript, the way
// Cursor grows one between polls.
func appendCursorLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// --- fixture shapes ----------------------------------------------------------
//
// Every record below reproduces a shape observed in the 142-transcript corpus
// under ~/.cursor/projects, with the prose replaced. The corpus' complete
// top-level key-set union is {role, message} ∪ {type, status, error} — there is
// no conversation id anywhere in it, which is the whole reason continuation has
// to be reconstructed rather than read.

func cursorUserLine(text string) string {
	return `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Thursday, Aug 6, 2026, 1:48 PM (UTC-5)</timestamp>\n<user_query>` + text + `</user_query>"}]}}`
}

// cursorMoveLookupLine is the SCHEMA FETCH, which is all four of the six corpus
// predecessors that carry no invocation actually hold: the turn died before the
// call was written.
func cursorMoveLookupLine(tool string) string {
	return `{"role":"assistant","message":{"content":[{"type":"text","text":"Relocating."},{"type":"tool_use","name":"GetMcpTools","input":{"server":"cursor-app-control","toolName":"` + tool + `"}}]}}`
}

// cursorMoveCallLine is the invocation, recorded under CallMcpTool.
func cursorMoveCallLine(tool string) string {
	return `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"CallMcpTool","input":{"server":"cursor-app-control","toolName":"` + tool + `"}}]}}`
}

// cursorDynamicMoveLine is the second shape the corpus records the same tool
// under. It carries NO server field, which is why cursorTranscriptHasAgentMove
// keys on toolName alone.
func cursorDynamicMoveLine(tool string) string {
	return `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"GetDynamicTools","input":{"toolName":"` + tool + `"}}]}}`
}

// cursorTurnEndedErrorLine is the record a move leaves behind on the transcript
// it kills. Every one of the six corpus continuations ends its predecessor here.
func cursorTurnEndedErrorLine() string {
	return `{"type":"turn_ended","status":"error","error":"User aborted request"}`
}

// --- the detector ------------------------------------------------------------

// TestCursorResolveSessionID drives the four-condition detector over the shapes
// the corpus actually contains.
//
// The two directions are not symmetric and the table says so. A missed
// continuation costs a duplicate the server's
// UNIQUE(org_id, session_id, kind, ts, md5(data)) index cannot collapse across
// two session ids — bad, and exactly today's behaviour. A false adoption merges
// two real sessions and silently eats events into a conversation they were never
// part of, which nothing downstream can detect. Every "want no adoption" row
// below is guarding the second.
func TestCursorResolveSessionID(t *testing.T) {
	const (
		aID = "3df490ff-1111-4111-8111-111111111111"
		bID = "44da3651-2222-4222-8222-222222222222"
	)
	sharedHead := []string{
		cursorUserLine("pick up the handoff"),
		cursorMoveLookupLine("move_agent_to_cloned_root"),
	}

	cases := []struct {
		name string
		// files, keyed by projects-relative path.
		files map[string][]string
		// known are the projects-relative paths already in progress.Offsets, in
		// the order the watcher saw them.
		known []string
		// target is the projects-relative path being resolved.
		target string
		want   string
	}{
		{
			// The defect, end to end: same opening records, different project
			// directory, predecessor holds a move.
			name: "continuation adopts the earlier id",
			files: map[string][]string{
				"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl": append(append([]string{}, sharedHead...), cursorTurnEndedErrorLine()),
				"proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl": append(append([]string{}, sharedHead...), cursorMoveCallLine("move_agent_to_cloned_root"), cursorUserLine("carry on")),
			},
			known:  []string{"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"},
			target: "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl",
			want:   aID,
		},
		{
			// The corpus' dynamic-tool shape: same tool, no `server` field.
			// Requiring server=="cursor-app-control" dropped this continuation
			// in simulation against the real corpus.
			name: "continuation adopts when the move is recorded as a dynamic tool",
			files: map[string][]string{
				"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl": {
					cursorUserLine("pick up the handoff"),
					cursorDynamicMoveLine("move_agent_to_root"),
					cursorTurnEndedErrorLine(),
				},
				"proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl": {
					cursorUserLine("pick up the handoff"),
					cursorDynamicMoveLine("move_agent_to_root"),
					cursorUserLine("carry on"),
				},
			},
			known:  []string{"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"},
			target: "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl",
			want:   aID,
		},
		{
			// THE FALSE PAIR. Two subagents handed identical opening briefs in
			// ONE project directory. Three such pairs exist in the corpus and
			// one is byte-identical for its whole length, so no prefix length
			// separates them — the project directory does, because a move always
			// relocates.
			name: "same project directory never adopts",
			files: map[string][]string{
				"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl": append(append([]string{}, sharedHead...), cursorMoveCallLine("move_agent_to_root")),
				"proj-a/agent-transcripts/" + bID + "/" + bID + ".jsonl": append(append([]string{}, sharedHead...), cursorMoveCallLine("move_agent_to_root")),
			},
			known:  []string{"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"},
			target: "proj-a/agent-transcripts/" + bID + "/" + bID + ".jsonl",
			want:   bID,
		},
		{
			// Cross-directory and a matching prefix, but nothing that could have
			// relocated the agent. Two engineers re-running the same saved
			// prompt in two repos is not one conversation.
			name: "no move in the predecessor never adopts",
			files: map[string][]string{
				"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl": {
					cursorUserLine("pick up the handoff"),
					cursorShellLine("/tmp/x"),
					cursorTurnEndedErrorLine(),
				},
				"proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl": {
					cursorUserLine("pick up the handoff"),
					cursorShellLine("/tmp/x"),
					cursorUserLine("carry on"),
				},
			},
			known:  []string{"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"},
			target: "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl",
			want:   bID,
		},
		{
			// K = 2. One shared record is not a prefix, it is a coincidence —
			// and the corpus has a genuine instance of exactly that.
			name: "one shared record is below K and never adopts",
			files: map[string][]string{
				"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl": {
					cursorUserLine("pick up the handoff"),
					cursorMoveCallLine("move_agent_to_root"),
					cursorTurnEndedErrorLine(),
				},
				"proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl": {
					cursorUserLine("pick up the handoff"),
					cursorUserLine("something else entirely"),
				},
			},
			known:  []string{"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"},
			target: "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl",
			want:   bID,
		},
		{
			// The new transcript itself has not written K complete records yet.
			// Fail to today's behaviour rather than adopt on one record.
			name: "fewer than K records in the new transcript never adopts",
			files: map[string][]string{
				"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl": append(append([]string{}, sharedHead...), cursorTurnEndedErrorLine()),
				"proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl": {sharedHead[0]},
			},
			known:  []string{"proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"},
			target: "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl",
			want:   bID,
		},
		{
			// Nothing tracked yet: the very first transcript the watcher sees
			// cannot continue anything.
			name: "no candidates at all mints today's id",
			files: map[string][]string{
				"proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl": append(append([]string{}, sharedHead...), cursorUserLine("carry on")),
			},
			known:  nil,
			target: "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl",
			want:   bID,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := cursorProjectsRoot(t)
			for rel, lines := range c.files {
				writeCursorTranscript(t, root, rel, lines...)
			}
			progress := cursorWatchProgress{
				Offsets:  map[string]int64{},
				Match:    map[string]string{},
				Roots:    map[string]string{},
				Sessions: map[string]string{},
			}
			for _, rel := range c.known {
				k := cursorProgressKey(filepath.Join(root, filepath.FromSlash(rel)))
				progress.Offsets[k] = 0
				progress.Match[k] = "yes"
			}
			target := filepath.Join(root, filepath.FromSlash(c.target))
			key := cursorProgressKey(target)
			got, cacheable := cursorResolveSessionID(target, key, progress, false)
			if got != c.want {
				t.Fatalf("session id = %q, want %q", got, c.want)
			}
			if !cacheable {
				t.Fatalf("a main-chain resolution must be cacheable; got cacheable=false for %q", got)
			}
		})
	}
}

// A MISSING PREDECESSOR IS NOT A MATCH. The watcher remembers a transcript by
// path, and Cursor's projects tree is the engineer's to prune. Once the file the
// prefix would have been compared against is gone there is nothing to compare,
// so the new transcript takes its own id — today's behaviour, and the only
// answer that is not a guess.
func TestCursorResolveSessionIDFallsBackWhenThePredecessorIsGone(t *testing.T) {
	const (
		aID = "3df490ff-1111-4111-8111-111111111111"
		bID = "44da3651-2222-4222-8222-222222222222"
	)
	root := cursorProjectsRoot(t)
	shared := []string{
		cursorUserLine("pick up the handoff"),
		cursorMoveLookupLine("move_agent_to_cloned_root"),
	}
	aRel := "proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"
	bRel := "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl"
	writeCursorTranscript(t, root, bRel, append(append([]string{}, shared...), cursorUserLine("carry on"))...)

	aKey := filepath.ToSlash(aRel)
	progress := cursorWatchProgress{
		// A remembered predecessor whose file no longer exists.
		Offsets:  map[string]int64{aKey: 0},
		Match:    map[string]string{aKey: "yes"},
		Roots:    map[string]string{},
		Sessions: map[string]string{},
	}
	bPath := filepath.Join(root, filepath.FromSlash(bRel))
	got, cacheable := cursorResolveSessionID(bPath, cursorProgressKey(bPath), progress, false)
	if got != bID {
		t.Fatalf("session id = %q, want the file's own id %q — an absent predecessor cannot be matched", got, bID)
	}
	if !cacheable {
		t.Fatal("fallback resolution must be cacheable")
	}
}

// ONE CONVERSATION MOVED TWICE IS STILL ONE CONVERSATION. The corpus holds a
// chain of three: A ended on a turn_ended error, B rewrote it under a new uuid
// in a new directory and was itself moved, C rewrote B. All three must land on
// A's id.
//
// THE DIRECTORY NAMES ARE NOT DECORATION. Candidates are scanned in sorted key
// order, and in the corpus the worktree directory sorts BEFORE the repo it was
// cut from ('-' < '/'), so C meets B first — never A. That is what forces the
// resolver down the transitive path, reading B's RECORDED id rather than B's
// filename. Name the directories the other way and the test passes on the
// direct A-match, proving nothing: a mutation replacing the recorded-id lookup
// with cursorSessionIDFromPath(candidate) stayed green until this was fixed.
//
// The second phase removes A from disk entirely, so there is no direct match
// left to fall back on and the recorded id is the only route to aID.
func TestCursorContinuationChainsTransitively(t *testing.T) {
	const (
		aID = "3df490ff-1111-4111-8111-111111111111"
		bID = "44da3651-2222-4222-8222-222222222222"
		cID = "025c841e-3333-4333-8333-333333333333"
	)
	root := cursorProjectsRoot(t)
	head := []string{
		cursorUserLine("pick up the handoff"),
		cursorMoveLookupLine("move_agent_to_cloned_root"),
	}
	rel := map[string]string{
		aID: "repo-main/agent-transcripts/" + aID + "/" + aID + ".jsonl",
		bID: "repo-main-worktree-b/agent-transcripts/" + bID + "/" + bID + ".jsonl",
		cID: "repo-main-worktree-c/agent-transcripts/" + cID + "/" + cID + ".jsonl",
	}
	if !(filepath.ToSlash(rel[bID]) < filepath.ToSlash(rel[aID])) {
		t.Fatalf("fixture no longer forces the transitive path: %q must sort before %q", rel[bID], rel[aID])
	}
	writeCursorTranscript(t, root, rel[aID], append(append([]string{}, head...), cursorTurnEndedErrorLine())...)
	writeCursorTranscript(t, root, rel[bID], append(append([]string{}, head...),
		cursorMoveCallLine("move_agent_to_cloned_root"), cursorTurnEndedErrorLine())...)
	writeCursorTranscript(t, root, rel[cID], append(append([]string{}, head...),
		cursorMoveCallLine("move_agent_to_cloned_root"), cursorUserLine("carry on"))...)

	progress := cursorWatchProgress{
		Offsets:  map[string]int64{},
		Match:    map[string]string{},
		Roots:    map[string]string{},
		Sessions: map[string]string{},
	}
	resolve := func(id string) string {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel[id]))
		k := cursorProgressKey(p)
		got, cacheable := cursorResolveSessionID(p, k, progress, false)
		if !cacheable {
			t.Fatalf("%s: resolution not cacheable", id)
		}
		progress.Sessions[k] = got
		progress.Offsets[k] = 0
		progress.Match[k] = "yes"
		return got
	}

	// Discovery order, exactly as the poll loop records each answer.
	for _, id := range []string{aID, bID, cID} {
		if got := resolve(id); got != aID {
			t.Fatalf("%s resolved to %q, want the head of the chain %q", id, got, aID)
		}
	}

	// Second phase: the head is pruned from disk but still remembered. C is
	// re-resolved from scratch; only B's recorded id can still reach aID.
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel[aID]))); err != nil {
		t.Fatal(err)
	}
	cPath := filepath.Join(root, filepath.FromSlash(rel[cID]))
	delete(progress.Sessions, cursorProgressKey(cPath))
	got, _ := cursorResolveSessionID(cPath, cursorProgressKey(cPath), progress, false)
	if got != aID {
		t.Fatalf("with the head pruned, C resolved to %q, want %q via B's recorded id", got, aID)
	}
}

// A SUBAGENT MUST NOT BE ORPHANED BY THE FIX. cursorSessionIDFromPath rolls a
// sidechain up to the uuid of the directory holding it, and that rollup stays.
// But once the parent adopts an earlier id, the on-disk uuid is a session its
// own main chain no longer uses — so the rollup target is routed through the
// same map. Without this the fix would trade one split for another: prompts on
// the adopted id, delegation events on the abandoned one.
func TestCursorSidechainFollowsItsParentsAdoptedID(t *testing.T) {
	const (
		aID     = "9bee0746-1111-4111-8111-111111111111"
		bID     = "fbdbfd64-2222-4222-8222-222222222222"
		childID = "c0ffee00-4444-4444-8444-444444444444"
	)
	root := cursorProjectsRoot(t)
	bRel := "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl"
	childRel := "proj-b/agent-transcripts/" + bID + "/subagents/" + childID + ".jsonl"
	writeCursorTranscript(t, root, bRel, cursorUserLine("x"))
	writeCursorTranscript(t, root, childRel, cursorUserLine("delegated brief"))

	bKey := cursorProgressKey(filepath.Join(root, filepath.FromSlash(bRel)))
	childPath := filepath.Join(root, filepath.FromSlash(childRel))

	// Sanity: the rollup itself is untouched.
	if got := cursorSessionIDFromPath(childPath); got != bID {
		t.Fatalf("cursorSessionIDFromPath(sidechain) = %q, want the parent dir %q", got, bID)
	}

	// Parent has adopted A's id.
	progress := cursorWatchProgress{
		Offsets:  map[string]int64{bKey: 0},
		Match:    map[string]string{bKey: "yes"},
		Roots:    map[string]string{},
		Sessions: map[string]string{bKey: aID},
	}
	got, cacheable := cursorResolveSessionID(childPath, cursorProgressKey(childPath), progress, false)
	if got != aID {
		t.Fatalf("sidechain session id = %q, want its parent's adopted id %q", got, aID)
	}
	if !cacheable {
		t.Fatal("a sidechain resolved against a known parent must be cacheable")
	}

	// Parent not resolved yet: fall back to the rollup, and say the answer is
	// PROVISIONAL rather than freezing it — the parent may adopt on a later poll.
	progress.Sessions = map[string]string{}
	got, cacheable = cursorResolveSessionID(childPath, cursorProgressKey(childPath), progress, false)
	if got != bID {
		t.Fatalf("sidechain fallback = %q, want the rollup %q", got, bID)
	}
	if cacheable {
		t.Fatal("a sidechain resolved before its parent must NOT be cached — the parent can still adopt")
	}
}

// --- the mechanism the detector rests on -------------------------------------

func TestCursorTranscriptHasAgentMove(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"schema lookup counts", []string{cursorMoveLookupLine("move_agent_to_root")}, true},
		{"invocation counts", []string{cursorMoveCallLine("move_agent_to_cloned_root")}, true},
		{"dynamic-tool shape with no server counts", []string{cursorDynamicMoveLine("move_agent_to_root")}, true},
		{"an ordinary tool call does not", []string{cursorShellLine("/tmp/x")}, false},
		{"a turn_ended error alone does not", []string{cursorTurnEndedErrorLine()}, false},
		{"the words in prose do not", []string{cursorUserLine("please call move_agent_to_root for me")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := cursorProjectsRoot(t)
			p := writeCursorTranscript(t, root, "proj/agent-transcripts/a/a.jsonl", c.lines...)
			if got := cursorTranscriptHasAgentMove(p); got != c.want {
				t.Fatalf("cursorTranscriptHasAgentMove = %v, want %v", got, c.want)
			}
		})
	}
}

// A transcript being written right now can end mid-record. Comparing a torn line
// against a complete one reports a divergence that does not exist, so an
// unterminated trailing line is not a record yet.
func TestCursorHeadRecordsIgnoresAnUnterminatedTrailingLine(t *testing.T) {
	root := cursorProjectsRoot(t)
	p := writeCursorTranscript(t, root, "proj/agent-transcripts/a/a.jsonl", cursorUserLine("one"))
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"role":"user","message":{"content":[{"type":"te`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got := cursorHeadRecords(p, 2)
	if len(got) != 1 {
		t.Fatalf("cursorHeadRecords returned %d record(s), want 1 — the partial line is not a record", len(got))
	}
}

func TestCursorSidechainParentKey(t *testing.T) {
	root := cursorProjectsRoot(t)
	child := filepath.Join(root, "proj", "agent-transcripts", "parent", "subagents", "child.jsonl")
	if got, want := cursorSidechainParentKey(child), "proj/agent-transcripts/parent/parent.jsonl"; got != want {
		t.Fatalf("cursorSidechainParentKey = %q, want %q", got, want)
	}
	main := filepath.Join(root, "proj", "agent-transcripts", "parent", "parent.jsonl")
	if got := cursorSidechainParentKey(main); got != "" {
		t.Fatalf("cursorSidechainParentKey(main chain) = %q, want empty", got)
	}
}

// --- through the poll loop ---------------------------------------------------

// The end the defect is actually measured at: two transcripts, one conversation,
// one session id on the wire. The rewritten transcript still keeps its OWN byte
// offset — unifying identity must not collapse the per-path bookkeeping that
// makes each line read exactly once.
func TestPollCursorTranscriptsEmitsAContinuationUnderTheOriginalSessionID(t *testing.T) {
	const (
		aID = "3df490ff-1111-4111-8111-111111111111"
		bID = "44da3651-2222-4222-8222-222222222222"
	)
	root := cursorProjectsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws := resolvePath(t.TempDir())

	session := Session{TaskRoot: ws, DeviceID: "dev-test"}
	cutoff := time.Now().Add(-time.Hour)
	processors := map[string]*normalize.CursorTranscriptProcessor{}

	head := []string{cursorUserLine("pick up the handoff"), cursorShellLine(ws)}

	// First poll with nothing on disk marks the pre-existing/new boundary.
	pollCursorTranscripts(session, ws, cutoff, processors, true, false)

	aPath := writeCursorTranscript(t, root, "proj-a/agent-transcripts/"+aID+"/"+aID+".jsonl",
		append(append([]string{}, head...), cursorMoveLookupLine("move_agent_to_cloned_root"), cursorTurnEndedErrorLine())...)
	if q := pollCursorTranscripts(session, ws, cutoff, processors, false, false); q == 0 {
		t.Fatal("the original transcript queued nothing")
	}

	// The move: Cursor rewrites the whole conversation under a new uuid in a new
	// project directory.
	bPath := writeCursorTranscript(t, root, "proj-b/agent-transcripts/"+bID+"/"+bID+".jsonl",
		append(append([]string{}, head...), cursorMoveCallLine("move_agent_to_cloned_root"), cursorShellLine(ws))...)
	if q := pollCursorTranscripts(session, ws, cutoff, processors, false, false); q == 0 {
		t.Fatal("the rewritten transcript queued nothing")
	}

	progress := loadCursorWatchProgress()
	aKey, bKey := cursorProgressKey(aPath), cursorProgressKey(bPath)
	if progress.Sessions[bKey] != aID {
		t.Fatalf("rewritten transcript emitted under %q, want the original session %q", progress.Sessions[bKey], aID)
	}
	if progress.Sessions[aKey] != aID {
		t.Fatalf("original transcript emitted under %q, want %q", progress.Sessions[aKey], aID)
	}
	// Identity is unified; offsets are NOT. Each path keeps its own cursor.
	if progress.Offsets[aKey] == 0 || progress.Offsets[bKey] == 0 {
		t.Fatalf("per-path offsets not advanced independently: a=%d b=%d", progress.Offsets[aKey], progress.Offsets[bKey])
	}
	if progress.Offsets[aKey] == progress.Offsets[bKey] {
		t.Fatalf("both transcripts share offset %d — the bookkeeping collapsed with the identity", progress.Offsets[aKey])
	}
	if got := processors[bKey]; got == nil {
		t.Fatal("no processor for the rewritten transcript")
	}
}

// THE HOOK RAIL AND THIS ONE MUST NOT DISAGREE ABOUT WHAT SESSION A TRANSCRIPT
// IS. On a hook-claimed transcript this rail emits only cursorHookBlindKinds,
// and that is safe precisely because both rails resolve the same id. The hook
// rail reads Cursor's own payload, which after a move is the NEW uuid — so
// adopting the earlier one here would strand task_dispatch and mcp_call on a
// session carrying no prompts. A duplicate on two narrow kinds is the cheaper
// error than a split.
func TestCursorResolveSessionIDLeavesAHookClaimedTranscriptAlone(t *testing.T) {
	const (
		aID = "3df490ff-1111-4111-8111-111111111111"
		bID = "44da3651-2222-4222-8222-222222222222"
	)
	root := cursorProjectsRoot(t)
	shared := []string{
		cursorUserLine("pick up the handoff"),
		cursorMoveLookupLine("move_agent_to_cloned_root"),
	}
	aRel := "proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"
	bRel := "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl"
	writeCursorTranscript(t, root, aRel, append(append([]string{}, shared...), cursorTurnEndedErrorLine())...)
	bPath := writeCursorTranscript(t, root, bRel, append(append([]string{}, shared...), cursorUserLine("carry on"))...)

	aKey := cursorProgressKey(filepath.Join(root, filepath.FromSlash(aRel)))
	progress := cursorWatchProgress{
		Offsets:  map[string]int64{aKey: 0},
		Match:    map[string]string{aKey: "yes"},
		Roots:    map[string]string{},
		Sessions: map[string]string{},
	}

	// Unclaimed, this is a continuation and adopts.
	if got, _ := cursorResolveSessionID(bPath, cursorProgressKey(bPath), progress, false); got != aID {
		t.Fatalf("unclaimed continuation resolved to %q, want %q", got, aID)
	}
	// Claimed by the hook rail, it keeps its own id — and the answer is not
	// frozen, because the claim carries a TTL.
	got, cacheable := cursorResolveSessionID(bPath, cursorProgressKey(bPath), progress, true)
	if got != bID {
		t.Fatalf("hook-claimed transcript resolved to %q, want its own id %q", got, bID)
	}
	if cacheable {
		t.Fatal("a hook-claimed resolution must not be cached — the claim expires")
	}
}

// --- a provisional answer must be re-asked -------------------------------------

// cursorQueuedEvents returns every event on the outbox, in order.
func cursorQueuedEvents(t *testing.T) []event.Event {
	t.Helper()
	data, err := os.ReadFile(state.OutboxPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read outbox: %v", err)
	}
	var out []event.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev event.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal outbox line: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// cursorEventsFromLane returns the queued events carrying a given `agentId` —
// the subagent transcript's own uuid, which rides every event it emits. It is
// how a test tells the child's events from its parent's.
func cursorEventsFromLane(t *testing.T, laneID string) []event.Event {
	t.Helper()
	var out []event.Event
	for _, ev := range cursorQueuedEvents(t) {
		d, ok := ev.Data.(map[string]interface{})
		if !ok {
			continue
		}
		if id, _ := d["agentId"].(string); id == laneID {
			out = append(out, ev)
		}
	}
	return out
}

// A PROVISIONAL ANSWER MUST BE RE-ASKED, AND THE PROCESSOR CACHE IS WHERE THAT
// STOPPED HAPPENING.
//
// cursorResolveSessionID reports cacheable=false for a subagent whose parent has
// not resolved yet, and pollCursorTranscripts honours that by not writing
// progress.Sessions. But `processors[key]` used to be populated unconditionally,
// and a non-nil processor meant the resolver was never consulted again for that
// key — so the answer was frozen one level ABOVE the map that was refusing to
// freeze it, and a subagent whose parent later adopted an earlier id kept
// emitting under the abandoned uuid for the daemon's life.
//
// Poll 1: the child reveals a workspace path and is captured, but its parent has
// revealed none yet and stays undecided — so the child's id is provisional.
// Poll 2: the parent reveals a path, matches the head across a project
// directory, and adopts. The child must follow.
func TestPollCursorTranscriptsReResolvesAProvisionalSidechain(t *testing.T) {
	const (
		aID     = "3df490ff-1111-4111-8111-111111111111"
		bID     = "44da3651-2222-4222-8222-222222222222"
		childID = "c0ffee00-4444-4444-8444-444444444444"
	)
	root := cursorProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))
	ws := resolvePath(t.TempDir())

	// The shared prefix carries NO path, so a transcript holding only it stays
	// undecided — which is what makes the child provisional on poll 1.
	shared := []string{
		cursorUserLine("pick up the handoff"),
		cursorUserLine("second record of the shared prefix"),
	}
	aRel := "proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"
	bRel := "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl"
	childRel := "proj-b/agent-transcripts/" + bID + "/subagents/" + childID + ".jsonl"

	session := Session{TaskRoot: ws, DeviceID: "dev-test"}
	cutoff := time.Now().Add(-time.Hour)
	processors := map[string]*normalize.CursorTranscriptProcessor{}

	// An EMPTY first poll sets the pre-existing/new boundary. The transcripts
	// have to be written after it: anything already on disk at first poll is
	// history and is seeded to EOF, which would emit nothing at all.
	pollCursorTranscripts(session, ws, cutoff, map[string]*normalize.CursorTranscriptProcessor{}, true, false)

	writeCursorTranscript(t, root, aRel, append(append([]string{}, shared...),
		cursorShellLine(ws), cursorMoveLookupLine("move_agent_to_cloned_root"), cursorTurnEndedErrorLine())...)
	bPath := writeCursorTranscript(t, root, bRel, shared...)
	childPath := writeCursorTranscript(t, root, childRel,
		cursorUserLine("delegated brief"), cursorShellLine(ws))

	// Poll 1.
	pollCursorTranscripts(session, ws, cutoff, processors, false, false)

	childKey := cursorProgressKey(childPath)
	progress := loadCursorWatchProgress()
	if got, present := progress.Sessions[childKey]; present {
		t.Fatalf("a provisional sidechain was frozen into progress.Sessions as %q", got)
	}
	if got := progress.Sessions[cursorProgressKey(bPath)]; got != "" {
		t.Fatalf("parent resolved on poll 1 as %q — the fixture no longer exercises the provisional path", got)
	}
	early := cursorEventsFromLane(t, childID)
	if len(early) == 0 {
		t.Fatal("the child queued nothing on poll 1 — the fixture is not exercising capture")
	}
	// The documented residual, asserted rather than left implicit: events emitted
	// while the answer was provisional stay on the id they were sent under.
	if got := early[len(early)-1].SessionID; got != bID {
		t.Fatalf("poll-1 child event carried %q, want the on-disk parent uuid %q", got, bID)
	}

	// Poll 2: the parent finally reveals a path and adopts the head's id.
	appendCursorLines(t, bPath, cursorShellLine(ws), cursorMoveCallLine("move_agent_to_cloned_root"))
	appendCursorLines(t, childPath, cursorShellLine(ws))
	pollCursorTranscripts(session, ws, cutoff, processors, false, false)

	progress = loadCursorWatchProgress()
	if got := progress.Sessions[cursorProgressKey(bPath)]; got != aID {
		t.Fatalf("parent resolved to %q, want the adopted id %q", got, aID)
	}
	if got := progress.Sessions[childKey]; got != aID {
		t.Fatalf("sidechain resolved to %q, want its parent's adopted id %q", got, aID)
	}
	late := cursorEventsFromLane(t, childID)
	if len(late) <= len(early) {
		t.Fatalf("the child queued nothing new on poll 2 (%d then %d)", len(early), len(late))
	}
	if got := late[len(late)-1].SessionID; got != aID {
		t.Fatalf("poll-2 child event carried %q, want the adopted id %q — the provisional answer was never re-asked", got, aID)
	}
}

// THE ANCHOR IS THE REGRESSION THIS FIX COULD PLAUSIBLY INTRODUCE. Cursor stamps
// no per-record time; all that exists is the <timestamp> injected into a user
// turn, held on the processor as tsAnchor and inherited by every assistant
// record after it. Rebuilding the processor to change its session id would reset
// that anchor to zero and every later event would silently fall back to read
// time — moving `ts`, which is a column in the server's dedupe key.
//
// So: an anchored turn on poll 1, an assistant record on poll 2 across a
// migration, and the second must carry the first's time.
func TestPollCursorTranscriptsKeepsTheTimestampAnchorAcrossAMigration(t *testing.T) {
	const (
		aID     = "3df490ff-1111-4111-8111-111111111111"
		bID     = "44da3651-2222-4222-8222-222222222222"
		childID = "c0ffee00-4444-4444-8444-444444444444"
		// What the fixture's <timestamp> parses to: 1:48 PM at UTC-5.
		wantTs = "2026-08-06T18:48:00Z"
	)
	root := cursorProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))
	ws := resolvePath(t.TempDir())

	shared := []string{
		cursorUserLine("pick up the handoff"),
		cursorUserLine("second record of the shared prefix"),
	}
	aRel := "proj-a/agent-transcripts/" + aID + "/" + aID + ".jsonl"
	bRel := "proj-b/agent-transcripts/" + bID + "/" + bID + ".jsonl"
	childRel := "proj-b/agent-transcripts/" + bID + "/subagents/" + childID + ".jsonl"

	session := Session{TaskRoot: ws, DeviceID: "dev-test"}
	cutoff := time.Now().Add(-time.Hour)
	processors := map[string]*normalize.CursorTranscriptProcessor{}

	// Empty first poll first — see the sidechain test for why.
	pollCursorTranscripts(session, ws, cutoff, map[string]*normalize.CursorTranscriptProcessor{}, true, false)

	writeCursorTranscript(t, root, aRel, append(append([]string{}, shared...),
		cursorShellLine(ws), cursorMoveLookupLine("move_agent_to_cloned_root"), cursorTurnEndedErrorLine())...)
	bPath := writeCursorTranscript(t, root, bRel, shared...)
	// The child's brief carries the anchor; a sidechain emits no prompt from it
	// but DOES take the time off it.
	childPath := writeCursorTranscript(t, root, childRel,
		cursorUserLine("delegated brief"), cursorShellLine(ws))

	pollCursorTranscripts(session, ws, cutoff, processors, false, false)

	early := cursorEventsFromLane(t, childID)
	if len(early) == 0 {
		t.Fatal("the child queued nothing on poll 1")
	}
	if got := early[len(early)-1].Ts; got != wantTs {
		t.Fatalf("poll-1 child event ts = %q, want the anchored %q", got, wantTs)
	}

	// The migration happens on this poll.
	appendCursorLines(t, bPath, cursorShellLine(ws), cursorMoveCallLine("move_agent_to_cloned_root"))
	appendCursorLines(t, childPath, cursorShellLine(ws))
	pollCursorTranscripts(session, ws, cutoff, processors, false, false)

	late := cursorEventsFromLane(t, childID)
	if len(late) <= len(early) {
		t.Fatalf("the child queued nothing new on poll 2 (%d then %d)", len(early), len(late))
	}
	last := late[len(late)-1]
	// Guard: without a migration this test would pass for the wrong reason.
	if last.SessionID != aID {
		t.Fatalf("poll-2 child event carried %q, want the adopted id %q — no migration happened, so this proves nothing about the anchor", last.SessionID, aID)
	}
	if last.Ts != wantTs {
		t.Fatalf("poll-2 child event ts = %q, want the anchor %q carried across the migration (a rebuilt processor falls back to read time)", last.Ts, wantTs)
	}
}
