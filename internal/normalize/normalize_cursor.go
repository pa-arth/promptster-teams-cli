package normalize

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Cursor transcript normalization.
//
// THIS IS ONE OF TWO CURSOR RAILS. The other is the hook rail
// (normalize_cursor_hook.go), which is primary because its payload carries a
// real model, exact timestamps and exact cwd — none of which exist below.
//
// This rail is the fallback, and it is not redundant: it needs no enrollment, so
// it covers sessions that predate install and machines where hook enrollment
// failed or was refused. The hook rail claims a session by transcript path and
// this rail then skips it, so one session is never captured twice.
//
// The hook config we write is the USER scope, `~/.cursor/hooks.json` — a single
// global file outside every repository. We never write Cursor's PROJECT scope,
// `<workspace>/.cursor/hooks.json`: that one is a tracked file inside the
// customer's repo and enrolls per-workspace, so every repo an engineer forgets
// would read as "captured nothing". An earlier revision of this comment cited
// those two objections as grounds for rejecting hooks ENTIRELY; they are
// grounds for rejecting one scope. See CLAUDE.md, "Capture surfaces".
//
// THE FORMAT, verified against 61 real transcripts (2026-02 → 2026-07) and one
// live `cursor-agent` run. Every record, in every file, is exactly:
//
//	{"role":"user"|"assistant","message":{"content":[ …items… ]}}
//
// and NOTHING else — no timestamp field, no cwd, no model, no token usage, and
// no `tool_result` records at all. That is not an omission in this parser; the
// key-set union across the whole corpus is `{role, message}` and `{content}`.
// Do not add a field here on the assumption Cursor "must" send it — check a
// real file first, the way this list was built.
//
// Content items are `{"type":"text","text":…}` or
// `{"type":"tool_use","name":…,"input":{…}}`. The tool vocabulary observed
// across the corpus, re-counted 2026-08-25 over 142 transcripts (7,314 tool_use
// items), most frequent first: Read 2114, Shell 1952, Grep 1407, StrReplace 702,
// Glob 343, UpdateCurrentStep 192, Write 143, GetMcpTools 95, AwaitShell 86,
// TodoWrite 64, CallMcpTool 62, Task 50, SearchConversations 24,
// SetActiveBranch 21, WebSearch 16, Await 8, SwitchMode 7, WebFetch 6,
// GetDynamicTools 6, CallDynamicTool 5, CreatePlan 4, Delete 3, AskQuestion 3,
// ReadLints 1.
//
// THIS LIST GREW BY EIGHT NAMES between the 61-transcript reading above and the
// 142-transcript one — AwaitShell, GetMcpTools, GetDynamicTools,
// CallDynamicTool, SearchConversations, SetActiveBranch, SwitchMode and
// AskQuestion. Cursor's vocabulary is not stable, and one of those additions
// (CallDynamicTool) was a RENAME of a mapped tool that silently zeroed a board
// rather than a new capability. A name absent from this list is not
// evidence Cursor does not send it; it is evidence nobody has re-counted since.
// Re-run the count before concluding anything from an absence here.
//
// WHAT LEAVES THE MACHINE. Cursor's edit tools carry the code itself —
// StrReplace's old_string/new_string, Write's contents. Those are COUNTED here
// and then dropped on the floor; they are never placed in Data, never placed in
// RawPayload (which this normalizer never sets at all), and never named in an
// error string. Reading them on-device is the job; egress is the line. The
// redaction projector would strip them anyway — `file_diff` allowlists only
// path/linesAdded/linesRemoved — but defence in depth is the point: a leak
// should require two independent mistakes, not one.

// cursorUserQuery isolates the human's typed text from Cursor's prompt
// envelope.
//
// A user record's text is NOT the prompt. Cursor wraps it in tags and rides
// other material alongside — `<attached_files>` (24 occurrences in the corpus)
// carries FILE CONTENTS the engineer attached, and one transcript carries a
// `<git_diff_from_branch_to_main>` block. Taking the whole text would ship
// source through the one kind whose `text` field is allowlisted, which is the
// single worst place in this pipeline to be loose.
//
// So this is deliberately a WHITELIST, not a blacklist: only the inside of
// `<user_query>` is a prompt. A user record without one produces no event
// rather than a best-effort guess — a new envelope tag we have not seen must
// fail closed to silence, never to "ship whatever was in there".
var cursorUserQuery = regexp.MustCompile(`(?s)<user_query>(.*?)</user_query>`)

