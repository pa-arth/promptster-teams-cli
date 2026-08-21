package normalize

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Codex instrumentation works by tailing the per-session rollout JSONL that the
// `codex` CLI writes to ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl. Unlike
// Claude Code / Cursor, the codex hooks engine does NOT fire for `codex exec`
// (and is interactive-TUI-gated besides), so the rollout file is the reliable
// capture channel — it is written in every mode and carries prompts, tool
// calls, command output, file patches, assistant messages and token usage.
//
// Each rollout line is one RolloutItem:
//
//	{"timestamp":"...","type":"session_meta|event_msg|response_item|turn_context","payload":{...}}
//
// The payload's own "type" discriminates further (user_message, agent_message,
// function_call, custom_tool_call, patch_apply_end, token_count, ...).

// codexPendingCall holds a tool call awaiting its output line so the two can be
// merged into a single canonical event (mirrors how Claude's PostToolUse
// carries both input and response).
type codexPendingCall struct {
	name string
	args map[string]interface{}
}

type codexRunningCall struct {
	call   codexPendingCall
	callID string
}

// codexRolloutProcessor converts codex rollout JSONL lines into canonical
// Events. It is stateful: function-call lines are correlated with their
// *_output lines by call_id, and the latest token usage is attached to the next
// final assistant message.
type CodexRolloutProcessor struct {
	sessionID      string
	pending        map[string]codexPendingCall
	running        map[string]codexRunningCall
	lastTokenUsage map[string]interface{}
	// lastContextWindow is codex's `token_count.info.model_context_window` — the
	// size of the window the session is running against, restated on every
	// token_count line. Stashed beside lastTokenUsage and attached to the same
	// events, because a context size without its ceiling is not a reading: a
	// peak of 84k means one thing against 200k and another against 258400.
	//
	// It is the VENDOR's number and must stay that way. Codex reports 258400 for
	// gpt-5.5 / gpt-5.6-sol — 272000 net of reserved output — which no model-id
	// lookup table would have produced, and a table has no subscriber to the
	// vendor's next release. 0 means "not reported"; never emitted as 0.
	lastContextWindow int64
	// workdir is the session's cwd, home-collapsed to "~/…", captured from the
	// session_meta header (the ONLY codex rollout line that carries cwd). It is
	// stamped onto each prompt event so the teams dashboard can show where the
	// session ran; the raw absolute cwd is never emitted on prompts.
	workdir string
	// subagentThread records whether this rollout is a DELEGATED thread rather
	// than the one the human types into (see codexIsSubagentThread). Its
	// user_message lines are the orchestrator's instructions to that subagent, not
	// anything a person wrote — and since those events now roll up into the human's
	// session (see CodexConversationID) they must say so, or the fluency judge
	// grades machine-authored text as the engineer's own prompting.
	subagentThread bool
	// subagentName is the delegated agent's NAME, from session_meta.source
	// (see codexSubagentName). "" when the rollout names none — including every
	// rollout of a build that predates the field.
	//
	// It rides subagent_usage as `attributionAgent`, which is the field the
	// backend's spend row is NAMED from and the field its frequency board is
	// KEYED on. Without it every delegated turn in an org collapses into one row
	// literally called "subagent" and the frequency board drops all of them —
	// measured at 0 of 731 on a live customer before this existed.
	subagentName string
	// threadID is this rollout's OWN thread id (session_meta.payload.id, which is
	// also the uuid in the filename). It is no longer the session id — see
	// CodexConversationID — but it is what tells one subagent's spend apart from
	// another's inside the merged session, so it rides subagent_usage as agentId.
	threadID string
	// model is the per-turn model id captured from the LATEST turn_context line
	// (session_meta carries only model_provider — the vendor, e.g. "openai"). It is
	// stamped onto each ai_response so the backend can price the turn against the
	// real model. turn_context precedes the turn's agent messages, so the captured
	// value is always the model in force for the response it lands on.
	model string
	// RepoRoot is the canonical per-session repository identity (a git remote slug
	// owner/name, or a stable opaque hash for a no-remote/non-git dir). Unlike
	// workdir — which normalize derives itself from the payload cwd via
	// HomeRelativeStrict — repoRoot needs `git config` (fs/exec) and so CANNOT be
	// derived here; it is resolved ONCE per session in internal/capture
	// (capture.sessionRepoRoot) and threaded in as session state, then stamped onto
	// each prompt event beside workdir when non-empty. Empty (omitted) when the cwd
	// was gone/unresolvable, mirroring workdir's empty-on-failure.
	RepoRoot string
	// RepoHost is the remote's bare hostname (e.g. "github.com"), resolved and
	// threaded exactly like RepoRoot. The slug alone cannot tell providers apart —
	// gitlab.com/acme/api and github.com/acme/api both reduce to "acme/api" — so
	// the backend needs the host to require a real provider match instead of
	// treating a colliding owner name as one. Non-empty ONLY when RepoRoot is a
	// remote slug; omitted when empty.
	RepoHost string
	// RepoTracked reports whether the resolved cwd was inside a git working tree
	// at all, resolved in the SAME capture-side pass as RepoRoot/RepoHost and
	// threaded in the same way. It exists because the two opaque-hash cases —
	// "git repo with no origin remote" and "not a git repo at all" — emit a
	// structurally identical key, so without it a home directory or a container
	// folder is indistinguishable from a real remoteless repo and occupies a row
	// on a board titled "Top repos".
	//
	// Stamped explicitly as true OR false whenever repoRoot is stamped, omitted
	// entirely when repoRoot is. Absence means "a CLI too old to have looked" and
	// is treated downstream as tracked. Only meaningful alongside a non-empty
	// RepoRoot.
	RepoTracked bool
	// pendingUser holds a human turn recovered from a `response_item`
	// message/role=user line, DEFERRED until the next line tells us whether the
	// authoritative `event_msg`/user_message follows it. See
	// codexUserMessageLine for why the deferral (rather than a flag) is the only
	// shape that works, and recoverUserPrompt for why the fallback exists.
	pendingUser codexPendingUserTurn
}

// codexPendingUserTurn is one buffered response_item user turn awaiting the
// verdict of the following line.
type codexPendingUserTurn struct {
	text string
	ts   string
	raw  string
	// aged records that this turn has already survived one end-of-poll check,
	// i.e. a full poll interval elapsed with no following line. See
	// FlushStaleUserPrompt.
	aged bool
}

func NewCodexRolloutProcessor(sessionID string) *CodexRolloutProcessor {
	return &CodexRolloutProcessor{
		sessionID: sessionID,
		pending:   map[string]codexPendingCall{},
		running:   map[string]codexRunningCall{},
	}
}

// stableEventID derives a deterministic event id from a STABLE per-source key
// scoped by session and kind, so the same rollout record always yields the same
// id no matter how often it is re-observed. Codex resume/fork writes a NEW
// rollout file that copies prior history verbatim, and the watcher runs one
// processor (its own dedup state) per file — so without this a copied line
// re-emits as a byte-identical duplicate carrying a fresh random id the backend
// can't collapse. The stable key is the record's own identity: the session id
// for session_start, the tool call_id for command/tool/mcp/plan/file_diff, and
// the rollout line timestamp for the event_msg-derived prompt/ai_response (Codex
// event_msg lines carry no per-item id — the line timestamp is copied verbatim
// on fork, so it is the record's stable identity). When sourceKey is empty it
// falls back to a random id — never worse than the previous always-random
// behavior. Mirrors ClaudeTranscriptProcessor.stableEventID.
func (p *CodexRolloutProcessor) stableEventID(sourceKey, kind string) string {
	if sourceKey == "" {
		return event.NewUUID()
	}
	return event.DeterministicUUID(p.sessionID + "\x1f" + kind + "\x1f" + sourceKey)
}

