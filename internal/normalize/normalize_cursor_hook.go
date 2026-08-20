package normalize

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// Cursor HOOK payloads — the richer of the two Cursor rails.
//
// Cursor invokes a registered command once per hook step with the payload as
// JSON on stdin. Unlike the transcript (see normalize_cursor.go), the payload
// carries model identity, real durations, session outcome, and an exact cwd.
//
// Field names below were captured from a live `cursor-agent` run against a
// throwaway workspace, not from documentation — Cursor's docs do not enumerate
// them and the sibling hiring CLI's fixtures are stale (they predate
// model_id/model_params/workspace_roots entirely). If a field here looks wrong,
// re-capture rather than reasoning about it: register a hook that appends stdin
// to a file and run one turn.
//
// TOKENS ARE HERE NOW, AND WHY THEY WERE NOT IS THE PART WORTH KEEPING.
//
// This header used to say Cursor exposed no token counts, on a 2026-08-03 probe
// that enrolled our own logger and observed nothing. It was measuring OUR
// configuration: Cursor's dispatcher early-returns on any step nobody
// registered (`hasHookForStep`, workbench.desktop.main.js @25399531, Cursor
// 3.12.17), and `stop` was not in cursorHookSteps. A probe that never ran and a
// vendor that sends nothing produce byte-identical evidence. The 2026-08-18
// re-probe added `beforeSubmitPrompt` as a POSITIVE CONTROL so that the two
// outcomes are distinguishable, and `stop` then delivered all four counts per
// generation.
//
// What `stop` carries, measured (raw payloads quoted in the change's design.md):
//
//   - input_tokens / output_tokens / cache_read_tokens / cache_write_tokens,
//     PER TURN — not cumulative. Output FELL 902 -> 525 between consecutive
//     generations of one conversation, which no running total can do. That is
//     the whole reason usageEvent tags `usageScope: "request"`: with the tag
//     absent, the backend's readUsage treats a row as CUMULATIVE, differences it
//     against a running maximum, and drops the first row as a baseline — the
//     observed pair would book 92,763 then +3,372, losing turn one entirely.
//   - status (completed | aborted | ...). An ABORTED turn arrives with the token
//     keys ABSENT, not zero. Deliberately NOT branched on: a `status ==
//     "completed"` gate would silently drop an unenumerated status that does
//     bill, where absent-means-absent covers that case for free.
//   - model / model_id — both the literal "default". The resolved id lives on
//     afterAgentThought, and the two arrive in different processes, so the join
//     happens on device; this package takes it as CursorHookOptions.ResolveModel
//     rather than doing any I/O of its own.
//
// STILL NOT HERE: anything resembling source. tool_output,
// edits[].old_string/new_string and afterShellExecution's `output` all arrive in
// the payload and NONE of them is read except to count lines. See
// cursorHookEditLineCounts. `stop` additionally carries `user_email` — the first
// PII field this rail has presented — and it survives nothing, because it is in
// no struct here and on no allowlist.
type cursorHookPayload struct {
	HookEventName  string `json:"hook_event_name"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	GenerationID   string `json:"generation_id"`
	CursorVersion  string `json:"cursor_version"`

	// Model identity. `model` is frequently the literal "default" (routing).
	// `model_id` is usually the resolved model — except Cursor also sends
	// model_id:"default" as a sentinel (IDE afterAgentThought, measured
	// 2026-08-03). modelLabel rejects both sentinels.
	Model       string                `json:"model"`
	ModelID     string                `json:"model_id"`
	ModelParams []cursorHookModelParm `json:"model_params"`

	WorkspaceRoots []string `json:"workspace_roots"`
	Cwd            string   `json:"cwd"`
	TranscriptPath string   `json:"transcript_path"`

	// beforeSubmitPrompt
	Prompt       string `json:"prompt"`
	ComposerMode string `json:"composer_mode"`

	// afterFileEdit
	FilePath string           `json:"file_path"`
	Edits    []cursorHookEdit `json:"edits"`

	// afterShellExecution
	Command string `json:"command"`
	Sandbox bool   `json:"sandbox"`

	// postToolUseFailure
	ToolName     string `json:"tool_name"`
	ErrorMessage string `json:"error_message"`
	FailureType  string `json:"failure_type"`
	IsInterrupt  bool   `json:"is_interrupt"`

	// stop — one generation ended. The four counts are POINTERS so that an
	// aborted turn's absent fields stay absent through normalization: a zero
	// here reads downstream as "this turn cost nothing" rather than "we were
	// not told", and that distinction is the one this rail keeps getting wrong.
	Status           string `json:"status"`
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`

	// sessionStart / sessionEnd
	FinalStatus       string  `json:"final_status"`
	Reason            string  `json:"reason"`
	IsBackgroundAgent bool    `json:"is_background_agent"`
	DurationMs        float64 `json:"duration_ms"`
	Duration          float64 `json:"duration"`
}