// cursorTimestamp matches the only wall-clock signal Cursor persists:
// `<timestamp>Friday, Jul 31, 2026, 5:10 PM (UTC-5)</timestamp>`, injected into
// the user turn's own text. It is human-formatted, MINUTE resolution, and
// carries a whole-hour UTC offset rather than a zone name.
//
// It appears on user turns ONLY, and only on recent Cursor builds (4 of 209
// user records in the corpus have it). Everything else — every assistant record
// — has no time of its own. See cursorEventTs for what that costs and how it is
// handled honestly.
var cursorTimestamp = regexp.MustCompile(`<timestamp>[^,]+, ([A-Z][a-z]{2} \d{1,2}, \d{4}), (\d{1,2}:\d{2} [AP]M) \(UTC([+-]\d{1,2})\)</timestamp>`)

// CursorTranscriptProcessor converts one Cursor agent transcript into canonical
// events. One processor per transcript file, held for the daemon's life so the
// prompt-timestamp anchor survives across polls.
type CursorTranscriptProcessor struct {
	sessionID string

	// Sidechain marks a subagent transcript
	// (<parent>/agent-transcripts/<parent>/subagents/<child>.jsonl). Its "user"
	// records are agent-authored task briefs, not the engineer's prompts, so
	// they must never enter the human timeline — the same rule the Claude
	// processor enforces with UsageOnly, for the same reason.
	Sidechain bool

	// LaneID is THIS subagent's own identity — the child transcript's filename
	// uuid — set only when Sidechain is true. It rides every event this
	// processor emits as `agentId`,
	// the same key Claude and Codex already use for the same thing.
	//
	// It exists because sessionID deliberately does NOT carry it.
	// cursorSessionIDFromPath rolls a child up to its parent so one conversation
	// stays one session, which is right and stays; the cost was that the child's
	// own identity was DISCARDED at capture, so every Cursor subagent's events
	// landed under the parent with nothing to tell one from another. Cursor was
	// the only rail of the three where within-session concurrency was not
	// computable, and this is the whole of that gap: the identity was already on
	// disk as the filename, we simply threw it away.
	//
	// Session identity and lane identity are different questions and now have
	// different fields. Do not re-derive one from the other.
	LaneID string

	// Workdir is the session's cwd, home-collapsed by the caller's resolution
	// pass. Cursor records no cwd anywhere in the transcript (unlike Claude Code
	// and Codex), so capture resolves it once from the transcript's own observed
	// absolute paths and threads it in — see capture.cursorSessionCwd.
	Workdir string

	// RepoRoot / RepoHost / RepoTracked are the canonical per-session repo
	// identity, resolved ONCE in internal/capture from the same cwd (git needs
	// fs/exec, so it cannot be derived here) and threaded in as session state.
	// Identical contract to the Claude and Codex processors: all three come from
	// ONE resolution pass so they always describe one observation of one
	// directory.
	RepoRoot    string
	RepoHost    string
	RepoTracked bool

	// tsAnchor is the last wall-clock time recovered from a user turn's
	// <timestamp>. Assistant records that follow inherit it (see cursorEventTs).
	tsAnchor time.Time
}

func NewCursorTranscriptProcessor(sessionID string) *CursorTranscriptProcessor {
	return &CursorTranscriptProcessor{sessionID: sessionID}
}

// cursorRecord is the complete on-disk record shape. Deliberately exhaustive:
// if a future Cursor build adds a field, this struct failing to grow is a
// visible gap rather than a silent one.
type cursorRecord struct {
	Role    string `json:"role"`
	Message struct {
		Content []cursorContentItem `json:"content"`
	} `json:"message"`
}