// newCodexEvent builds a canonical Event stamped with the rollout line's own
// timestamp (so the replay timeline reflects when things actually happened, not
// when the watcher observed them) and source="codex". Actor is derived from
// kind: prompts are the candidate, session lifecycle is the system, and every
// tool/output event is the agent acting. sourceKey is the stable identity of the
// source record (session id / tool call_id / rollout line ts); pass "" only when
// no stable key exists.
func (p *CodexRolloutProcessor) newCodexEvent(kind, ts, sourceKey string) event.Event {
	e := event.NewEvent(kind, p.sessionID)
	// Overwrite NewEvent's random id with the stable, source-derived one (keeping
	// NewEvent as the single source of Ts/Source/V defaults).
	e.ID = p.stableEventID(sourceKey, kind)
	e.Source = "codex"
	switch kind {
	case "prompt":
		e.Actor = event.HumanActor()
	case "session_start", "session_end":
		e.Actor = event.SystemActor()
	default:
		e.Actor = event.AIActor()
	}
	if t := parseCodexTs(ts); t != "" {
		e.Ts = t
	}
	return e
}

// process parses one rollout line and returns zero or more canonical events.
func (p *CodexRolloutProcessor) Process(line []byte) []event.Event {
	var rec map[string]interface{}
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	typ, _ := rec["type"].(string)
	payload, _ := rec["payload"].(map[string]interface{})
	if payload == nil {
		return nil
	}
	ts, _ := rec["timestamp"].(string)
	raw := strPreview(string(line), 500)

	// Resolve any buffered response_item user turn BEFORE handling this line.
	// Codex writes the response_item copy of a human turn one line AHEAD of the
	// authoritative event_msg (verified 25/25 on real rollouts, always at line N
	// then N+1), so "have I seen an event_msg yet?" can never be answered at the
	// moment the response_item arrives — the answer is always "not yet". Holding
	// it for exactly one line and discarding it the instant the event_msg lands
	// is what makes this fallback a NO-OP for every rollout that emits event_msg.
	var recovered []event.Event
	if p.pendingUser.text != "" && !p.codexLineClaimsPendingTurn(typ, payload) {
		recovered = p.flushPendingUserPrompt()
	}

	switch typ {
	case "session_meta":
		return append(recovered, p.sessionMeta(payload, ts, raw)...)
	case "event_msg":
		return append(recovered, p.eventMsg(payload, ts, raw)...)
	case "response_item":
		return append(recovered, p.responseItem(payload, ts, raw)...)
	case "turn_context":
		// turn_context carries the per-turn model (the only rollout line that does);
		// stash it for the turn's ai_response. Assigned UNCONDITIONALLY: a
		// turn_context that declares no model clears any prior value rather than
		// carrying it forward, so the next ai_response omits model instead of pricing
		// against a stale one (never attribute a model we do not currently know). A
		// turn with no turn_context at all leaves the last value untouched — the model
		// genuinely persists until the next turn_context changes it. Emits no event.
		p.model = stringField(payload, "model")
		return recovered
	default:
		// Unknown wrappers carry no candidate-visible signal.
		return recovered
	}
}

// codexUserMessageLine reports whether a rollout line is the authoritative
// `event_msg`/user_message record of a human turn — the one record whose arrival
// means a buffered response_item copy must be thrown away rather than emitted.
func codexUserMessageLine(typ string, payload map[string]interface{}) bool {
	return typ == "event_msg" && stringField(payload, "type") == "user_message"
}

// codexLineClaimsPendingTurn reports whether this line belongs to the SAME human
// turn already buffered — in which case the buffer must be kept (or replaced),
// never flushed, because flushing would mint a second prompt for one turn.
//
// Two records claim it:
//
//  1. The authoritative `event_msg`/user_message. Unconditional, regardless of
//     text: it is adjacent by construction and is the record the fallback defers
//     to. This is the leg that makes the fallback a no-op on hosts that emit it.
//
//  2. ANOTHER `response_item` user message carrying the SAME text. This is the
//     regression fixed here. A live org's rollouts repeat the user turn on that
//     channel — three copies within a millisecond for one turn — and the plain
//     "the next line isn't an event_msg, so the turn moved on" rule fired on each
//     repeat, minting one prompt per copy. Verified on the customer's live data:
//     zero millisecond-apart duplicate prompts in 3,291 prompts before, 38 in the
//     405 captured after, codex only.
//
//     No local rollout reproduced it, and the reason is worth keeping: the only
//     repeated response_item they contain is `<environment_context>`, which the
//     synthetic drop-list removes before it ever reaches the buffer. So the
//     replay-real-rollouts differential that proved the fallback safe was blind
//     to this shape BY CONSTRUCTION — the same class of blind spot as writing a
//     harness drop-list against one vendor's syntax.
//
// Text is compared trimmed, matching how recoverUserPrompt stores it. A repeat
// with DIFFERENT text is a genuinely distinct turn and still flushes.
func (p *CodexRolloutProcessor) codexLineClaimsPendingTurn(typ string, payload map[string]interface{}) bool {
	if codexUserMessageLine(typ, payload) {
		return true
	}
	if typ != "response_item" || stringField(payload, "type") != "message" {
		return false
	}
	if stringField(payload, "role") != "user" {
		return false
	}
	return strings.TrimSpace(codexUserContentText(payload["content"])) == p.pendingUser.text
}

// codexSyntheticUserPrefixes are the wrappers Codex itself writes into the
// `response_item` user channel. Nobody typed them, and unlike the event_msg
// channel — which carries only what a person actually sent — this channel mixes
// them in with real turns, so recovering prompts from it REQUIRES this list.
//
// Measured across every local rollout: `<environment_context>` (the cwd/shell
// header, one per session), `<recommended_plugins>`, `<user_instructions>`, and
// the `# AGENTS.md instructions for <path>` project-instruction injection.
// Prefix-anchored for the same reason the backend's NON_HUMAN_PREFIXES is: a
// human prompt that merely QUOTES one of these is still a human prompt.
var codexSyntheticUserPrefixes = []string{
	"<environment_context",
	"<user_instructions",
	"<recommended_plugins",
	"# AGENTS.md instructions for",
}

// recoverUserPrompt buffers a `response_item` message/role=user line as a
// candidate human turn.
//
// WHY THIS EXISTS. The normalizer has always taken human turns from
// `event_msg`/user_message and skipped this channel as a duplicate. On
// 2026-07-30 the first Codex customer's org showed ZERO human prompts across 29
// sessions and a full day of work — 63 stored prompts, 60 of them Codex
// auto-review scaffolding and 3 empty — while the same sessions captured 1,400+
// tool calls and 400+ file diffs. raw_events proved the backend was never sent
// one, and the watcher (tails from offset 0) and the on-device projector (which
// explicitly allows prompt.text) were both ruled out, leaving this normalizer as
// the only place the turn could go missing. Their rollouts evidently do not
// carry `event_msg`/user_message, though every local rollout does — so the exact
// upstream trigger (Codex build, IDE/exec host, config) is NOT established.
//
// This fallback is therefore written to be correct WITHOUT knowing that trigger:
// the response_item copy carries the same human text (verified — it matches the
// event_msg text on every local rollout), so taking it only when the event_msg
// never arrives recovers the turn for any host that omits the event_msg, and
// changes nothing for any host that emits one.
//
// Subagent threads are excluded here for the same reason their event_msg
// user_message is dropped: that text is the orchestrator's instruction to a
// delegated agent, not something a person wrote.
func (p *CodexRolloutProcessor) recoverUserPrompt(payload map[string]interface{}, ts, raw string) {
	if p.subagentThread {
		return
	}
	text := strings.TrimSpace(codexUserContentText(payload["content"]))
	if text == "" {
		return
	}
	for _, prefix := range codexSyntheticUserPrefixes {
		if strings.HasPrefix(text, prefix) {
			return
		}
	}
	// A same-text repeat is the SAME turn arriving again, so it must not restart
	// the staleness clock: resetting `aged` would push the end-of-poll flush out
	// by another poll for every repeat that straddles a poll boundary, and a turn
	// whose repeats keep landing one per poll would never flush at all.
	aged := p.pendingUser.aged && p.pendingUser.text == text
	p.pendingUser = codexPendingUserTurn{text: text, ts: ts, raw: raw, aged: aged}
}

