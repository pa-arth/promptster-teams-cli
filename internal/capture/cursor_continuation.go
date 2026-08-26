package capture

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CURSOR REWRITES A CONVERSATION UNDER A NEW UUID, AND THAT LOOKS LIKE A NEW
// SESSION TO EVERY IDENTITY WE HAVE.
//
// When an agent calls `cursor-app-control`'s `move_agent_to_root` or
// `move_agent_to_cloned_root`, Cursor terminates the running transcript on a
// `{"type":"turn_ended","status":"error",...}` record and re-writes the ENTIRE
// conversation so far into a NEW file, under a NEW uuid, in a DIFFERENT project
// directory. cursorSessionIDFromPath reads the session id off the filename, so
// the rewritten history arrives as a brand-new session, at offset 0, and every
// prompt and tool call the engineer already made is ingested a second time.
//
// The server does not save us. The teams ingest path dedupes on
// `UNIQUE (org_id, session_id, kind, ts, md5(data::text))` with
// `onConflictDoNothing`, so content-identical events collapse — but only WITHIN
// ONE session_id. Unifying the session id is therefore not merely cosmetic: it
// is the whole fix, and it needs no backend change.
//
// Measured on 142 real transcripts in ~/.cursor/projects (2026-08-25):
// 16 first-record-duplicate groups covering 19 redundant files (13.4% of the
// corpus); 6 of those are true continuations and every one of the 6 crosses a
// project directory and carries a move call. One group is a chain of THREE.
//
// WHY THE DETECTOR IS A CONJUNCTION OF FOUR CONDITIONS. Merging two genuinely
// distinct sessions is strictly worse than duplicating one: a duplicate is
// absorbed by the server's index, whereas a bad merge silently eats real events
// into a session they did not belong to and nothing downstream can tell. So
// every condition below exists to make a false merge harder, and the failure
// direction of every one of them is "mint today's id" — i.e. fall back to the
// duplicate we already ship.
//
// THE FIRST RECORD ALONE IS NOT A DETECTOR, and the corpus proves it. Three
// same-directory pairs share a first record with no move anywhere in them: two
// subagents handed identical opening briefs, and two pairs of scaffolding runs.
// One of those pairs is byte-identical for its entire 6-record length. So a
// prefix test of ANY length fails to separate them; what separates them is
// condition 2 (different project directories), because a move always relocates.

// cursorContinuationPrefixRecords is K: how many leading records the new
// transcript must reproduce byte-for-byte from the one it claims to continue.
//
// K = 2, and it is a CEILING chosen by the data rather than a floor. Across the
// six true continuations in the corpus the contiguous identical prefixes run
// 2, 8, 10, 12, 13 and 15 records; the shortest is a predecessor that was
// aborted two records in (a prompt, one assistant turn, then the turn_ended
// error). Any K above 2 discards that continuation for nothing, and K below 2
// weakens the test with no continuation to gain. K therefore sits at the
// largest value that still admits every continuation we have observed.
//
// NOTE WHAT K IS *NOT* DOING. It is tempting to justify K as the thing that
// rejects the false pairs; it is not. Of the three same-directory look-alikes,
// one diverges at record 1 (K=2 rejects it), one shares 2 records, and one is
// identical for all 6. Conditions 2 and 3 reject all three. K is a
// belt-and-braces check on top of them, not the load-bearing one.
const cursorContinuationPrefixRecords = 2

// cursorMoveScanBuffer bounds a single record while scanning for a move call.
// Matches the ceiling cursorClassify already uses on the same files.
const cursorMoveScanBuffer = 8 * 1024 * 1024

// cursorAgentMoveTools are the `cursor-app-control` tools whose invocation
// relocates a running agent — the only two that trigger the rewrite.
var cursorAgentMoveTools = map[string]bool{
	"move_agent_to_root":        true,
	"move_agent_to_cloned_root": true,
}

// cursorMoveMarker is a cheap pre-filter so the JSON parse below runs on the
// handful of records that could possibly match rather than on every line.
var cursorMoveMarker = []byte("move_agent_to_")