type cursorContentItem struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Process parses one transcript line and returns zero or more canonical events.
//
// offset is the line's byte position in its transcript. It is the STABLE
// IDENTITY of the record and the basis of every event id this processor mints:
// a record's offset does not change when the watcher restarts, when the file is
// re-tailed, or when an event is re-sent after a transport error, so the
// backend can collapse duplicates at the source of identity. A counter would
// not survive a restart mid-file (it would restart at 0 while the offset was
// mid-file and mint ids that collide with EARLIER, different records — dedupe
// would then silently eat real events, which is strictly worse than duplicates).
func (p *CursorTranscriptProcessor) Process(line []byte, offset int64) []event.Event {
	var rec cursorRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}

	var out []event.Event
	for i, item := range rec.Message.Content {
		switch {
		case rec.Role == "user" && item.Type == "text":
			if ev, ok := p.promptEvent(item.Text, offset, i); ok {
				out = append(out, ev)
			}
		case rec.Role == "assistant" && item.Type == "tool_use":
			if ev, ok := p.toolEvent(item, offset, i); ok {
				out = append(out, ev)
			}
		}
	}
	return stampLaneID(out, p.LaneID)
}

// promptEvent builds the human `prompt` event from a user turn.
func (p *CursorTranscriptProcessor) promptEvent(text string, offset int64, idx int) (event.Event, bool) {
	// Anchor first, and unconditionally: even a sidechain's brief carries the
	// wall clock, and dropping the record must not drop the time signal that
	// every following assistant record depends on.
	p.noteTimestamp(text)

	// A subagent's "user" record is the parent agent's task brief. It is
	// AI-authored, so emitting it as a prompt would credit the engineer with
	// prompts they never wrote and pollute the prompt-quality signal.
	if p.Sidechain {
		return event.Event{}, false
	}

	m := cursorUserQuery.FindStringSubmatch(text)
	if m == nil {
		return event.Event{}, false
	}
	query := strings.TrimSpace(m[1])
	if query == "" {
		return event.Event{}, false
	}

	e := p.newEvent("prompt", offset, idx)
	e.Actor = event.HumanActor()
	e.Provenance = transcriptHumanProvenance()

	data := map[string]interface{}{"text": query}
	// workdir / repoRoot / repoHost / repoTracked — identical contract and
	// identical omit-when-empty rules as the Claude processor. repoTracked is
	// stamped explicitly true OR false whenever repoRoot is, so "not a repo"
	// stays distinguishable from "a CLI too old to have looked".
	if p.Workdir != "" {
		data["workdir"] = p.Workdir
	}
	if p.RepoRoot != "" {
		data["repoRoot"] = p.RepoRoot
		data["repoTracked"] = p.RepoTracked
	}
	if p.RepoHost != "" {
		data["repoHost"] = p.RepoHost
	}
	e.Data = data
	return e, true
}

// cursorEditInput is the union of the MAPPED tools' inputs. old_string/
// new_string/contents are read ONLY to be counted; see the package note.
//
// The key names below are verified against 142 real transcripts, not assumed —
// the standing rule in this file's header. `Task` carries
// {description, prompt, subagent_type} with optional {model, run_in_background,
// resume}; `CallMcpTool` carries {server, toolName, arguments} with optional
// {description} (34 of its 62 corpus calls omit `description`, 28 carry it);
// `CallDynamicTool` carries {namespace, toolName, arguments} and, in all 5
// corpus calls, exactly those three — no `description` has yet been observed on
// it, so do not add one on the assumption that it mirrors CallMcpTool.
//
// `prompt` and `arguments` are deliberately NOT fields here. They are the full
// delegated instruction and the full MCP call payload — free text that can
// carry anything the human typed or the agent read. Leaving them unparsed means
// no later edit can accidentally place them in Data.
type cursorEditInput struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
	Contents  string `json:"contents"`
	Command   string `json:"command"`

	// Task
	Description  string `json:"description"`
	SubagentType string `json:"subagent_type"`
	// CallMcpTool
	Server string `json:"server"`
	// CallDynamicTool. The exact analogue of Server — same position, same
	// meaning, renamed by Cursor. Kept as a SEPARATE field rather than a second
	// json tag on Server (which Go does not support anyway) so the resolve in
	// toolEvent stays keyed on the tool NAME and never coalesces two fields
	// whose co-occurrence has never been observed.
	Namespace string `json:"namespace"`
	// Both.
	ToolName string `json:"toolName"`
}