// FlushStaleUserPrompt releases a buffered turn that the next line will never
// come for — the last human prompt of a rollout nothing is appended to again
// (the user quit right after typing, or the agent died before answering).
//
// Process only ever flushes on the FOLLOWING line, so without this the final
// turn of such a rollout stays buffered forever. That matters most for exactly
// the customer this fallback exists for, where the recovered turn is the ONLY
// prompt source.
//
// TWO polls, not one, and that is the whole design. Flushing at the end of the
// first poll would race the authoritative record: a poll can land in the gap
// between the response_item and the event_msg that Codex writes immediately
// after it, and emitting there would mint the prompt AND then let the event_msg
// mint it again next poll. Requiring the buffer to survive a full poll interval
// — seconds, against two adjacent lines written in the same instant — makes that
// race unreachable while still bounding the delay to one extra poll.
func (p *CodexRolloutProcessor) FlushStaleUserPrompt() []event.Event {
	if p.pendingUser.text == "" {
		return nil
	}
	if !p.pendingUser.aged {
		p.pendingUser.aged = true
		return nil
	}
	return p.flushPendingUserPrompt()
}

// flushPendingUserPrompt mints the buffered turn as a prompt and clears it.
func (p *CodexRolloutProcessor) flushPendingUserPrompt() []event.Event {
	turn := p.pendingUser
	p.pendingUser = codexPendingUserTurn{}
	return p.newPromptEvent(turn.text, turn.ts, turn.raw)
}

// codexUserContentText joins the text parts of a response_item message's
// content array. Non-text parts (images, attachments) contribute nothing, so a
// turn made only of them yields "" and is correctly not treated as a prompt.
func codexUserContentText(content interface{}) string {
	parts, ok := content.([]interface{})
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		m, ok := part.(map[string]interface{})
		if !ok {
			continue
		}
		if s := stringField(m, "text"); s != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(s)
		}
	}
	return b.String()
}

// CodexConversationID resolves the LOGICAL conversation a rollout file belongs
// to, from its session_meta payload.
//
// One rollout file is one Codex THREAD, and modern Codex (>= 0.145) splits ONE
// conversation across SEVERAL threads: the user's thread plus one file per
// subagent it delegates to. session_meta carries all three ids:
//
//	id                -> this thread's own id — and the uuid in the FILENAME
//	session_id        -> the ROOT conversation the thread belongs to
//	parent_thread_id  -> the immediate parent (present on subagent threads only)
//
// The watcher seeds each processor from the filename, i.e. from `id`, so every
// subagent thread used to mint a session of its own: the user's thread carried
// the prompts (and therefore workdir/repoRoot, which ride only on prompts) while
// the delegated threads carried the file_diff/tool_use work, and no session held
// both. Prefer session_id, then the parent, then the thread's own id. Rollouts
// from older Codex builds carry only `id`, so they resolve exactly as before.
//
// This mirrors claudeSessionIDFromPath's subagent roll-up on the Claude side —
// same failure, same contract: delegated work belongs to the session that
// spawned it.
func CodexConversationID(payload map[string]interface{}) string {
	if s := stringField(payload, "session_id"); s != "" {
		return s
	}
	if s := stringField(payload, "parent_thread_id"); s != "" {
		return s
	}
	return stringField(payload, "id")
}

// codexIsSubagentThread reports whether this rollout is a delegated thread
// rather than the one the human is typing into. `thread_source` is the vendor's
// own marker; the id comparison is the structural backstop for builds that carry
// the parentage without the enum.
func codexIsSubagentThread(payload map[string]interface{}) bool {
	if stringField(payload, "thread_source") == "subagent" {
		return true
	}
	parent := stringField(payload, "parent_thread_id")
	return parent != "" && parent != stringField(payload, "id")
}