// cursorMoveProbe is the minimum of a Cursor record needed to spot a move.
// Deliberately NOT normalize's cursorRecord: this is a capture-side question
// (which file continues which) and must not couple to the normalizer's shape.
//
// Input is a RawMessage, parsed per item, so ONE content item with a
// non-object input cannot make the whole record — including a move sitting
// beside it — unparseable. All 58 move occurrences in the corpus sit at
// message.content[*]; none is nested deeper.
type cursorMoveProbe struct {
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

type cursorMoveInput struct {
	Server   string `json:"server"`
	ToolName string `json:"toolName"`
}

// cursorResolveSessionID answers "which conversation do this file's events
// belong to", which is a different question from cursorSessionIDFromPath's
// "what does this path say". It returns the id and whether that answer is
// stable enough to persist.
//
// The order matters: a previously recorded answer wins outright, so a decision
// made once survives daemon restarts and never re-derives differently.
func cursorResolveSessionID(path, key string, progress cursorWatchProgress, hookClaimed bool) (string, bool) {
	base := cursorSessionIDFromPath(path)
	if id := progress.Sessions[key]; id != "" {
		return id, true
	}

	// A CLAIMED TRANSCRIPT KEEPS ITS FILENAME ID, and this is not an oversight.
	//
	// On a claimed transcript this rail emits only cursorHookBlindKinds —
	// task_dispatch and mcp_call — and the reason that is safe is stated there:
	// both rails agree on session identity, so those two kinds land on the SAME
	// session the hooks are populating instead of forking a second one. The
	// hook rail reads its session id from Cursor's own payload, which after a
	// move is the NEW uuid; adopting the earlier one here would put delegation
	// and MCP identity on a session with no prompts in it, which is the phantom
	// session the subagent rollup exists to prevent.
	//
	// The cost is bounded to those two kinds re-arriving under the new id on a
	// hook-enrolled machine. That is a duplicate; the alternative is a split.
	//
	// Not cached: the claim carries a TTL (see isCursorHookClaimed), so this
	// answer is only true while the hook rail is alive, and freezing it would
	// outlive the reason for it.
	if hookClaimed {
		return base, false
	}

	// A SUBAGENT FOLLOWS ITS PARENT, IT DOES NOT DETECT ITS OWN CONTINUATION.
	//
	// cursorSessionIDFromPath already rolls a sidechain up to the uuid of the
	// directory holding it, and that rollup stays exactly as it is. But when the
	// PARENT adopted an earlier id, rolling up to the on-disk uuid would leave
	// the subagents of a moved session pointing at a session id its own main
	// chain no longer uses — a fresh split, created by the fix. So the rollup
	// target is itself routed through this map.
	//
	// filepath.Walk is lexical and a hex uuid always sorts before the literal
	// "subagents", so within any one poll the parent is resolved first and the
	// lookup below is populated. When it is not — the parent is still undecided,
	// or belongs to another workspace — we fall back to the on-disk uuid and say
	// so by returning cacheable=false, because that answer may change on a
	// later poll and must not be frozen.
	if isCursorSidechainFile(path) {
		parentKey := cursorSidechainParentKey(path)
		if parentKey == "" {
			return base, true
		}
		if id := progress.Sessions[parentKey]; id != "" {
			return id, true
		}
		return base, false
	}

	if id, ok := cursorContinuationSessionID(path, key, progress); ok {
		return id, true
	}
	return base, true
}

// cursorContinuationSessionID looks for a transcript this watcher already
// tracks that the file at `path` is a rewritten continuation of, and returns
// that transcript's RESOLVED session id.
//
// Returning the resolved id rather than the candidate's filename uuid is what
// makes continuations chain: in the corpus one conversation was moved twice
// (A → B → C), and C matches B, whose own recorded id is already A's. Reading
// the candidate's filename instead would land C on B and leave the three split
// two ways instead of three.
//
// Candidates come from progress.Offsets — the transcripts this watcher has
// classified as belonging to the session and actually tailed. A predecessor we
// never tracked cannot be found here, and that is the honest limit of the
// method: a move ACROSS a workspace boundary, where only one side is watched,
// produces no duplicate to unify in the first place.
func cursorContinuationSessionID(path, key string, progress cursorWatchProgress) (string, bool) {
	head := cursorHeadRecords(path, cursorContinuationPrefixRecords)
	if len(head) < cursorContinuationPrefixRecords {
		return "", false
	}
	own := cursorSessionIDFromPath(path)
	selfDir := cursorProjectDirOfKey(key)

	// Sorted so a corpus with two eligible predecessors (the chain) resolves the
	// same way on every poll and after every restart. Any eligible candidate is
	// correct — they all resolve to the same head — but only a deterministic
	// choice is testable.
	known := make([]string, 0, len(progress.Offsets))
	for k := range progress.Offsets {
		known = append(known, k)
	}
	sort.Strings(known)

	for _, candidateKey := range known {
		if candidateKey == key {
			continue
		}
		// (2) A move always relocates. Two transcripts filed under the same
		// project directory were never separated by one, whatever they share.
		if cursorProjectDirOfKey(candidateKey) == selfDir {
			continue
		}
		candidatePath := cursorTranscriptPathForKey(candidateKey)
		if isCursorSidechainFile(candidatePath) {
			continue
		}
		// Also the "predecessor is gone from disk" case: an unreadable file
		// yields no records and is skipped, so the caller mints today's id.
		candidateHead := cursorHeadRecords(candidatePath, cursorContinuationPrefixRecords)
		if len(candidateHead) < cursorContinuationPrefixRecords {
			continue
		}
		// (1) + (4) The first record matches, and so do the K-1 after it.
		if !cursorRecordsEqual(candidateHead, head) {
			continue
		}
		// (3) The predecessor shows the mechanism that would have caused the
		// rewrite. Last because it is the only condition that reads a whole
		// file, and by here at most one candidate reaches it.
		if !cursorTranscriptHasAgentMove(candidatePath) {
			continue
		}
		id := progress.Sessions[candidateKey]
		if id == "" {
			id = cursorSessionIDFromPath(candidatePath)
		}
		if id == "" || id == own {
			return "", false
		}
		return id, true
	}
	return "", false
}

// cursorTranscriptHasAgentMove reports whether a transcript contains a
// `move_agent_to_*` tool call.
//
// IT ACCEPTS THE SCHEMA LOOKUP AS WELL AS THE INVOCATION, and that is not
// laziness. Cursor records the tool under two names — `GetMcpTools` /
// `GetDynamicTools` (the schema fetch) and `CallMcpTool` / `CallDynamicTool`
// (the invocation) — and in FOUR of the six corpus continuations the
// predecessor holds only the fetch: its last record is the turn_ended error, so
// the invocation itself was never written to the file that the move ended. Only
// the rewritten successor carries it. Requiring the invocation would therefore
// miss two-thirds of the cases this function exists to find.
//
// It also does NOT require `server == "cursor-app-control"`. One corpus
// predecessor records the same tool through the dynamic-tool shape, which
// carries no server field at all; demanding the server string silently dropped
// that continuation in simulation. The toolName is the discriminating part.
func cursorTranscriptHasAgentMove(path string) bool {
	// #nosec G304 -- path is a Cursor transcript discovered under the projects dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), cursorMoveScanBuffer)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, cursorMoveMarker) {
			continue
		}
		var rec cursorMoveProbe
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		for _, item := range rec.Message.Content {
			if item.Type != "tool_use" || len(item.Input) == 0 {
				continue
			}
			var in cursorMoveInput
			if err := json.Unmarshal(item.Input, &in); err != nil {
				continue
			}
			if cursorAgentMoveTools[in.ToolName] {
				return true
			}
		}
	}
	return false
}