// cursorBuiltinNamespacePrefix marks Cursor's OWN tool namespaces, which ride
// the same invocation envelope as real MCP servers but are not MCP.
//
// Only two prefix families exist across the 142-transcript corpus: `cursor-*`
// (Cursor built-ins — `cursor-app-control`'s chat management, `cursor-ide-browser`)
// and `user-*` (servers the engineer configured — Railway, supabase, clerk,
// promptster). Nothing else has been observed on either invocation name.
//
// DENYLIST, NOT ALLOWLIST — deliberate, and the direction matters more than the
// list. Excluding `cursor-*` fails OPEN: an MCP server arriving under some third
// prefix (a future project- or org-scoped namespace) stays on the board, which
// is correct, and a wrong guess costs a visible row somebody can question. An
// allowlist on `user-*` would fail CLOSED and drop that server silently — which
// is the exact invisible-loss defect this whole patch exists to fix, so
// re-introducing it one line lower would be self-defeating. A NEW Cursor
// built-in will still be named `cursor-*` and will still be caught; a new MCP
// namespace shape is the case we cannot predict, so it is the one that must
// survive.
const cursorBuiltinNamespacePrefix = "cursor-"

// isCursorBuiltinNamespace is the single site of that decision, shared by both
// invocation names below. Written once on purpose: the same predicate applied
// in two places is one edit away from being applied in one and a half.
func isCursorBuiltinNamespace(ns string) bool {
	return strings.HasPrefix(ns, cursorBuiltinNamespacePrefix)
}