// codexSubagentName recovers the delegated agent's NAME from session_meta, or ""
// when the rollout does not name one.
//
// Codex writes `source` as a tagged union: a bare string for an ordinary thread
// ("cli", "exec") and an object for a delegated one —
// `{"subagent":{"other":"guardian"}}`, captured live on codex-cli 0.146.0.
//
// THE INNER KEY IS AN OPEN SET. `other` is evidently the fallback variant for a
// user-defined agent, which means named variants exist that this build has never
// seen. So the value of whichever single string the object holds is the name; an
// unrecognized key yields the name it carries rather than a rejection. Switching
// on `other` would silently start dropping names the day Codex ships a second
// variant, which is the failure this whole change exists to fix.
//
// A shape we cannot read yields "" — the field is then omitted downstream, never
// defaulted to a placeholder. "We do not know which agent" and "an agent called
// subagent" are opposite claims, and the second one is what the boards render.
func codexSubagentName(payload map[string]interface{}) string {
	source, ok := payload["source"].(map[string]interface{})
	if !ok {
		return ""
	}
	inner, ok := source["subagent"].(map[string]interface{})
	if !ok {
		return ""
	}
	for _, v := range inner {
		if s, ok := v.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

func (p *CodexRolloutProcessor) sessionMeta(payload map[string]interface{}, ts, raw string) []event.Event {
	// session_meta is line 1 of every rollout, so this runs before any event is
	// minted — including on the replay-the-consumed-prefix path a restarted
	// watcher takes.
	//
	// A conversation root that differs from this thread's own id OVERRIDES the
	// filename-derived seed: the filename only ever carries the thread id, and
	// nothing but session_meta knows which conversation that thread belongs to.
	// When the rollout names no separate root (every Codex build before 0.145)
	// the seed stands, and session_meta is the fallback exactly as before — so
	// events are never stamped "unknown" and pooled into a shared cross-session
	// chain.
	conv := CodexConversationID(payload)
	switch {
	case conv != "" && conv != stringField(payload, "id"):
		p.sessionID = conv
	case p.sessionID == "":
		p.sessionID = conv
	}
	p.subagentThread = codexIsSubagentThread(payload)
	p.subagentName = codexSubagentName(payload)
	p.threadID = stringField(payload, "id")
	// Stash the home-collapsed cwd for prompt events: session_meta is the only
	// rollout line carrying cwd, and it precedes every prompt. HomeRelativeStrict
	// emits ONLY a provably home-relative ("~"-prefixed) value — an outside-home
	// cwd or a home-lookup failure yields "", so the prompt omits workdir rather
	// than leaking an absolute path that may carry the OS username.
	p.workdir = state.HomeRelativeStrict(stringField(payload, "cwd"))
	// Key session_start off the CONVERSATION, not the thread: the parent and each
	// of its subagents open with their own session_meta, and one logical session
	// must begin exactly once. Same conversation => same deterministic id => the
	// backend collapses the duplicates instead of stacking N starts on one session.
	e := p.newCodexEvent("session_start", ts, p.sessionID)
	data := map[string]interface{}{
		"ideSessionId": p.sessionID,
		"cwd":          stringField(payload, "cwd"),
		"source":       stringField(payload, "originator"),
		"cliVersion":   stringField(payload, "cli_version"),
		"model":        stringField(payload, "model_provider"),
	}
	e.Data = data
	e.RawPayload = raw
	return []event.Event{e}
}

// newPromptEvent mints a human prompt event and stamps the session metadata
// that rides only on prompts. Shared by BOTH human-turn sources — the
// authoritative event_msg and the recovered response_item — so a session whose
// prompts came via the fallback still carries workdir/repoRoot/repoHost and
// still joins to outcome_events exactly like any other.
//
// idSeed is the rollout line's timestamp: neither channel carries a per-item id,
// and the line ts is the record's stable identity (copied verbatim on
// resume/fork).
func (p *CodexRolloutProcessor) newPromptEvent(text, ts, raw string) []event.Event {
	e := p.newCodexEvent("prompt", ts, ts)
	e.Provenance = event.HumanProvenance()
	data := map[string]interface{}{"text": text}
	// workdir rides its own allowlisted key (never raw cwd); set only when the
	// session_meta header supplied one.
	if p.workdir != "" {
		data["workdir"] = p.workdir
	}
	// repoRoot — the canonical repo identity resolved in capture and threaded in;
	// stamped only when non-empty (mirrors workdir). It de-fragments the repo
	// across subdirs/worktrees and joins exactly to outcome_events.repo.
	if p.RepoRoot != "" {
		data["repoRoot"] = p.RepoRoot
		// repoTracked — whether that key came from a real git working tree or from
		// a directory that is not a repo at all. Stamped EXPLICITLY as true or
		// false, and only ever beside repoRoot: omitting it when false would make
		// "not a repo" indistinguishable from "CLI too old to have looked".
		data["repoTracked"] = p.RepoTracked
	}
	// repoHost — the provider the slug came from, so the backend can tell
	// gitlab.com/acme/api apart from github.com/acme/api. Same omit-when-empty
	// rule; empty is the honest answer for a repo with no remote.
	if p.RepoHost != "" {
		data["repoHost"] = p.RepoHost
	}
	e.Data = data
	e.RawPayload = raw
	saveLastPromptTs()
	return []event.Event{e}
}

func (p *CodexRolloutProcessor) eventMsg(payload map[string]interface{}, ts, raw string) []event.Event {
	switch stringField(payload, "type") {
	case "user_message":
		// A DELEGATED thread's user_message is the orchestrator's instruction to a
		// subagent — machine-authored, never something a person typed. The Claude
		// path drops sidechain "user" lines for exactly this reason ("their 'user'
		// prompts are authored by the parent agent, not the human"), and it matters
		// more here now that the thread rolls up into the human's real session
		// instead of a phantom one of its own: emitted, they would inflate the
		// engineer's prompt count with text the fluency judge then grades as their
		// own prompting. The turn's cost is kept via subagentUsage below.
		if p.subagentThread {
			return nil
		}
		// The authoritative record for this turn has arrived, so any response_item
		// copy buffered from the preceding line is a duplicate — drop it. This is
		// the line that keeps recoverUserPrompt a no-op on hosts that emit
		// event_msg, i.e. every rollout we can observe locally.
		p.pendingUser = codexPendingUserTurn{}
		// event_msg lines carry no per-item id; the rollout line ts is the
		// record's stable identity (copied verbatim on resume/fork).
		return p.newPromptEvent(stringField(payload, "message"), ts, raw)

	case "agent_message":
		// Codex emits multiple agent_message lines per turn: "commentary" (interim
		// narration) and "final_answer". Only the final answer is the turn-end
		// assistant message analogous to Claude's Stop.
		if stringField(payload, "phase") != "final_answer" {
			return nil
		}
		// A delegated thread's answer is written to its orchestrator, not to the
		// human, and its turn is not the human's turn. Keep the SPEND (it is real
		// and belongs to the session that spawned the subagent) and drop the prose
		// and the turn timing — the same split the Claude path makes.
		if p.subagentThread {
			return p.subagentUsage(ts, raw)
		}
		// Same as the prompt: no per-item id on the event_msg line, so key off
		// the (fork-stable) rollout line ts.
		e := p.newCodexEvent("ai_response", ts, ts)
		data := map[string]interface{}{
			"lastAssistantMessage": stringField(payload, "message"),
		}
		if p.model != "" {
			data["model"] = p.model
		}
		p.attachTokenUsage(data)
		if last := loadLastPromptTs(); !last.IsZero() {
			data["turnDurationMs"] = time.Since(last).Milliseconds()
		}
		e.Data = data
		e.RawPayload = raw
		return []event.Event{e}

	case "patch_apply_end":
		return p.patchApplyEnd(payload, ts, raw)

	case "mcp_tool_call_end":
		return p.mcpToolCall(payload, ts)

	case "token_count":
		// Stash the latest usage; attached to the next final assistant message.
		if info, ok := payload["info"].(map[string]interface{}); ok {
			if usage, ok := info["total_token_usage"].(map[string]interface{}); ok {
				p.lastTokenUsage = usage
			}
			// Tracked independently of total_token_usage: the two are separate
			// keys on `info` and a line can carry either without the other.
			// Latest-wins rather than sticky — a mid-session model switch moves
			// the window, and the last line before a turn is the one that turn
			// ran against.
			if w := intField(info, "model_context_window"); w > 0 {
				p.lastContextWindow = w
			}
		}
		return nil

	default:
		return nil
	}
}

// subagentUsage emits a delegated thread's completed turn as COUNTERS ONLY.
// The spend is real and belongs to the conversation that spawned the subagent,
// so it must survive the roll-up; the agent prose is not the assistant's answer
// to the human and must never enter the timeline. Mirrors
// ClaudeTranscriptProcessor.flushSidechain, including the `sidechain` marker and
// the agentId that keeps two concurrent subagents' spend distinguishable.
//
// usageScope is deliberately left unset, exactly as the codex ai_response path
// leaves it: Codex reports total_token_usage as a running SESSION total, not a
// per-request delta, and inventing a scope token here would assert otherwise.
func (p *CodexRolloutProcessor) subagentUsage(ts, raw string) []event.Event {
	_ = raw // the wrapper line is agent prose; deliberately not retained
	// Scope the stable key by THREAD as well as line ts: sibling subagents run
	// concurrently inside one conversation and can finish a turn in the same
	// millisecond, and a bare-ts key would collapse one of them into the other.
	e := p.newCodexEvent("subagent_usage", ts, p.threadID+"\x1f"+ts)
	e.Provenance = event.AIProvenance()
	data := map[string]interface{}{"sidechain": true}
	if p.model != "" {
		data["model"] = p.model
	}
	p.attachTokenUsage(data)
	if p.threadID != "" {
		data["agentId"] = p.threadID
	}
	// The agent's NAME, when session_meta gave one. Omitted (not blanked) when it
	// did not: the backend reads absent as "unnamed" and names the span
	// `attributionSkill ?? attributionAgent ?? "subagent"`, so an empty string
	// here would be a name rather than the absence of one.
	if p.subagentName != "" {
		data["attributionAgent"] = p.subagentName
	}
	e.Data = data
	e.RawPayload = "codex subagent usage"
	return []event.Event{e}
}

// mcpToolCall emits an mcp_call from Codex's `mcp_tool_call_end` event_msg.
//
// THIS IS THE ONLY PLACE MCP IDENTITY APPEARS ON THE CODEX WIRE. The model
// reaches an MCP tool through the `exec` JavaScript wrapper
// (`ALL_TOOLS.find(x => x.name.includes("probe_echo"))`), so there is no
// `server__tool` function name anywhere in the rollout — which is why the old
// name-shaped detector (`strings.Contains(name, "__")`) could never fire and a
// live customer showed exactly 0 mcp_call events over 30 days. Zero from a
// detector that cannot fire is unmeasured, not unused.
//
// The server rides INSIDE the tool name as "<server>__<tool>". mcp_call's field
// allowlist is {name, tool, status} on BOTH default-deny rails (the CLI's
// redact/project.go and the backend's captureAllowlist.ts) — there is no
// `server` field, and adding one means editing both lists or the value is
// stripped silently. It does not need adding: the backend's mcpServerOf() already
// splits an mcp_call's tool name on the first "__" and returns the left side as
// the server, code the Claude rail already exercises.
//
// NOT READ, and this is the point of the function rather than an aside:
// `invocation.arguments` is whatever the engineer passed the tool and
// `result.content` is the tool's full output. Both are the same class as the
// codex.tool_result arguments/output the OTel translator refuses whole. Only the
// two names are read, and the record is never placed in RawPayload.
func (p *CodexRolloutProcessor) mcpToolCall(payload map[string]interface{}, ts string) []event.Event {
	invocation, ok := payload["invocation"].(map[string]interface{})
	if !ok {
		return nil
	}
	server := strings.TrimSpace(stringField(invocation, "server"))
	tool := strings.TrimSpace(stringField(invocation, "tool"))
	if server == "" || tool == "" {
		return nil
	}
	// call_id is the vendor's stable identity for this call; fall back to the
	// line ts so a build that omits it still mints a deterministic id.
	seed := stringField(payload, "call_id")
	if seed == "" {
		seed = ts
	}
	e := p.newCodexEvent("mcp_call", ts, seed)
	e.Provenance = event.AIProvenance()
	e.Data = map[string]interface{}{
		"tool":   server + "__" + tool,
		"status": codexMCPStatus(payload),
	}
	// RawPayload deliberately unset: the record holds both the call arguments and
	// the tool's output.
	return []event.Event{e}
}

// codexMCPStatus reads the call's outcome. Codex wraps the result in a Rust-style
// tagged union — {"Ok":…} or {"Err":…} — so presence of `Ok` is the verdict.
// An unreadable result yields "error": a call whose outcome we cannot establish
// is not evidence that it worked.
func codexMCPStatus(payload map[string]interface{}) string {
	result, ok := payload["result"].(map[string]interface{})
	if !ok {
		return "error"
	}
	if _, ok := result["Ok"]; ok {
		return "ok"
	}
	return "error"
}

// patchApplyEnd emits one file_diff per changed file. The payload carries a
// ready-made unified_diff per path, plus the change type (add/update/delete),
// so no apply-patch envelope parsing is needed.
func (p *CodexRolloutProcessor) patchApplyEnd(payload map[string]interface{}, ts, raw string) []event.Event {
	changes, ok := payload["changes"].(map[string]interface{})
	if !ok || len(changes) == 0 {
		return nil
	}
	// call_id is the apply_patch call's stable identity; one patch_apply_end
	// emits one file_diff per path, so scope the key by path too.
	callID := stringField(payload, "call_id")
	var events []event.Event
	for path, rawChange := range changes {
		change, _ := rawChange.(map[string]interface{})
		if change == nil {
			continue
		}
		diff := stringField(change, "unified_diff")
		added, removed := countDiffLines(diff)
		e := p.newCodexEvent("file_diff", ts, callID+"\x1f"+path)
		e.Provenance = event.AIProvenance()
		data := map[string]interface{}{
			"path":         path,
			"diff":         diff,
			"linesAdded":   added,
			"linesRemoved": removed,
			"attribution":  "likely_ai",
			"changeType":   stringField(change, "type"),
		}
		if ranges := lineRangesFromUnifiedDiff(diff, e.Provenance.Attribution); len(ranges) > 0 {
			data["lineRanges"] = ranges
		}
		if mv := stringField(change, "move_path"); mv != "" {
			data["movePath"] = mv
		}
		e.Data = data
		e.RawPayload = strPreview(diff, 500)
		events = append(events, e)
	}
	return events
}

func (p *CodexRolloutProcessor) responseItem(payload map[string]interface{}, ts, raw string) []event.Event {
	switch stringField(payload, "type") {
	case "function_call", "custom_tool_call":
		name := stringField(payload, "name")
		args := parseCodexArgs(payload)
		// Newer Codex hosts expose one generic custom tool named `exec`. Its input
		// is a small JavaScript orchestration program which calls the real tool
		// (`tools.exec_command`, `tools.update_plan`, `tools.apply_patch`, ...).
		// Unwrap the tool identity here so shell/planning/edit telemetry does not
		// collapse into an empty generic tool_use event. Direct codex-cli calls are
		// left unchanged for backwards compatibility.
		if name == "exec" {
			name, args = unwrapCodexExec(args)
		}
		// apply_patch is reported via the richer event_msg/patch_apply_end; skip
		// the call line so we don't double-count file edits.
		if name == "apply_patch" {
			return nil
		}
		callID := stringField(payload, "call_id")
		if callID != "" {
			p.pending[callID] = codexPendingCall{name: name, args: args}
		}
		return nil

	case "function_call_output", "custom_tool_call_output":
		callID := stringField(payload, "call_id")
		call, ok := p.pending[callID]
		if !ok {
			return nil
		}
		delete(p.pending, callID)
		output := codexOutputText(payload["output"])
		// Current hosts detach long-running exec_command calls and complete them
		// through one or more write_stdin calls. Hold the original command until
		// completion so we never report the handoff as a false exitCode=0, and use
		// the original call id so replay remains deterministic.
		if isCodexShellTool(call.name) {
			if cellID := codexRunningCellID(output); cellID != "" {
				p.running[cellID] = codexRunningCall{call: call, callID: callID}
				return nil
			}
		}
		if call.name == "write_stdin" {
			cellID := stringField(call.args, "session_id")
			if running, ok := p.running[cellID]; ok {
				if nextID := codexRunningCellID(output); nextID != "" {
					if nextID != cellID {
						delete(p.running, cellID)
						p.running[nextID] = running
					}
					return nil
				}
				delete(p.running, cellID)
				return p.emitToolEvent(running.call, running.callID, output, ts, raw)
			}
		}
		return p.emitToolEvent(call, callID, output, ts, raw)

	case "message":
		// A user message here is the response_item COPY of a human turn. It is
		// normally a duplicate of the event_msg that follows on the next line and is
		// discarded there; it becomes the prompt only when that event_msg never
		// arrives. Assistant/developer messages carry no signal this channel owns.
		if stringField(payload, "role") == "user" {
			p.recoverUserPrompt(payload, ts, raw)
		}
		return nil

	default:
		// reasoning context items duplicate event_msg signal — skip.
		return nil
	}
}

// emitToolEvent converts a completed tool call (call + output) into the right
// canonical event, branching on the codex tool name. callID is the tool call's
// stable identity (the OpenAI call_id, copied verbatim on resume/fork), used to
// derive a deterministic event id — mirrors how the Claude path keys off
// tool_use_id.
func (p *CodexRolloutProcessor) emitToolEvent(call codexPendingCall, callID, output, ts, raw string) []event.Event {
	switch {
	case call.name == "wrapped_apply_patch":
		return p.emitWrappedPatch(call, callID, ts, raw)

	case isCodexShellTool(call.name):
		cmd := codexCommandString(call.args)
		// The backend's canonical command schema requires a non-empty invocation.
		// Current exec wrappers occasionally build arguments indirectly, where the
		// narrow non-evaluating extractor cannot recover cmd safely. Preserve the
		// tool occurrence without fabricating an empty command or retaining wrapper
		// source; a future parser can widen this only with a pinned safe shape.
		if strings.TrimSpace(cmd) == "" {
			e := p.newCodexEvent("tool_use", ts, callID)
			e.Data = map[string]interface{}{
				"tool":   call.name,
				"status": codexToolStatus(output),
			}
			e.RawPayload = raw
			return []event.Event{e}
		}
		exitCode, stdout := parseCodexExecOutput(output)
		e := p.newCodexEvent("command", ts, callID)
		e.Provenance = event.AIProvenance()
		e.Data = map[string]interface{}{
			"command":  cmd,
			"exitCode": exitCode,
			"stdout":   stdout,
		}
		e.RawPayload = raw
		events := []event.Event{e}
		// A Codex skill invocation IS this shell command: Codex has no skill tool
		// and reaches for a skill by reading its own SKILL.md (verified live on
		// 0.146.0 — `sed -n '1,240p' <root>/skills/<slug>/SKILL.md`). The sibling
		// event carries the identity; the command event is emitted unchanged
		// because other consumers count it.
		if slug := codexSkillSlugFromCommand(cmd); slug != "" {
			s := p.newCodexEvent("tool_use", ts, callID+"\x1fskill")
			s.Provenance = event.AIProvenance()
			s.Data = map[string]interface{}{
				"tool":   "skill",
				"skill":  slug,
				"status": codexToolStatus(output),
			}
			// No RawPayload: the wrapper source and the file's contents both ride
			// the command event's, and this event needs neither.
			events = append(events, s)
		}
		return events

	case call.name == "update_plan":
		e := p.newCodexEvent("planning", ts, callID)
		data := map[string]interface{}{}
		// Codex carries the plan steps under "plan" (array of {step,status}).
		if plan, ok := call.args["plan"]; ok {
			data["todos"] = plan
		} else if steps, ok := call.args["steps"]; ok {
			data["todos"] = steps
		}
		e.Data = data
		e.RawPayload = raw
		return []event.Event{e}

	// NO name-shaped MCP branch here, deliberately. It used to read
	// `strings.Contains(call.name, "__")` on the theory that Codex namespaces MCP
	// tools as `server__tool`. It does not: the model reaches MCP through the
	// `exec` JS wrapper, so no MCP tool name ever appears in a function_call /
	// custom_tool_call. The branch was unreachable and reported the same zero a
	// customer with no MCP servers would — which is exactly how it was read.
	// MCP now arrives on its own event_msg; see mcpToolCall.

	default:
		e := p.newCodexEvent("tool_use", ts, callID)
		e.Data = map[string]interface{}{
			"tool":   call.name,
			"status": codexToolStatus(output),
		}
		e.RawPayload = raw
		return []event.Event{e}
	}
}

func (p *CodexRolloutProcessor) attachTokenUsage(data map[string]interface{}) {
	u := p.lastTokenUsage
	if u == nil {
		return
	}
	input := intField(u, "input_tokens")
	output := intField(u, "output_tokens")
	cacheRead := intField(u, "cached_input_tokens")
	if input > 0 || output > 0 {
		data["inputTokens"] = input
		data["outputTokens"] = output
		data["cacheReadTokens"] = cacheRead
		// reasoning_output_tokens is OpenAI-only and absent on non-reasoning
		// turns. Attach reasoningTokens ONLY when the provider actually reported
		// it — emitting 0 for an unreported count would conflate "no reasoning
		// data" with "reported zero," the same fabrication the attribution
		// buckets refuse. Absent-by-omission is the honest signal.
		if _, ok := u["reasoning_output_tokens"].(float64); ok {
			data["reasoningTokens"] = intField(u, "reasoning_output_tokens")
		}
		// cache_write_input_tokens — prompt tokens written to cache. Free on every
		// OpenAI model before the GPT-5.6 GA (2026-07-09) and billed at 1.25x the
		// uncached input rate from 5.6 onward, so a turn we do not capture is a
		// turn the backend prices at $0. Discarding it was correct while the fee
		// did not exist and became an under-count the day it did.
		//
		// Emitted under its OWN key, never folded into cacheWriteTokens: that key
		// is the Anthropic ADDEND beside inputTokens, while this count is a SUBSET
		// of Codex's input. Same inversion as cached_input vs cacheReadTokens.
		//
		// Absent-by-omission, like reasoningTokens above — and here the absence
		// carries information the value cannot. Every record of the pre-5.6 corpus
		// reported this field as 0, which is why subset-vs-addend was never
		// settled; a real nonzero row is the first evidence either way, and a
		// fabricated 0 on a provider that never reported one would look exactly
		// like that evidence.
		// TWO spellings accepted, because the rollout name is an INFERENCE and a
		// wrong guess here fails silently in the worst direction: the key simply
		// never appears and reads as "the vendor doesn't send it," which is
		// indistinguishable from the real pre-5.6 zeros. `cache_write_input_tokens`
		// follows the rollout's own convention (OTel `cached_input` arrives here as
		// `cached_input_tokens`); `cache_write_tokens` is the name OpenAI's caching
		// guide uses for the Responses API. Delete whichever one a real 5.6 rollout
		// disproves — do not leave both standing once the answer is observed.
		for _, k := range []string{"cache_write_input_tokens", "cache_write_tokens"} {
			if _, ok := u[k].(float64); ok {
				data["cacheWriteInputTokens"] = intField(u, k)
				break
			}
		}
	}
	// OUTSIDE the input>0||output>0 gate on purpose. The window is a property of
	// the SESSION, not of this turn's counters: a turn whose usage is missing or
	// unparseable still ran against a real ceiling, and dropping the ceiling with
	// the counters would blind the denominator exactly where the numerator is
	// already weakest.
	//
	// Absent, never 0. A 0 window is not a small window — downstream it is either
	// a division by zero or a session reported as 100% full.
	if p.lastContextWindow > 0 {
		data["contextWindowTokens"] = p.lastContextWindow
	}
}

// --- helpers ---------------------------------------------------------------

func isCodexShellTool(name string) bool {
	switch name {
	case "exec_command", "shell", "local_shell", "local_shell_call", "container.exec", "unified_exec":
		return true
	}
	return false
}

// unwrapCodexExec recovers the real tool identity from the JavaScript wrapper
// used by current Codex hosts. Only narrowly-recognized tool calls are lifted;
// unknown programs remain generic `exec` tool_use events. The wrapper source is
// never emitted. For shell commands we recover only the cmd string (which the
// source-exclusion projector subsequently applies its inline-code scrub to).
func unwrapCodexExec(args map[string]interface{}) (string, map[string]interface{}) {
	input := stringField(args, "input")
	switch {
	case strings.Contains(input, "tools.exec_command("):
		out := map[string]interface{}{}
		if cmd := extractJSObjectStringField(input, "cmd"); cmd != "" {
			out["cmd"] = cmd
		}
		return "exec_command", out
	case strings.Contains(input, "tools.update_plan("):
		return "update_plan", map[string]interface{}{}
	case strings.Contains(input, "tools.apply_patch("):
		out := map[string]interface{}{}
		if patch := extractJSCallStringArg(input, "tools.apply_patch"); patch != "" {
			out["patch"] = patch
		}
		// Keep this distinct from a direct apply_patch call: direct codex-cli also
		// emits patch_apply_end and is skipped above, while the wrapper has no such
		// companion record and must derive content-free file metadata itself.
		return "wrapped_apply_patch", out
	case strings.Contains(input, "tools.write_stdin("):
		out := map[string]interface{}{}
		if id := extractJSNumericField(input, "session_id"); id != "" {
			out["session_id"] = id
		}
		return "write_stdin", out
	default:
		return "exec", map[string]interface{}{}
	}
}

var jsCmdFieldRe = regexp.MustCompile(`(?s)\bcmd\s*:\s*("(?:\\.|[^"\\])*")`)

func extractJSNumericField(input, field string) string {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(field) + `\s*:\s*(\d+)`)
	m := re.FindStringSubmatch(input)
	if m == nil {
		return ""
	}
	return m[1]
}

func extractJSObjectStringField(input, field string) string {
	if field != "cmd" { // keep the parser deliberately narrow
		return ""
	}
	m := jsCmdFieldRe.FindStringSubmatch(input)
	if m == nil {
		return ""
	}
	s, ok := decodeJSDoubleQuoted(m[1])
	if !ok {
		return ""
	}
	return s
}

// extractJSCallStringArg extracts a JSON-style double-quoted first argument.
// Codex serializes the wrapper this way today. Refuse other JavaScript syntax
// rather than attempting to evaluate it or accidentally retaining source.
func extractJSCallStringArg(input, callee string) string {
	start := strings.Index(input, callee+"(")
	if start < 0 {
		return ""
	}
	rest := strings.TrimSpace(input[start+len(callee)+1:])
	if rest == "" {
		return ""
	}
	if rest[0] != '"' {
		// functions.exec commonly assigns a large patch to a local first:
		//   const patch = "..."; await tools.apply_patch(patch)
		// Resolve only a plain identifier bound to a double-quoted literal. Never
		// evaluate expressions or template strings.
		end := 0
		for end < len(rest) && ((rest[end] >= 'a' && rest[end] <= 'z') ||
			(rest[end] >= 'A' && rest[end] <= 'Z') ||
			(rest[end] >= '0' && rest[end] <= '9') || rest[end] == '_') {
			end++
		}
		if end == 0 {
			return ""
		}
		name := rest[:end]
		for _, decl := range []string{"const ", "let ", "var "} {
			assign := decl + name
			idx := strings.Index(input, assign)
			if idx < 0 {
				continue
			}
			value := strings.TrimSpace(input[idx+len(assign):])
			if !strings.HasPrefix(value, "=") {
				continue
			}
			rest = strings.TrimSpace(strings.TrimPrefix(value, "="))
			break
		}
	}
	if rest == "" || rest[0] != '"' {
		return ""
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] != '"' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && rest[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			continue
		}
		s, ok := decodeJSDoubleQuoted(rest[:i+1])
		if ok {
			return s
		}
		return ""
	}
	return ""
}