type cursorHookModelParm struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// cursorHookEdit is one edit within afterFileEdit. OldString and NewString are
// parsed ONLY so cursorHookEditLineCounts can count their lines; nothing else in
// this package may read them, and neither ever reaches an event.
type cursorHookEdit struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// CursorHookResult is what a hook invocation produced: the events to queue, plus
// the transcript this session writes to.
//
// TranscriptPath is the join key that keeps the two Cursor rails from
// double-counting. A hook payload names the exact transcript file the same
// session is appending to, so the watcher can skip a session that hooks already
// claimed. Without it the rails would have to be reconciled by content hash
// after the fact, which is guesswork; with it the handoff is exact.
type CursorHookResult struct {
	Events         []event.Event
	SessionID      string
	TranscriptPath string
	// Step is the hook step this payload came from. Surfaced so the capture
	// layer can decide what to persist WITHOUT re-parsing: the generation ->
	// model cache is written from afterAgentThought alone, and a result carrying
	// a model from some other step must not be allowed to write it.
	Step string
	// GenerationID identifies the one agent turn this payload belongs to. It is
	// the join key between the model (afterAgentThought) and the tokens (stop),
	// and the discriminator that gives each turn's usage row its own identity.
	GenerationID string
	// Model is the model this payload resolved, if any. Surfaced so the capture
	// layer can suppress a repeat without re-parsing the events.
	Model string
	// Workdir is the RAW absolute cwd this payload observed, before any
	// home-collapsing. Surfaced rather than resolved here because turning a cwd
	// into repoRoot/repoHost/repoTracked means running git, and this package
	// does no I/O. The capture layer resolves it exactly the way the transcript
	// rail does, so both rails stamp the identical fields.
	Workdir string
}

// CursorHookOptions carries what this package cannot obtain for itself.
//
// It exists for exactly one dependency: the tokens and the model arrive on two
// DIFFERENT hook invocations — separate short-lived processes seconds apart —
// so resolving a generation's model requires state on disk, and this package
// does no I/O. The capture layer owns the cache and passes a lookup in.
type CursorHookOptions struct {
	// ResolveModel returns the model already observed for a generation, or ""
	// when none is known. A "" answer is a real answer: the usage row is then
	// emitted with tokens and NO model rather than inheriting one from another
	// turn, because Cursor auto-routes and genuinely switches models mid
	// conversation. Nil is allowed and means "no cache" — the shape a test gets.
	ResolveModel func(generationID string) string
}

// NormalizeCursorHook turns one hook payload into events.
//
// `line` must already have been through redact.RedactBytes — the same
// raw-JSON-before-parse ordering the transcript rail uses.
func NormalizeCursorHook(line []byte, opts CursorHookOptions) (CursorHookResult, bool) {
	var p cursorHookPayload
	if err := json.Unmarshal(line, &p); err != nil {
		return CursorHookResult{}, false
	}
	sess := p.sessionKey()
	if sess == "" || p.HookEventName == "" {
		return CursorHookResult{}, false
	}
	// `stop` reports the routing sentinel on both model spellings, so its model
	// comes from the cache the thought step filled. Every other step resolves its
	// own or has none; neither is allowed to reach into the cache, because the
	// value of a per-generation join is precisely that it cannot inherit.
	model := p.modelLabel()
	if model == "" && p.HookEventName == "stop" && p.GenerationID != "" && opts.ResolveModel != nil {
		model = opts.ResolveModel(p.GenerationID)
	}
	res := CursorHookResult{
		SessionID:      sess,
		TranscriptPath: p.TranscriptPath,
		Step:           p.HookEventName,
		GenerationID:   p.GenerationID,
		Model:          model,
		Workdir:        p.workdirSource(),
	}

	switch p.HookEventName {
	case "sessionStart":
		res.Events = append(res.Events, p.lifecycleEvent("session_start"))
	case "sessionEnd":
		res.Events = append(res.Events, p.lifecycleEvent("session_end"))
	case "beforeSubmitPrompt":
		if e, ok := p.promptEvent(); ok {
			res.Events = append(res.Events, e)
		}
	case "afterFileEdit":
		if e, ok := p.fileEditEvent(); ok {
			res.Events = append(res.Events, e)
		}
	case "afterShellExecution":
		if e, ok := p.commandEvent(); ok {
			res.Events = append(res.Events, e)
		}
	case "postToolUseFailure":
		if e, ok := p.toolFailureEvent(); ok {
			res.Events = append(res.Events, e)
		}
	case "stop":
		if e, ok := p.usageEvent(model); ok {
			res.Events = append(res.Events, e)
		}
	case "afterAgentThought":
		// Registered ONLY to reach model_id, and it emits NO EVENT AT ALL.
		//
		// It used to mint an ai_response carrying the model, keyed on
		// session+model so a turn's eight thoughts collapsed to one row. That
		// identity is incompatible with per-turn tokens — every generation in a
		// session would collide onto one id and all but one turn's usage would be
		// lost to dedupe — so the model now rides the usage row instead, and this
		// step's only product is the cache entry the capture layer writes from
		// res.Model / res.GenerationID.
		//
		// Its `text` is the agent's reasoning prose and is NEVER read.
	default:
		// An unregistered or newly-added step. Emitting nothing is correct: a
		// step we did not opt into has not had its payload inspected for source.
		return res, false
	}
	return res, len(res.Events) > 0
}