// toolEvent maps one assistant tool_use to a canonical event.
//
// SCOPE. Only the tools that describe WORK are mapped: edits, file creation,
// deletion, shell commands, delegation, and MCP calls. Read/Grep/Glob are 782 of
// the corpus's 1,809 tool calls and say only that the agent looked at something
// — they are deliberately unmapped rather than shipped as volume.
//
// Task and CallMcpTool were previously in that unmapped set, with the stated
// reason that they "need allowlist review on both sides before they can carry
// anything". That review is done: `task_dispatch` allowlists {name, status,
// summary} and `mcp_call` allowlists {name, tool, status} in BOTH
// internal/redact/project.go and the backend's eventFieldProjection.ts, so the
// keys emitted below survive projection on both rails. This widens neither
// allowlist — it gives two already-sanctioned shapes their first Cursor
// producer. Without it a Cursor engineer's delegations and MCP usage are not
// merely unpriced, they are INVISIBLE, and the asset boards read that absence as
// "this engineer built and used nothing".
//
// TodoWrite/CreatePlan remain unmapped for the original reason.
func (p *CursorTranscriptProcessor) toolEvent(item cursorContentItem, offset int64, idx int) (event.Event, bool) {
	var in cursorEditInput
	if len(item.Input) > 0 {
		// A malformed input is a dropped event, never a partial one: every
		// branch below reads a path or a command, and half a path is worse than
		// no event.
		if err := json.Unmarshal(item.Input, &in); err != nil {
			return event.Event{}, false
		}
	}

	switch item.Name {
	case "StrReplace":
		if in.Path == "" {
			return event.Event{}, false
		}
		e := p.newAIEvent("file_diff", offset, idx)
		e.Data = map[string]interface{}{
			"path": in.Path,
			// COUNTS ONLY. The strings themselves die with this function.
			"linesAdded":   cursorLineCount(in.NewString),
			"linesRemoved": cursorLineCount(in.OldString),
		}
		return e, true

	case "Write":
		if in.Path == "" {
			return event.Event{}, false
		}
		e := p.newAIEvent("file_create", offset, idx)
		e.Data = map[string]interface{}{
			"path":       in.Path,
			"linesAdded": cursorLineCount(in.Contents),
			"sizeBytes":  len(in.Contents),
		}
		return e, true

	case "Delete":
		if in.Path == "" {
			return event.Event{}, false
		}
		e := p.newAIEvent("file_delete", offset, idx)
		e.Data = map[string]interface{}{"path": in.Path}
		return e, true

	case "Shell":
		if in.Command == "" {
			return event.Event{}, false
		}
		e := p.newAIEvent("command", offset, idx)
		// The command STRING only. Cursor's transcript records no tool results
		// at all, so there is no stdout, no stderr and no exit code to leak —
		// and none to report either. exitCode is left ABSENT rather than
		// fabricated as 0, which would report every failed command as a success.
		e.Data = map[string]interface{}{"command": in.Command}
		return e, true

	case "Task":
		// `task_dispatch`, NOT `subagent_usage`. The two are not
		// interchangeable: subagent_usage is a SPEND event — its allowlist is
		// projectUsageFields plus attribution — and Cursor's transcripts carry no
		// token usage at ALL (no usage object, no tool_result records, nothing).
		// Emitting one here would put a row on the spend boards whose token
		// counts are absent, and an absent count reads downstream as a measured
		// zero. task_dispatch is the "which agent was spun up" dimension, which
		// is the question actually being asked, and it needs no numbers to answer.
		//
		// Keys match the Claude/Codex branch in normalize.go exactly, so Cursor
		// delegations land in the same buckets as everyone else's rather than
		// opening a parallel vocabulary.
		e := p.newAIEvent("task_dispatch", offset, idx)
		data := map[string]interface{}{
			// Description only — never `prompt`, which is the full delegated
			// instruction and can carry anything the human typed.
			"summary": strPreview(in.Description, 100),
		}
		// Trim BEFORE testing: a whitespace-only subagent_type passes `!= ""`
		// and then emits `name: ""`, which opens a nameless agent-type bucket
		// instead of being skipped. One of the 27 real Task calls in the corpus
		// carries no subagent_type at all, so the omit path is exercised in the
		// field, not just in theory.
		if st := strings.TrimSpace(in.SubagentType); st != "" {
			data["name"] = st
		}
		e.Data = data
		return e, true

	case "CallMcpTool", "CallDynamicTool":
		// TWO NAMES, ONE CALL. Cursor renamed this invocation around
		// 2026-08-22T20:29Z: `CallMcpTool` with {server, toolName, arguments}
		// became `CallDynamicTool` with {namespace, toolName, arguments}.
		// `namespace` is the exact analogue of `server` — same position, same
		// meaning, new spelling. Handling only the old name silently dropped
		// EVERY post-cut Cursor MCP call, and a dropped mcp_call is invisible:
		// the board reads it as "this engineer used no MCP", not as a gap.
		//
		// On the cut TIME specifically: the local corpus only BRACKETS it. The two
		// names never co-occur in any transcript; the last file carrying
		// `CallMcpTool` was last written 2026-08-22T07:12Z and the first carrying
		// `CallDynamicTool` 2026-08-26T02:48Z, so file mtimes place the cut
		// somewhere in between and cannot narrow it further. The 20:29Z above
		// comes from outside this corpus — treat the bracket as what the
		// transcripts on disk actually prove.
		//
		// `CallMcpTool` stays mapped and is not dead code. The watcher re-reads
		// transcripts back to `transcriptHistoryWindow`
		// (internal/capture/session.go, 28 days), so old-format records are
		// live input on every install for four weeks after the rename, and
		// longer on machines that were offline.
		//
		// `CallDynamicTool` IS A SUPERSET OF MCP, and this is the part a
		// one-line rename would get wrong. It carries Cursor's own tooling on
		// the same envelope: all 5 CallDynamicTool calls in the corpus are
		// `namespace: "cursor-app-control"` — rename_chat, move_agent_to_root,
		// move_agent_to_cloned_root — and NONE is an MCP call. Mapping the name
		// straight across would have put 5 of 5 non-MCP invocations on the MCP
		// board.
		//
		// APPLYING THE FILTER TO `CallMcpTool` IS A CORRECTION, NOT A
		// REFACTOR, and it will move a live number. The same pollution was
		// already shipping on the old name: of 62 corpus CallMcpTool calls, 42
		// are Cursor built-ins (cursor-app-control 40, cursor-ide-browser 2)
		// against 20 real MCP calls (user-Railway 8, user-supabase 6,
		// user-clerk 3, user-promptster 3). So expect Cursor `mcp_call` volume
		// to fall by roughly two thirds once this deploys. That drop is the
		// board becoming true, not the rail breaking — do not "fix" it by
		// loosening the predicate.
		//
		// Cursor names the server and the tool in SEPARATE fields, where Claude
		// and Codex ship one `server__tool` string. Recompose it into the single
		// name so `mcpServerOf` — which splits on the first `__` — resolves the
		// server for all three tools with no per-tool branch.
		ns := in.Server
		if item.Name == "CallDynamicTool" {
			ns = in.Namespace
		}
		if ns == "" || in.ToolName == "" {
			// Half a name is worse than none: `__foo` or `bar__` would split into
			// an empty server and open a nameless bucket on the MCP board.
			return event.Event{}, false
		}
		if isCursorBuiltinNamespace(ns) {
			// Cursor's own chat/IDE tooling. Not MCP, and counting it as MCP
			// overstates every MCP-adoption figure a Cursor engineer appears in.
			return event.Event{}, false
		}
		e := p.newAIEvent("mcp_call", offset, idx)
		// NO `status`. Cursor records no tool results anywhere in the transcript,
		// so there is no outcome to report — and stamping "ok" would report every
		// failed MCP call as a success. Absent is the honest value, same reasoning
		// as the missing exitCode on Shell above.
		//
		// NO `arguments`. It is the full call payload and is never parsed.
		e.Data = map[string]interface{}{"tool": ns + "__" + in.ToolName}
		return e, true
	}

	return event.Event{}, false
}