// decodeJSDoubleQuoted decodes one JavaScript double-quoted string literal
// without evaluating JavaScript. Codex's wrapper is JavaScript source, not a Go
// string: in addition to JSON escapes it may legally contain \v, \xNN, identity
// escapes, or escaped line continuations, all of which strconv.Unquote rejects.
func decodeJSDoubleQuoted(lit string) (string, bool) {
	if len(lit) < 2 || lit[0] != '"' || lit[len(lit)-1] != '"' {
		return "", false
	}
	var out strings.Builder
	for i := 1; i < len(lit)-1; {
		if lit[i] != '\\' {
			r, size := utf8.DecodeRuneInString(lit[i : len(lit)-1])
			if r == utf8.RuneError && size == 1 {
				return "", false
			}
			out.WriteRune(r)
			i += size
			continue
		}
		i++
		if i >= len(lit)-1 {
			return "", false
		}
		switch lit[i] {
		case '"', '\\', '/':
			out.WriteByte(lit[i])
			i++
		case 'b':
			out.WriteByte('\b')
			i++
		case 'f':
			out.WriteByte('\f')
			i++
		case 'n':
			out.WriteByte('\n')
			i++
		case 'r':
			out.WriteByte('\r')
			i++
		case 't':
			out.WriteByte('\t')
			i++
		case 'v':
			out.WriteByte('\v')
			i++
		case '0':
			// Numeric escapes are deliberately unsupported, except JavaScript's
			// unambiguous NUL escape (a following decimal digit makes it legacy
			// octal syntax).
			if i+1 < len(lit)-1 && lit[i+1] >= '0' && lit[i+1] <= '9' {
				return "", false
			}
			out.WriteByte(0)
			i++
		case '\n':
			i++ // JavaScript line continuation contributes no character.
		case '\r':
			i++
			if i < len(lit)-1 && lit[i] == '\n' {
				i++
			}
		case 'x':
			v, next, ok := decodeJSHexEscape(lit, i+1, 2)
			if !ok {
				return "", false
			}
			out.WriteRune(rune(v))
			i = next
		case 'u':
			v, next, ok := decodeJSUnicodeEscape(lit, i)
			if !ok {
				return "", false
			}
			i = next
			if v >= 0xD800 && v <= 0xDBFF && i+2 < len(lit)-1 && lit[i] == '\\' && lit[i+1] == 'u' {
				if low, after, lowOK := decodeJSUnicodeEscape(lit, i+1); lowOK && low >= 0xDC00 && low <= 0xDFFF {
					out.WriteRune(utf16.DecodeRune(rune(v), rune(low)))
					i = after
					continue
				}
			}
			if v >= 0xD800 && v <= 0xDFFF {
				out.WriteRune(utf8.RuneError)
			} else {
				out.WriteRune(rune(v))
			}
		default:
			// JavaScript identity escape: \q is the character q. Decode a full
			// UTF-8 rune so non-ASCII identity escapes are preserved as well.
			r, size := utf8.DecodeRuneInString(lit[i : len(lit)-1])
			if r == utf8.RuneError && size == 1 {
				return "", false
			}
			out.WriteRune(r)
			i += size
		}
	}
	return out.String(), true
}