// cursorHeadRecords returns up to n leading non-blank records, trimmed.
//
// ONLY NEWLINE-TERMINATED LINES COUNT, the same discipline tailCursorTranscript
// uses: a transcript being written right now can end mid-record, and comparing
// a torn line against a complete one would report a divergence that does not
// exist. A file that has not yet written K complete records simply fails the
// prefix test this poll and is re-examined on the next one.
func cursorHeadRecords(path string, n int) [][]byte {
	// #nosec G304 -- path is a Cursor transcript discovered under the projects dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	out := make([][]byte, 0, n)
	for len(out) < n {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		out = append(out, append([]byte(nil), trimmed...))
	}
	return out
}

func cursorRecordsEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// cursorProjectDirOfKey is the `<munged-workspace>` segment of a progress key —
// the directory Cursor filed the transcript under. Keys are always
// slash-normalized (see cursorProgressKey), so this reads the same on Windows.
func cursorProjectDirOfKey(key string) string {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i]
	}
	return key
}

// cursorTranscriptPathForKey is the inverse of cursorProgressKey.
func cursorTranscriptPathForKey(key string) string {
	return filepath.Join(CursorProjectsDir(), filepath.FromSlash(key))
}

// cursorSidechainParentKey is the progress key of the main-chain transcript a
// subagent hangs off:
//
//	<proj>/agent-transcripts/<parent>/subagents/<child>.jsonl
//	                     →  <proj>/agent-transcripts/<parent>/<parent>.jsonl
//
// Empty for anything that is not a sidechain.
func cursorSidechainParentKey(path string) string {
	if !isCursorSidechainFile(path) {
		return ""
	}
	parentDir := filepath.Dir(filepath.Dir(path))
	return cursorProgressKey(filepath.Join(parentDir, filepath.Base(parentDir)+".jsonl"))
}