// newEvent builds the canonical envelope with a deterministic, offset-derived
// id and the best timestamp available.
func (p *CursorTranscriptProcessor) newEvent(kind string, offset int64, idx int) event.Event {
	e := event.NewEvent(kind, p.sessionID)
	e.ID = event.DeterministicUUID(
		p.sessionID + "\x1f" + kind + "\x1f" + strconv.FormatInt(offset, 10) + "\x1f" + strconv.Itoa(idx),
	)
	// `source` is what puts "cursor" into the session row's source_service
	// array, which is how the backend tells a genuinely captured engineer from a
	// bare enrollment row. An engineer with no tool in source_service reads as
	// "captured nothing" — do not change this string without changing
	// AGENT_SOURCES and TOOL_ROSTER on the backend to match.
	e.Source = "cursor"
	if ts := p.eventTs(); ts != "" {
		e.Ts = ts
	}
	return e
}

func (p *CursorTranscriptProcessor) newAIEvent(kind string, offset int64, idx int) event.Event {
	e := p.newEvent(kind, offset, idx)
	e.Actor = event.AIActor()
	e.Provenance = transcriptAiProvenance()
	return e
}

// stampLane marks an event with this subagent's lane identity. No-op on the main
// chain, which HAS no lane id — the main thread is not a delegated agent, and
// stamping one would make every session read as though it delegated to itself.
//
// Applied at Process's single return, to EVERY event the sidechain emits, rather
// than to one chosen kind. A first draft picked ai_response as "the one event per
// turn"; the Cursor transcript never emits ai_response at all (its kinds are
// command / file_create / file_delete / file_diff / mcp_call / prompt /
// task_dispatch), so that carrier would have stamped nothing and the lane would
// have stayed invisible while looking implemented. Stamping every event also
// makes the span robust to a subagent whose whole life is, say, three commands.
//
// Cursor deliberately emits no subagent_usage (see the Task branch: its
// transcripts carry no token usage, and a spend event with absent counts reads
// downstream as a measured zero). That decision stands and does not block this
// one — a LANE needs identity and timestamps, not spend.

// noteTimestamp records the wall clock from a user turn's <timestamp> envelope,
// if this Cursor build writes one.
func (p *CursorTranscriptProcessor) noteTimestamp(text string) {
	m := cursorTimestamp.FindStringSubmatch(text)
	if m == nil {
		return
	}
	offsetHours, err := strconv.Atoi(m[3])
	if err != nil {
		return
	}
	t, err := time.Parse("Jan 2, 2006 3:04 PM", m[1]+" "+m[2])
	if err != nil {
		return
	}
	// Reinterpret the parsed wall-clock reading in the zone Cursor named. The
	// offset is whole hours in every sample; a future half-hour zone would fail
	// the regex and fall through to the read-time default rather than land an
	// event 30 minutes wrong.
	p.tsAnchor = t.Add(-time.Duration(offsetHours) * time.Hour).UTC()
}

