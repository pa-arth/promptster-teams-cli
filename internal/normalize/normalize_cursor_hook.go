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
// TWO THINGS THAT ARE NOT HERE AND ARE NOT COMING:
//
//   - Tokens. `stop` and `afterAgentResponse` construct input/output/cache token
//     fields in Cursor's shipped code, but neither step fires on the headless
//     agent path — confirmed on a turn that ended final_status:"completed". There
//     is no token or cost number on this surface.
//   - Anything resembling source. tool_output, edits[].old_string/new_string and
//     afterShellExecution's `output` all arrive in the payload and NONE of them
//     is read except to count lines. See cursorHookEditLineCounts.
type cursorHookPayload struct {
	HookEventName  string `json:"hook_event_name"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	GenerationID   string `json:"generation_id"`
	CursorVersion  string `json:"cursor_version"`

	// Model identity. `model` is the display/routing name and is frequently the
	// literal "default"; `model_id` is the resolved model and is the field worth
	// trusting. Both are kept because a session that only ever reports "default"
	// is still a fact about routing.
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

// NormalizeCursorHook turns one hook payload into events.
//
// `line` must already have been through redact.RedactBytes — the same
// raw-JSON-before-parse ordering the transcript rail uses.
func NormalizeCursorHook(line []byte) (CursorHookResult, bool) {
	var p cursorHookPayload
	if err := json.Unmarshal(line, &p); err != nil {
		return CursorHookResult{}, false
	}
	sess := p.sessionKey()
	if sess == "" || p.HookEventName == "" {
		return CursorHookResult{}, false
	}
	res := CursorHookResult{
		SessionID:      sess,
		TranscriptPath: p.TranscriptPath,
		Model:          p.modelLabel(),
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
	case "afterAgentThought":
		// Registered ONLY to reach model_id, and it emits no event of its own.
		//
		// Measured across a live run: afterAgentThought is the ONLY step that
		// carries model_id and model_params. Every other step reports
		// model:"default", which describes routing rather than a model and is
		// worth nothing downstream. Without this case the rail's headline
		// benefit — real model attribution — is simply absent, which is what the
		// first end-to-end run showed.
		//
		// Its `text` is the agent's reasoning prose and is NEVER read. The step
		// fires many times per turn (8 in one short run); the model event's id is
		// derived from session+model alone, so all of those collapse to one.
	default:
		// An unregistered or newly-added step. Emitting nothing is correct: a
		// step we did not opt into has not had its payload inspected for source.
		return res, false
	}
	// Model identity rides alongside whatever the step produced. Payloads that
	// only ever say "default" (sessionStart/sessionEnd do) resolve to no model and
	// add nothing.
	if e, ok := p.modelEvent(); ok {
		res.Events = append(res.Events, e)
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

// modelLabel resolves the model to report. model_id is the resolved model;
// `model` is often the literal "default", which describes routing rather than a
// model and is worth nothing downstream on its own.
func (p cursorHookPayload) modelLabel() string {
	if p.ModelID != "" {
		return p.ModelID
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

// modelEvent reports which model produced this session's work.
//
// WHY ai_response WITH NO USAGE NUMBERS. `model` is allowlisted today on exactly
// one kind — ai_response, via projectUsageFields — on BOTH the CLI's
// redact/project.go and the backend's eventFieldProjection.ts. Emitting the model
// on prompt/file_diff/command instead would require adding a field to two
// default-deny allowlists in lockstep, and a field added to only one side is
// stripped SILENTLY and reads as "an older CLI". Riding the kind that already
// permits it costs nothing and needs no coordinated release.
//
// The absent token fields are absent, not zero. Cursor exposes no token counts
// (see the file header), and the projector drops fields by omission, so an
// ai_response from this rail carries a model and nothing else — which is the
// honest shape of what Cursor tells us.
//
// The id is derived from session + model, so the same model reported on every
// payload of a session collapses to ONE event through ordinary dedupe rather
// than one per tool call.
func (p cursorHookPayload) modelEvent() (event.Event, bool) {
	model := p.modelLabel()
	if model == "" {
		return event.Event{}, false
	}
	e := p.newEvent("ai_response", "model\x1f"+model)
	// Derived from session+model ALONE — deliberately not from the hook step —
	// so afterFileEdit and afterShellExecution in one session produce the same id.
	e.ID = event.DeterministicUUID(p.sessionKey() + "\x1fai_response\x1fmodel\x1f" + model)
	e.Actor = event.AIActor()
	e.Provenance = hookAiProvenance()
	e.Data = map[string]interface{}{"model": model}
	return e, true
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