func decodeJSUnicodeEscape(lit string, u int) (int, int, bool) {
	if u >= len(lit)-1 || lit[u] != 'u' {
		return 0, u, false
	}
	if u+1 < len(lit)-1 && lit[u+1] == '{' {
		end := strings.IndexByte(lit[u+2:len(lit)-1], '}')
		if end < 0 || end == 0 || end > 6 {
			return 0, u, false
		}
		end += u + 2
		v, _, ok := decodeJSHexEscape(lit, u+2, end-(u+2))
		if !ok || v > utf8.MaxRune {
			return 0, u, false
		}
		return v, end + 1, true
	}
	return decodeJSHexEscape(lit, u+1, 4)
}

func decodeJSHexEscape(lit string, start, count int) (int, int, bool) {
	if count <= 0 || start+count > len(lit)-1 {
		return 0, start, false
	}
	v := 0
	for _, c := range []byte(lit[start : start+count]) {
		v *= 16
		switch {
		case c >= '0' && c <= '9':
			v += int(c - '0')
		case c >= 'a' && c <= 'f':
			v += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v += int(c-'A') + 10
		default:
			return 0, start, false
		}
	}
	return v, start + count, true
}

// codexOutputText accepts both the legacy scalar output and the current
// Responses-style content array: [{type:"input_text", text:"..."}, ...].
func codexOutputText(v interface{}) string {
	switch out := v.(type) {
	case string:
		return out
	case []interface{}:
		parts := make([]string, 0, len(out))
		for _, item := range out {
			m, _ := item.(map[string]interface{})
			if text := stringField(m, "text"); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func codexToolStatus(output string) string {
	if strings.Contains(strings.ToLower(output), "script failed") {
		return "failed"
	}
	return "completed"
}

var codexRunningCellRe = regexp.MustCompile(`^Script running with cell ID ([0-9]+)$`)

func codexRunningCellID(output string) string {
	// A handoff is a host control response containing only this line. Searching
	// multiline stdout would misclassify a completed command that happened to
	// print the same text and suppress its event forever.
	m := codexRunningCellRe.FindStringSubmatch(strings.TrimSpace(output))
	if m == nil {
		return ""
	}
	return m[1]
}

// emitWrappedPatch reduces an apply_patch envelope to paths and line counts.
// Patch bodies stay only in transient normalizer memory and are never attached
// to the event; the normal source-exclusion pass remains a second backstop.
func (p *CodexRolloutProcessor) emitWrappedPatch(call codexPendingCall, callID, ts, raw string) []event.Event {
	patch := stringField(call.args, "patch")
	if patch == "" {
		return nil
	}
	type change struct {
		path           string
		added, removed int
	}
	var changes []change
	current := -1
	for _, line := range strings.Split(patch, "\n") {
		var path string
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: "} {
			if strings.HasPrefix(line, prefix) {
				path = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				break
			}
		}
		if path != "" {
			changes = append(changes, change{path: path})
			current = len(changes) - 1
			continue
		}
		if current < 0 || strings.HasPrefix(line, "*** ") || strings.HasPrefix(line, "@@") {
			continue
		}
		if strings.HasPrefix(line, "+") {
			changes[current].added++
		} else if strings.HasPrefix(line, "-") {
			changes[current].removed++
		}
	}
	events := make([]event.Event, 0, len(changes))
	for _, c := range changes {
		e := p.newCodexEvent("file_diff", ts, callID+"\x1f"+c.path)
		e.Provenance = event.AIProvenance()
		e.Data = map[string]interface{}{
			"path":         c.path,
			"linesAdded":   c.added,
			"linesRemoved": c.removed,
		}
		// RawPayload deliberately excludes the wrapper/patch source.
		e.RawPayload = "wrapped apply_patch"
		events = append(events, e)
	}
	return events
}

// codexSkillPathRe matches a read of a skill definition file, capturing the slug:
// the directory that immediately contains SKILL.md, at any depth under a skills
// root. Bundled Codex skills sit one level deeper
// (skills/.system/<slug>/SKILL.md) and DO count — they are skills the engineer
// reached for. `.` and `..` are rejected so a traversal cannot mint a slug.
//
// Both root spellings are accepted: Codex uses `skills/`, Cursor uses
// `skills-cursor/`. A Codex session can read either — the roots are just
// directories on the same machine — and Phase 2 wants the identical rule, so the
// pattern is written once rather than forked per tool.
var codexSkillPathRe = regexp.MustCompile(`/skills(?:-cursor)?/(?:[^/\s]+/)*([^/\s]+)/SKILL\.md\b`)

// codexSkillReadRe bounds the detector to commands that READ. Codex's own skill
// invocation uses `sed -n '1,240p' …`, but an engineer's session legitimately
// cats, heads or greps a SKILL.md too — all of those are the model taking the
// skill's text into context, which is the thing being counted. A command that
// merely NAMES the path to a non-reading program (rm, git add, mv) is not.
var codexSkillReadRe = regexp.MustCompile(`(^|[|;&]\s*|\s)(sed|cat|head|tail|less|more|bat|rg|grep|awk|nl)\b`)

// codexSkillSlugFromCommand recovers the skill slug a shell command read, or "".
//
// This is deliberately a HEURISTIC and is scoped like one. A model can read a
// SKILL.md without invoking the skill — while editing it, say. That is accepted:
// the field feeds "skills the model reached for" (the backend's
// modelInvokedSkills), which is what the board is titled, and the alternative on
// this rail is no signal whatsoever. It is never used to claim a skill RAN.
func codexSkillSlugFromCommand(cmd string) string {
	if !codexSkillReadRe.MatchString(cmd) {
		return ""
	}
	m := codexSkillPathRe.FindStringSubmatch(cmd)
	if m == nil {
		return ""
	}
	slug := m[1]
	if slug == "." || slug == ".." {
		return ""
	}
	return slug
}

// codexCommandString extracts a human-readable command from codex tool args,
// which may be {"cmd":"..."}, {"command":"..."} or {"command":["bash","-lc","..."]}.
func codexCommandString(args map[string]interface{}) string {
	if s := stringField(args, "cmd"); s != "" {
		return s
	}
	switch v := args["command"].(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// parseCodexArgs decodes a function_call's "arguments" field, which codex sends
// as a JSON-encoded string. custom_tool_call carries a plain "input" string.
func parseCodexArgs(payload map[string]interface{}) map[string]interface{} {
	if s, ok := payload["arguments"].(string); ok && s != "" {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
	}
	if m, ok := payload["arguments"].(map[string]interface{}); ok {
		return m
	}
	if s, ok := payload["input"].(string); ok && s != "" {
		return map[string]interface{}{"input": s}
	}
	return map[string]interface{}{}
}

var codexExitCodeRe = regexp.MustCompile(`(?i)(?:exited with code|exit code:?)\s*(\d+)`)

// parseCodexExecOutput pulls an exit code and the trailing stdout out of codex's
// exec output blob, which looks like:
//
//	Chunk ID: ...\nWall time: ...\nProcess exited with code 0\nOriginal token count: 2\nOutput:\n<stdout>
func parseCodexExecOutput(output string) (int, string) {
	exitCode := 0
	if m := codexExitCodeRe.FindStringSubmatch(output); m != nil {
		_, _ = fmt.Sscanf(m[1], "%d", &exitCode) // regex guarantees digits; exitCode stays 0 otherwise.
	}
	stdout := output
	if idx := strings.Index(output, "Output:\n"); idx >= 0 {
		stdout = output[idx+len("Output:\n"):]
	}
	return exitCode, stdout
}

// countDiffLines counts added/removed lines in a unified diff, excluding the
// ---/+++ file headers.
func countDiffLines(diff string) (added, removed int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return
}

// unifiedHunkHeaderRe captures the new-file side of a `@@ -a,b +c,d @@` hunk
// header: group 1 = new start (c), group 2 = new length (d, optional — a
// missing count means 1 per the unified-diff spec).
var unifiedHunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// lineRangesFromUnifiedDiff derives the new-file-side line span of each hunk in
// a unified diff as content-free {start,end,attribution} triples. Codex has no
// structured hunks, so this scans ONLY the `@@` header lines — never any
// `+`/`-` body content — mirroring the Claude structuredPatch path.
//
// Shape/attribution and the pure-deletion (new length 0) skip match
// lineRangesFromStructuredPatch; see its doc comment.
func lineRangesFromUnifiedDiff(diff, attribution string) []interface{} {
	var ranges []interface{}
	for _, line := range strings.Split(diff, "\n") {
		m := unifiedHunkHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start := 0
		_, _ = fmt.Sscanf(m[1], "%d", &start) // regex guarantees digits.
		count := 1
		if m[2] != "" {
			_, _ = fmt.Sscanf(m[2], "%d", &count)
		}
		if count == 0 {
			continue
		}
		ranges = append(ranges, map[string]interface{}{
			"start":       start,
			"end":         start + count - 1,
			"attribution": attribution,
		})
	}
	return ranges
}

// parseCodexTs normalizes a rollout timestamp ("2026-06-06T20:38:45.965Z") to
// RFC3339Nano. Returns "" if it can't be parsed (caller keeps the default).
func parseCodexTs(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func stringField(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

func intField(m map[string]interface{}, key string) int64 {
	if f, ok := m[key].(float64); ok {
		return int64(f)
	}
	return 0
}