// eventTs returns the timestamp to stamp, or "" to keep event.NewEvent's
// read-time default.
//
// HONEST LIMIT, because this is the one place Cursor is genuinely worse than
// the other two surfaces. Claude Code and Codex stamp every transcript record
// with its own RFC3339 timestamp; Cursor stamps none. All that exists is a
// minute-resolution string injected into user turns on recent builds. So:
//
//   - a prompt, and every assistant action in the turn that follows it, is
//     stamped with that turn's anchor when one is available;
//   - with no anchor (older Cursor builds), the read time stands.
//
// Within-file ORDER is always exact — the file is append-only and tailed in
// order — and because CURSOR capture is go-forward-only (its missing timestamp
// is exactly why the 28-day history backfill covers Claude and Codex alone),
// read time is at worst one poll interval late. What is genuinely lost is
// intra-turn latency: a 46-minute turn's actions all carry its start time. Daily
// and weekly rollups are unaffected; per-action timing is not available from
// this surface and must not be reported as though it were.
func (p *CursorTranscriptProcessor) eventTs() string {
	if p.tsAnchor.IsZero() {
		return ""
	}
	return p.tsAnchor.Format(time.RFC3339Nano)
}

// cursorLineCount counts the lines a string contributes to a diff. The empty
// string is 0 (a pure insertion has no lines on the old side), and a single
// trailing newline is not a line of its own — "foo\n" is one line, not two.
//
// EVERY read of old_string/new_string on EITHER Cursor rail terminates here, and
// this returns an integer. That is the containment boundary. The hook rail's
// cursorHookEditLineCounts routes through this same function rather than
// counting for itself, so the boundary stays one function wide as rails are
// added — keep it that way.
func cursorLineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

// CursorSessionWorkdir home-collapses an absolute cwd for the `workdir` field.
// Exported so capture can resolve it in the same pass that resolves the repo
// identity. HomeRelativeStrict returns "" for anything not provably under the
// home directory, so an outside-home path is omitted rather than shipped with
// the OS username in it.
func CursorSessionWorkdir(cwd string) string {
	return state.HomeRelativeStrict(cwd)
}

// CursorObservedPath extracts the first absolute filesystem path a transcript
// line reveals, so capture can decide whether the session belongs to the
// watched workspace.
//
// This exists because Cursor records no cwd. The transcript's directory name is
// a munged form of the workspace path, and it is NOT a reliable key: two
// different munging behaviours are observable on one machine — a full-length
// name, and a stem truncated to 43 characters with a 7-hex-digit suffix
// (`…-paarthjamdagne-e4e727c`). Reversing it is impossible for the truncated
// form and would be a guess for the rest, so the workspace decision is made
// from paths the agent actually touched, which are absolute and exact.
//
// Returns "" when the line reveals no path — the honest answer for a turn that
// has not used a tool yet. The caller must treat that as UNDECIDED and retry,
// never as a mismatch: caching a "no" on a file whose first records are pure
// prose is the bug that silently dropped whole Codex sessions.
func CursorObservedPath(line []byte) string {
	var rec cursorRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return ""
	}
	for _, item := range rec.Message.Content {
		if item.Type != "tool_use" || len(item.Input) == 0 {
			continue
		}
		var in struct {
			Path            string `json:"path"`
			WorkingDir      string `json:"working_directory"`
			TargetDirectory string `json:"target_directory"`
		}
		if err := json.Unmarshal(item.Input, &in); err != nil {
			continue
		}
		for _, c := range []string{in.WorkingDir, in.TargetDirectory, in.Path} {
			if strings.HasPrefix(c, "/") || cursorHasWindowsDrive(c) {
				return c
			}
		}
	}
	return ""
}

// cursorHasWindowsDrive reports whether p starts with a `C:\`-style drive
// prefix. Checked explicitly rather than via filepath.IsAbs so the decision is
// host-independent: a unix CI runner must classify a Windows transcript the
// same way a Windows machine does, and filepath.IsAbs does not.
func cursorHasWindowsDrive(p string) bool {
	if len(p) < 3 {
		return false
	}
	c := p[0]
	if !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') {
		return false
	}
	return p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// NOTE FOR ANYONE ADDING DEBUG LOGGING HERE: log the tool NAME only, never its
// input. A log line is an egress path like any other, and a Cursor tool input is
// where old_string/new_string/contents live. A helper that formatted exactly
// that used to sit here unused; it was removed rather than left as a loaded
// convenience.