// sessionKey picks the id that groups a session's events.
//
// conversation_id is preferred over session_id because it is also the transcript
// filename's uuid, so hook-derived and transcript-derived events for one session
// share an id without any translation. On the runs measured all three ids were
// equal, but they are not guaranteed to stay so, and the transcript is the tie
// that matters.
func (p cursorHookPayload) sessionKey() string {
	if p.ConversationID != "" {
		return p.ConversationID
	}
	return p.SessionID
}

// workdirSource is the cwd this payload observed. postToolUse-family payloads
// carry `cwd` directly; session and prompt payloads carry workspace_roots.
func (p cursorHookPayload) workdirSource() string {
	if p.Cwd != "" {
		return p.Cwd
	}
	if len(p.WorkspaceRoots) > 0 {
		return p.WorkspaceRoots[0]
	}
	return ""
}

// newEvent builds the envelope. The id is derived from the session, the step and
// a discriminator rather than minted randomly, so a hook that Cursor retries —
// or a payload replayed from a queue — cannot double-count.
func (p cursorHookPayload) newEvent(kind, discriminator string) event.Event {
	sess := p.sessionKey()
	e := event.NewEvent(kind, sess)
	e.ID = event.DeterministicUUID(
		sess + "\x1f" + p.HookEventName + "\x1f" + kind + "\x1f" + discriminator,
	)
	// Same string as the transcript rail: this is what puts "cursor" into the
	// session row's source_service. Do not diverge the two rails here or one
	// session would appear under two tools.
	e.Source = "cursor"
	return e
}

func (p cursorHookPayload) newAIEvent(kind, discriminator string) event.Event {
	e := p.newEvent(kind, discriminator)
	e.Actor = event.AIActor()
	e.Provenance = hookAiProvenance()
	return e
}

// modelLabel resolves the model to report. model_id is usually the resolved
// model, but Cursor also sends the literal "default" there as a routing
// sentinel (IDE afterAgentThought, measured 2026-08-03). Treating that as a
// real model poisons model-mix metrics and used to mint an ai_response that
// claimed the transcript away from the watcher with nothing useful on it.
// `model` is often the same sentinel and is worth nothing on its own.
func (p cursorHookPayload) modelLabel() string {
	if id := p.ModelID; id != "" && id != "default" {
		return id
	}
	if p.Model != "" && p.Model != "default" {
		return p.Model
	}
	return ""
}

// NOTE ON model_params: afterAgentThought also carries a reasoning-effort
// parameter (high/medium/low). It is deliberately NOT emitted. `effort` is not
// allowlisted in internal/redact/project.go or the backend's
// eventFieldProjection.ts, so emitting it would be stripped SILENTLY on one side
// or the other and read as "an older CLI" — MUST-DO #2's exact trap. If it is
// ever wanted, both allowlists learn it in the same change or not at all.

func (p cursorHookPayload) lifecycleEvent(kind string) event.Event {
	e := p.newEvent(kind, p.Reason+"\x1f"+p.FinalStatus)
	e.Actor = event.SystemActor()
	data := map[string]interface{}{}
	// `reason` is a short enum (completed | error | …). final_status carries the
	// same vocabulary; only one is kept so the allowlist stays minimal.
	if r := p.Reason; r != "" {
		data["reason"] = r
	} else if p.FinalStatus != "" {
		data["reason"] = p.FinalStatus
	}
	e.Data = data
	return e
}

// StampCursorHookRepoIdentity writes the session's repo identity onto the events
// that carry it, matching the transcript rail's contract field for field.
//
// It exists because the hook rail CLAIMS a session away from the watcher. If the
// hook rail emitted a prompt without these, a machine with hooks enrolled would
// silently lose repo attribution for every Cursor session — a regression against
// the rail it replaced, and invisible, because the events still arrive.
//
// `repoTracked` is stamped explicitly true OR false whenever repoRoot is, so
// "not a repo" stays distinguishable from "a CLI too old to have looked". Same
// rule as the Claude and Codex processors; do not make it omitempty.
func StampCursorHookRepoIdentity(evs []event.Event, workdir, repoRoot, repoHost string, repoTracked bool) {
	for i := range evs {
		if evs[i].Kind != "prompt" {
			continue
		}
		data, ok := evs[i].Data.(map[string]interface{})
		if !ok {
			continue
		}
		if workdir != "" {
			data["workdir"] = workdir
		}
		if repoRoot != "" {
			data["repoRoot"] = repoRoot
			data["repoTracked"] = repoTracked
		}
		if repoHost != "" {
			data["repoHost"] = repoHost
		}
	}
}

func (p cursorHookPayload) promptEvent() (event.Event, bool) {
	text := strings.TrimSpace(p.Prompt)
	if text == "" {
		return event.Event{}, false
	}
	// Unlike the transcript rail, `prompt` here is already the engineer's own
	// text — Cursor has not yet wrapped it in the <user_query> envelope with
	// attached file contents and rules. So there is nothing to strip, and equally
	// nothing else in this payload may be added to it.
	e := p.newEvent("prompt", hashDiscriminator(text))
	e.Actor = event.HumanActor()
	e.Provenance = hookHumanProvenance()
	e.Data = map[string]interface{}{"text": text}
	return e, true
}

func (p cursorHookPayload) fileEditEvent() (event.Event, bool) {
	if p.FilePath == "" {
		return event.Event{}, false
	}
	added, removed := cursorHookEditLineCounts(p.Edits)
	e := p.newAIEvent("file_diff", p.FilePath+"\x1f"+strconv.Itoa(added)+"\x1f"+strconv.Itoa(removed))
	e.Data = map[string]interface{}{
		"path":         p.FilePath,
		"linesAdded":   added,
		"linesRemoved": removed,
	}
	return e, true
}

func (p cursorHookPayload) commandEvent() (event.Event, bool) {
	if strings.TrimSpace(p.Command) == "" {
		return event.Event{}, false
	}
	e := p.newAIEvent("command", hashDiscriminator(p.Command))
	data := map[string]interface{}{"command": p.Command}
	// `duration` is FRACTIONAL MILLISECONDS, not seconds.
	//
	// This was gotten wrong first time on a single sample (duration: 15.113 read
	// as 15 seconds) and the live run caught it: `go build ./...` on a two-file
	// module reported duration 2021.129, which is 2.02 SECONDS, and the seconds
	// reading turned it into 33 minutes. Cross-checks: sessionEnd's duration_ms
	// for a 15s turn was 15337, same unit and same magnitude.
	//
	// Truncate rather than scale. If a future Cursor build switches units, a
	// wrong number here silently poisons every latency metric we derive — there
	// is no downstream signal that a duration is implausible.
	if p.Duration > 0 {
		data["durationMs"] = int(p.Duration)
	}
	// NOTE: no exitCode. afterShellExecution carries `output` but no status code,
	// and inventing one from empty output would be a fabricated fact.
	e.Data = data
	return e, true
}

func (p cursorHookPayload) toolFailureEvent() (event.Event, bool) {
	if p.ToolName == "" {
		return event.Event{}, false
	}
	e := p.newAIEvent("tool_result", p.ToolName+"\x1f"+p.FailureType)
	data := map[string]interface{}{
		"tool":   p.ToolName,
		"status": "error",
	}
	// failure_type is Cursor's own short enum (e.g. permission_denied), safe to
	// keep. error_message is NOT kept: it routinely embeds file paths, command
	// bodies and tool output.
	if p.FailureType != "" {
		data["status"] = p.FailureType
	}
	e.Data = data
	return e, true
}

// hookHumanProvenance / hookAiProvenance mirror the transcript-rail helpers but
// record `cursor-hook` as the method, so the worker can tell which of the two
// Cursor rails observed an event. Confidence is HIGHER than the transcript's for
// the AI case: a hook fires from inside the agent's own execution path, so an
// afterFileEdit is the agent reporting its own edit rather than an inference
// drawn from a transcript record.
func hookHumanProvenance() *event.Provenance {
	return &event.Provenance{
		Attribution:   "likely_human",
		Confidence:    0.9,
		Observability: "high",
		Methods:       []string{"cursor-hook"},
	}
}

func hookAiProvenance() *event.Provenance {
	return &event.Provenance{
		Attribution:   "likely_ai",
		Confidence:    0.95,
		Observability: "high",
		Methods:       []string{"cursor-hook"},
	}
}

// usageEvent is one generation's token usage — the only event `stop` produces.
//
// WHY ai_response. `model` and the token fields are allowlisted on exactly one
// kind — ai_response, via projectUsageFields — on BOTH the CLI's
// redact/project.go and the backend's eventFieldProjection.ts. Riding the kind
// that already permits them needs no coordinated release; inventing a kind would
// persist NOTHING until both default-deny allowlists learned it in lockstep.
//
// THE IDENTITY IS PER GENERATION, AND THAT IS THE WHOLE POINT. The retired
// modelEvent derived its id from session+model alone so a session's repeated
// model reports collapsed to one row. Reusing that here would collapse a
// four-turn session's four usage rows onto one id and lose three turns' spend to
// ordinary dedupe — silently, since the surviving row looks perfectly healthy.
//
// ABSENT COUNTS STAY ABSENT. An aborted turn carries no token keys at all, and
// writing zeros for them would assert that a turn we were told nothing about
// cost nothing. Same rule for the model: no cache entry means no `model` key,
// which the backend's readUsage declines to price — correct, and visible, where
// a defaulted model would be confidently wrong.
func (p cursorHookPayload) usageEvent(model string) (event.Event, bool) {
	data := map[string]interface{}{}
	putCount(data, "inputTokens", p.InputTokens)
	putCount(data, "outputTokens", p.OutputTokens)
	putCount(data, "cacheReadTokens", p.CacheReadTokens)
	putCount(data, "cacheWriteTokens", p.CacheWriteTokens)
	if model != "" {
		data["model"] = model
	}
	if len(data) == 0 {
		// An aborted turn whose generation never resolved a model: nothing was
		// measured and nothing is claimed. Emitting a row carrying only a scope
		// tag would ship bytes that say nothing and inflate this rail's response
		// count against the rails it is compared with.
		return event.Event{}, false
	}
	// PER-REQUEST, ASSERTED AT THE EMITTER. See the file header: absent, this
	// tag means CUMULATIVE downstream, and these numbers are not.
	data["usageScope"] = "request"
	e := p.newAIEvent("ai_response", p.usageDiscriminator())
	e.Data = data
	return e, true
}

// usageDiscriminator gives one turn's usage row a stable identity.
//
// generation_id is it, on every payload observed. The fallback exists because
// the cost of the two failure modes is wildly asymmetric: a COLLIDING id loses
// turns to dedupe with no trace, while a duplicate id merely costs a redundant
// event that the backend collapses anyway. So a payload with no generation id
// keys on its own counts and status — distinct turns differ, a retried delivery
// of the same payload does not.
func (p cursorHookPayload) usageDiscriminator() string {
	if p.GenerationID != "" {
		return "gen\x1f" + p.GenerationID
	}
	return "nogen\x1f" + p.Status + "\x1f" + hashDiscriminator(
		countKey(p.InputTokens)+"/"+countKey(p.OutputTokens)+
			"/"+countKey(p.CacheReadTokens)+"/"+countKey(p.CacheWriteTokens))
}

// putCount writes a count only when Cursor actually reported one.
func putCount(data map[string]interface{}, key string, v *int64) {
	if v == nil {
		return
	}
	data[key] = *v
}

// countKey renders a count for an id derivation, keeping absent distinguishable
// from zero — "-" and "0" are different turns.
func countKey(v *int64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatInt(*v, 10)
}

// cursorHookEditLineCounts is the ONLY function in this file that reads
// old_string or new_string, and it returns two integers.
//
// This mirrors cursorLineCount in the transcript rail deliberately. The sibling
// hiring CLI synthesizes a unified diff from these same two fields; that is
// correct there and forbidden here, and the containment is structural rather
// than a review rule: no other function can see the strings because no other
// function is passed them.
func cursorHookEditLineCounts(edits []cursorHookEdit) (added, removed int) {
	for _, ed := range edits {
		added += cursorLineCount(ed.NewString)
		removed += cursorLineCount(ed.OldString)
	}
	return added, removed
}

// hashDiscriminator makes a stable, bounded id component out of a variable-length
// string without carrying the string itself into the id derivation input's
// length. It is NOT a privacy boundary — DeterministicUUID already hashes — it
// just keeps the derivation input small and fixed.
func hashDiscriminator(s string) string {
	return strconv.Itoa(len(s)) + "\x1f" + strconv.FormatUint(uint64(fnv32(s)), 16)
}

func fnv32(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}
