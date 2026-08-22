package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Cursor hook enrollment.
//
// WHY HOOKS EXIST HERE AT ALL, GIVEN THE TRANSCRIPT WATCHER.
//
// The transcript rail (cmd_cursor_watch.go) needs no enrollment and captures
// sessions that predate install, but a Cursor transcript is thin: verified
// exhaustively against 64 transcripts / 1504 lines, its ONLY keys are `role` and
// `message.content[]`. No model, no timestamps, no cwd, no tool results.
//
// The hook payload carries all of those. Measured on a live `cursor-agent` run
// (2026.07.23-e383d2b), an ordinary turn delivers:
//
//   - model_id "grok-4.5" + model_params [{effort high} {fast false}]  — real
//     model attribution, where the transcript has none and Cursor's own
//     ai-code-tracking.db says "default" for 69% of rows
//   - duration / duration_ms per tool call, per thought, per session
//   - final_status + reason on sessionEnd (completed | error)
//   - error_message / failure_type / is_interrupt on postToolUseFailure
//   - workspace_roots (exact cwd) and transcript_path
//
// TOKENS ARE CAPTURED HERE, VIA `stop`. This block used to say the opposite,
// and the correction is the reusable part.
//
// The 2026-08-03 verdict was: "a logger enrolled in OUR ~/.cursor/hooks.json did
// not receive any stop payload across a 20-minute IDE probe; Cursor also
// requested zero stop steps for that window after enrollment". Every word of
// that observation was accurate. The conclusion drawn from it — that the vendor
// declines to send us tokens — was not. Cursor's dispatcher early-returns on any
// step nobody registered:
//
//	async triggerStopHook(e, t) {
//	  if (!this._cursorHooksService.hasHookForStep(Nu.stop)) return;   // <—
//	  ... input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
//	}
//	                       (workbench.desktop.main.js @25399531, Cursor 3.12.17)
//
// "Cursor requested zero stop steps" is what that early return looks like from
// outside. The probe measured OUR configuration — `stop` was absent from
// cursorHookSteps below — and filed the result under the vendor's capabilities.
//
// THE MISSING PIECE WAS A POSITIVE CONTROL. A probe that never ran and a vendor
// that sends nothing produce byte-identical evidence. The 2026-08-18 re-probe
// registered `beforeSubmitPrompt` — a step known to fire — beside the unknowns,
// so the two outcomes became distinguishable. The control fired three times;
// `stop` then delivered all four token counts per generation, per turn.
//
// The generalised rule now lives in the change's
// specs/vendor-capability-claims/spec.md: a recorded claim that a vendor does
// NOT emit something SHALL record how enrollment was confirmed.
//
// WHY THE USER SCOPE, NOT THE PROJECT SCOPE. Cursor resolves four hook configs
// and merges them:
//
//	enterprise  /Library/Application Support/Cursor/hooks.json   (macOS)
//	            C:\ProgramData\Cursor\hooks.json | /etc/cursor/hooks.json
//	team        <project>/.cursor/managed/active-team-hooks/hooks.json
//	user        ~/.cursor/hooks.json
//	project     <project>/.cursor/hooks.json
//
// Only the PROJECT scope is a tracked file inside the customer's repository, and
// only the project scope makes enrollment per-repo. The user scope is one file
// in the engineer's home directory — the same shape as Claude Code's
// ~/.claude/settings.json. So the objection that killed the original hooks plan
// ("we do not write into customer repos, and per-repo enrollment means every
// repo an engineer forgets reads as captured-nothing") applies to the project
// scope alone. NEVER write the project scope.
func cursorUserHooksPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cursor", "hooks.json")
}

// cursorHookSteps are the steps we register.
//
// Deliberately NOT registered:
//   - afterAgentResponse — it reads the SAME turnTokenUsage object as `stop` and
//     fires on the same path, so registering both delivers each generation's
//     numbers twice under two hook_event_names (observed: generation 03d46a51
//     reported {92763, 902, 68096} on each). Of the two, `stop` is strictly
//     better: it fires on aborted and error turns as well as completed ones, it
//     carries loop_count, and it carries NO assistant prose, where
//     afterAgentResponse hands us the model's entire message on every turn.
//     Deduping downstream was considered and rejected — it requires the
//     duplicate to arrive, which means projecting and signing prose we do not
//     want, forever, to throw half of it away.
//   - preToolUse — it gates the tool call: a slow or wedged hook there stalls the
//     engineer's agent before any work happens. postToolUse carries the same
//     identity fields plus the outcome, so the blocking one buys nothing.
//   - preCompact / subagentStart / subagentStop — real signals, still out of
//     scope. preCompact carries context_usage_percent / context_tokens /
//     context_window_size, the one context-pressure metric this rail exposes;
//     it stays unregistered until its payload shape has been observed rather
//     than assumed, and until the compaction model it would feed has settled.
var cursorHookSteps = []string{
	"sessionStart",
	"sessionEnd",
	"beforeSubmitPrompt",
	"afterFileEdit",
	"afterShellExecution",
	"postToolUseFailure",
	// The end of one generation, and the ONLY step carrying token counts. See
	// the TOKENS block above for why it took two probes to find that out.
	//
	// It is also a GATING step: Cursor reads this command's stdout and, if it
	// finds `followup_message`, SUBMITS IT AS A NEW CHAT TURN (up to
	// stopHookLoopCount 5). RunCursorHook therefore prints a compile-time
	// constant and never serialises any part of the payload — a structural
	// containment, not a review rule.
	"stop",
	// Registered for ONE field: model_id. It is the only step that carries it —
	// every other step says model:"default". The normalizer emits nothing else
	// from it and never reads its reasoning text. See normalize_cursor_hook.go.
	"afterAgentThought",
}

// cursorHookConfig mirrors Cursor's hooks.json. Unknown fields are preserved
// through a raw map so a merge never drops an engineer's own configuration —
// see mergeCursorHooks.
type cursorHookConfig struct {
	Version int                        `json:"version"`
	Hooks   map[string][]cursorHookCmd `json:"hooks"`
	// rest holds every top-level key we do not model, so writing the file back
	// does not silently delete an engineer's settings.
	rest map[string]json.RawMessage
}

// cursorHookCmd is one entry in a step's array.
//
// IT ROUND-TRIPS THE ENTRY'S ORIGINAL KEYS VERBATIM, and that is not tidiness —
// it is the fix for a defect that killed every hook on the machine it ran on.
//
// This struct used to model four string fields and marshal exactly those, which
// destroyed a neighbour's configuration two independent ways:
//
//  1. `type` was tagged `json:"type"` with no omitempty. Cursor's schema lets an
//     entry OMIT `type` (it defaults to "command"), so `{"command": "…"}` is
//     legal and common. Reading one gave Type == "" and writing it back produced
//     `"type": ""` — which Cursor rejects, and it rejects the WHOLE FILE for it.
//     Every hook on that machine, ours and three other tools', went silent for
//     two days. See the header of cursor_hooks_validate.go for the timeline.
//  2. `timeout`, `matcher`, `loop_limit` and `failClosed` are real Cursor fields
//     this struct never modelled, so they were silently deleted on every write.
//     `matcher` decides which files a neighbour's hook fires on and `failClosed`
//     decides whether its failure blocks the agent — dropping them changes what
//     their tool DOES, with nothing in their file to show why.
//
// So `raw` is the source of truth for marshalling and the typed fields are
// decoded conveniences for our own matching. Repairs edit `raw`; nothing mutates
// a typed field in place, because two representations of one entry drift.
type cursorHookCmd struct {
	Type    string
	Command string
	Prompt  string
	Model   string

	// raw is the entry exactly as read. nil for entries constructed in code.
	raw map[string]json.RawMessage
	// notObject holds an array element that is not a JSON object at all, kept
	// byte-for-byte. Cursor calls that invalid; we keep it so the validator can
	// SAY so and so writing the file back does not silently delete it.
	notObject json.RawMessage
}

// newCursorCommandHook builds one of our own entries.
func newCursorCommandHook(command string) cursorHookCmd {
	return cursorHookCmd{
		Type:    "command",
		Command: command,
		raw: map[string]json.RawMessage{
			"type":    json.RawMessage(`"command"`),
			"command": mustJSONString(command),
		},
	}
}

func mustJSONString(v string) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// A Go string always marshals. Falling back to an empty JSON string keeps
		// this total rather than panicking inside a best-effort install path.
		return json.RawMessage(`""`)
	}
	return b
}

func (c *cursorHookCmd) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		// Not an object. Keep the bytes: an entry we cannot model is still an
		// entry the engineer wrote, and the validator has to be able to see it to
		// explain why Cursor is refusing the file.
		c.notObject = append(json.RawMessage(nil), b...)
		return nil
	}
	c.raw = raw
	// Decoded best-effort: a non-string `command` must NOT fail the load, or we
	// never reach the validator that can name it. Presence and type are read from
	// raw, never inferred from these being empty.
	c.Type = cursorRawString(raw["type"])
	c.Command = cursorRawString(raw["command"])
	c.Prompt = cursorRawString(raw["prompt"])
	c.Model = cursorRawString(raw["model"])
	return nil
}

func (c cursorHookCmd) MarshalJSON() ([]byte, error) {
	if c.notObject != nil {
		return c.notObject, nil
	}
	if c.raw != nil {
		return json.Marshal(c.raw)
	}
	// A struct literal (tests, older call sites). Emit only non-empty keys, so a
	// literal without a Type does not acquire the `"type": ""` this whole file
	// exists to stop producing.
	out := map[string]json.RawMessage{}
	for k, v := range map[string]string{"type": c.Type, "command": c.Command, "prompt": c.Prompt, "model": c.Model} {
		if v != "" {
			out[k] = mustJSONString(v)
		}
	}
	return json.Marshal(out)
}

// has reports whether the entry carries a key AT ALL — the distinction Cursor
// draws between an absent `type` (legal, defaults to "command") and an empty one
// (fatal for the entire file).
func (c cursorHookCmd) has(key string) bool {
	if c.raw != nil {
		_, ok := c.raw[key]
		return ok
	}
	switch key {
	case "type":
		return c.Type != ""
	case "command":
		return c.Command != ""
	case "prompt":
		return c.Prompt != ""
	case "model":
		return c.Model != ""
	}
	return false
}

func (c cursorHookCmd) isString(key string) bool {
	if c.raw == nil {
		return c.has(key)
	}
	v, ok := c.raw[key]
	t := strings.TrimSpace(string(v))
	return ok && strings.HasPrefix(t, `"`)
}

func (c cursorHookCmd) isBool(key string) bool {
	v, ok := c.raw[key]
	t := strings.TrimSpace(string(v))
	return ok && (t == "true" || t == "false")
}

func (c cursorHookCmd) isNull(key string) bool {
	v, ok := c.raw[key]
	return ok && strings.TrimSpace(string(v)) == "null"
}

func (c cursorHookCmd) number(key string) (float64, bool) {
	v, ok := c.raw[key]
	if !ok {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return 0, false
	}
	return f, true
}

// withoutKey returns a copy with one key deleted, leaving the original alone.
func (c cursorHookCmd) withoutKey(key string) cursorHookCmd {
	out := c
	out.raw = map[string]json.RawMessage{}
	for k, v := range c.raw {
		if k != key {
			out.raw[k] = v
		}
	}
	switch key {
	case "type":
		out.Type = ""
	case "command":
		out.Command = ""
	case "prompt":
		out.Prompt = ""
	case "model":
		out.Model = ""
	}
	return out
}

// cursorRawString decodes a JSON string, or "" for anything that is not one.
func cursorRawString(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}

// cursorHookCommand is the command Cursor invokes. It MUST be the canonical
// managed binary path, not os.Executable().
//
// This is the autostart bug, exactly. `autostart enable` rendered
// state.SelfBin() into a launchd plist once and nothing revisited it; a live
// plist was found naming a node_modules path that the next `npm i -g` deleted,
// and capture silently never came back at the following login. A hooks.json
// entry has the identical failure mode and no louder signal — Cursor just runs a
// missing command and moves on. CanonicalInstallBin() is the path self-update
// swaps in place and the one npm's postinstall and install.sh both target, so it
// stays valid across every update mechanism.
func cursorHookCommand() string {
	return fmt.Sprintf("%q cursor-hook", state.CanonicalInstallBin())
}

// isPromptsterCursorHook reports whether an entry is ours, so a re-install
// replaces it rather than appending a duplicate (which would double-emit every
// event). Matched on the `cursor-hook` subcommand rather than the full string:
// the binary path legitimately changes across install methods, and an entry
// whose path went stale is precisely the one that must be REPLACED, not kept
// alongside a second copy.
func isPromptsterCursorHook(c cursorHookCmd) bool {
	// A type-less entry IS a command hook — Cursor defaults it. Requiring
	// type == "command" here would fail to recognise our own binary in a
	// hand-written entry and append a second copy beside it, double-emitting
	// every event on that step.
	if c.has("type") && c.Type != "command" {
		return false
	}
	return containsCursorHookSubcommand(c.Command)
}

func containsCursorHookSubcommand(cmd string) bool {
	// A bare "cursor-hook" match would also hit an engineer's own script named
	// e.g. "my-cursor-hooks.sh". Require the promptster binary name alongside it.
	return strings.Contains(cmd, "cursor-hook") && strings.Contains(cmd, "promptster-teams")
}

// loadCursorHookConfig reads the user-scope hooks.json. A missing file is an
// empty config, not an error — first enrollment is the common case.
//
// A file that exists but does NOT parse is an error and the caller must abort.
// Overwriting it would destroy an engineer's configuration to install telemetry,
// which is not a trade this CLI gets to make on their behalf.
func loadCursorHookConfig(path string) (cursorHookConfig, error) {
	cfg := cursorHookConfig{Version: 1, Hooks: map[string][]cursorHookCmd{}, rest: map[string]json.RawMessage{}}
	data, err := os.ReadFile(path) // #nosec G304 -- fixed path under the user's home.
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if len(data) == 0 {
		return cfg, nil
	}

	parsed, err := parseCursorHookConfig(data)
	if err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return parsed, nil
}

// parseCursorHookConfig decodes hooks.json bytes.
//
// SYNTAX ERRORS ARE FATAL, SEMANTIC ONES ARE NOT, and the split is deliberate.
// Bytes we cannot parse cannot be rewritten without guessing at what they meant,
// so the caller aborts. An entry that parses but breaks Cursor's rules is
// something we can name precisely, report, and sometimes repair — so it must
// reach the validator rather than dying here as "unreadable".
func parseCursorHookConfig(data []byte) (cursorHookConfig, error) {
	cfg := cursorHookConfig{Version: 1, Hooks: map[string][]cursorHookCmd{}, rest: map[string]json.RawMessage{}}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return cfg, fmt.Errorf("is not valid JSON: %w", err)
	}
	// Version is read WITHOUT a default when the key is present: Cursor rejects a
	// config whose version is not a positive integer, so a bad one has to survive
	// as far as the validator instead of being quietly normalised to 1 here.
	if _, ok := raw["version"]; !ok {
		cfg.Version = 0
	}
	for k, v := range raw {
		switch k {
		case "version":
			// A non-numeric version leaves this 0, which the validator reports and
			// the repair sets to 1.
			cfg.Version = 0
			_ = json.Unmarshal(v, &cfg.Version)
		case "hooks":
			if err := json.Unmarshal(v, &cfg.Hooks); err != nil {
				return cfg, fmt.Errorf("has an unreadable \"hooks\" block: %w", err)
			}
		default:
			cfg.rest[k] = v
		}
	}
	if cfg.Hooks == nil {
		cfg.Hooks = map[string][]cursorHookCmd{}
	}
	return cfg, nil
}

// mergeCursorHooks installs our entry into every step we want, leaving every
// other entry and every unmodelled top-level key untouched. Returns whether
// anything changed, so an unchanged config is not rewritten on every daemon
// start (a needless write that would churn mtime and race other writers).
func mergeCursorHooks(cfg *cursorHookConfig, command string) bool {
	changed := false
	for _, step := range cursorHookSteps {
		entries := cfg.Hooks[step]
		found := false
		kept := make([]cursorHookCmd, 0, len(entries)+1)
		for _, e := range entries {
			if isPromptsterCursorHook(e) {
				if found || e.Command != command {
					// Drop a stale-path copy or a duplicate; the canonical entry
					// is appended below exactly once.
					changed = true
					continue
				}
				found = true
			}
			kept = append(kept, e)
		}
		if !found {
			kept = append(kept, newCursorCommandHook(command))
			changed = true
		}
		cfg.Hooks[step] = kept
	}
	return changed
}

// removeCursorHooks strips our entries, leaving everything else. Returns whether
// anything changed. An empty step key is deleted rather than left as an empty
// array, so an uninstall restores the file to something an engineer recognises.
func removeCursorHooks(cfg *cursorHookConfig) bool {
	changed := false
	for step, entries := range cfg.Hooks {
		kept := make([]cursorHookCmd, 0, len(entries))
		for _, e := range entries {
			if isPromptsterCursorHook(e) {
				changed = true
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(cfg.Hooks, step)
			continue
		}
		cfg.Hooks[step] = kept
	}
	return changed
}

// saveCursorHookConfig writes the config back, preserving unmodelled keys.
// Written to a temp file and renamed so a crash mid-write cannot leave the
// engineer with a truncated hooks.json — which would break THEIR hooks, not just
// ours.
func saveCursorHookConfig(path string, cfg cursorHookConfig) error {
	out := map[string]json.RawMessage{}
	for k, v := range cfg.rest {
		out[k] = v
	}
	ver, err := json.Marshal(cfg.Version)
	if err != nil {
		return err
	}
	out["version"] = ver
	hooks, err := json.Marshal(cfg.Hooks)
	if err != nil {
		return err
	}
	out["hooks"] = hooks

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	// THE GATE: never persist a config Cursor would throw away.
	//
	// This is the check whose absence caused the outage. A read-side check would
	// not have caught it — the file we READ was valid, and our own serialisation
	// is what made it invalid. So the assertion has to sit on the last thing that
	// happens before the bytes land, and it has to run over the bytes themselves
	// rather than over the struct they came from.
	//
	// Refusing leaves the file exactly as it was. That is the right outcome even
	// when the defect is somebody else's: an unenrolled rail costs us data, while
	// a rejected hooks.json costs the engineer every hook they own.
	var written cursorHookConfig
	if written, err = parseCursorHookConfig(data); err != nil {
		return fmt.Errorf("refusing to write %s: %w", path, err)
	}
	if defects, _ := cursorHookConfigDefects(written); len(defects) > 0 {
		return fmt.Errorf("refusing to write %s: Cursor would reject the whole file (%s)", path, defects[0])
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".promptster.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EnsureCursorHooks installs (or repairs) the user-scope hook entries.
//
// IT MUST NEVER RETURN A FATAL ERROR TO ITS CALLER'S CONTROL FLOW. It runs on
// every `watch` startup, and capture working is worth more than hook enrollment
// working: a malformed hooks.json, a read-only home, or a locked file must
// degrade to "transcript-only capture", never to "no capture". The caller logs
// and continues. Same invariant as npm's postinstall and `autostart repair`,
// for the same reason.
//
// It is also the migration path for an already-installed fleet: the daemon
// self-updates within ~30m and re-execs, this runs at the new binary's startup,
// and the engineer does nothing. That only holds because it is idempotent and
// re-renders the command unconditionally rather than trusting whatever path is
// already in the file.
func EnsureCursorHooks() (changed bool, err error) {
	changed, _, err = ensureCursorHooks()
	return changed, err
}

// ensureCursorHooks is EnsureCursorHooks plus the repairs it made, which the
// caller reports. Repairs are separated from `changed` on purpose: enrolling our
// own entries is routine and silent, while editing an entry belonging to another
// tool is something an engineer is entitled to hear about.
func ensureCursorHooks() (changed bool, repairs []cursorHookRepair, err error) {
	if !dirExists(filepath.Dir(cursorUserHooksPath())) {
		// No ~/.cursor at all — Cursor is not installed on this machine. Creating
		// the directory would leave a config for a product the engineer does not
		// use, so do nothing and report no change.
		return false, nil, nil
	}
	path := cursorUserHooksPath()
	original, _ := os.ReadFile(path) // #nosec G304 -- fixed path under the user's home.
	cfg, err := loadCursorHookConfig(path)
	if err != nil {
		return false, nil, err
	}

	// REPAIR BEFORE MERGE. Our entries are irrelevant while the file as a whole
	// is being rejected — Cursor runs none of it — so the damage has to be
	// cleared first or enrolling is theatre.
	repairs = repairCursorHookConfig(&cfg)
	merged := mergeCursorHooks(&cfg, cursorHookCommand())
	if len(repairs) == 0 && !merged {
		return false, nil, nil
	}
	if err := saveCursorHookConfig(path, cfg); err != nil {
		return false, nil, err
	}
	recordCursorHookRepairs(path, original, repairs)
	return true, repairs, nil
}

// RemoveCursorHooks unenrolls this machine. Exported for a future `cursor-hook
// disable`; uninstall paths must call it so a removed CLI does not leave Cursor
// invoking a binary that no longer exists on every single event.
func RemoveCursorHooks() (changed bool, err error) {
	path := cursorUserHooksPath()
	cfg, err := loadCursorHookConfig(path)
	if err != nil {
		return false, err
	}
	if !removeCursorHooks(&cfg) {
		return false, nil
	}
	return true, saveCursorHookConfig(path, cfg)
}

// --- rail handoff -------------------------------------------------------------

// cursorHookClaimsPath records which transcripts the HOOK rail is already
// covering, so the transcript watcher can skip them.
//
// WHY A LEDGER AND NOT A GUESS. Both Cursor rails observe the same session: the
// hook fires as the agent works, and the watcher tails the transcript that same
// agent is appending to. Left alone they would both emit a prompt and both emit a
// file edit for one action. The hook payload names the exact transcript file it
// belongs to, so the handoff is an identity rather than a content-hash
// reconciliation after the fact.
//
// The hook rail wins because its events are strictly richer (model, real
// durations, session outcome). The watcher keeps covering everything else:
// sessions that started before enrollment, machines where the hook install
// failed, and the window after an update where Cursor has not re-read its config.
func cursorHookClaimsPath() string {
	return filepath.Join(state.StateDir(), "cursor-hook-claims.json")
}

type cursorHookClaims struct {
	// Claims maps a transcript's projects-relative key to the session id that
	// claimed it, with a timestamp for eviction.
	Claims map[string]cursorHookClaim `json:"claims"`
	V      int                        `json:"v"`
}

type cursorHookClaim struct {
	SessionID string `json:"sessionId"`
	TsMs      int64  `json:"tsMs"`
}

// A `model` field used to live on this claim, holding the last model reported
// for a session so a repeat ai_response could be suppressed before it was
// signed. Both are gone: afterAgentThought no longer mints an ai_response, and
// the one that replaced it is a per-generation usage row whose repeats are
// distinct turns. See the note in runCursorHookInner — a suppression keyed on
// (session, model) would now delete every turn's spend after the first.

const (
	cursorHookClaimsVersion = 1
	// cursorHookClaimTTL evicts claims for sessions that are long over.
	//
	// It is deliberately generous, and the asymmetry is the reason. A claim only
	// refreshes while hooks are actually firing, and an engineer can idle for
	// hours mid-session. If the TTL expires under a session whose hooks still
	// work, the watcher resumes and BOTH rails emit the rest of it — the exact
	// double-capture the ledger exists to prevent. If it is too long, the only
	// cost is that a dead session's transcript tail goes unread for a week, and
	// the hook rail already captured that session richer. Err long.
	//
	// The other half of that safety is in the watcher: it advances a skipped
	// transcript's offset to EOF, so an expiry resumes from there rather than
	// replaying the file from byte 0.
	cursorHookClaimTTL = 7 * 24 * time.Hour
)

func loadCursorHookClaims() cursorHookClaims {
	c := cursorHookClaims{Claims: map[string]cursorHookClaim{}, V: cursorHookClaimsVersion}
	data, err := os.ReadFile(cursorHookClaimsPath()) // #nosec G304 -- state dir path.
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	if c.Claims == nil {
		c.Claims = map[string]cursorHookClaim{}
	}
	return c
}

// recordCursorHookClaim marks a transcript as hook-covered.
//
// Called from the hook process, which races both the daemon and other hook
// invocations — Cursor fires several steps per turn and does not serialise them.
// The write goes through the shared buffer lock for the same reason the ai-paths
// ledger does: two hooks landing at once must not truncate each other's file.
func recordCursorHookClaim(transcriptPath, sessionID string) {
	if transcriptPath == "" {
		return
	}
	key := cursorProgressKey(transcriptPath)
	_ = sign.WithBufferLock(cursorHookClaimsPath()+".lock", func() error {
		c := loadCursorHookClaims()
		now := time.Now()
		if prev, ok := c.Claims[key]; ok && prev.SessionID == sessionID &&
			now.Sub(time.UnixMilli(prev.TsMs)) < time.Minute {
			// Refreshed within the last minute by another step of the same turn.
			// Skip the write: Cursor fires several hooks per turn and rewriting
			// this file on each one is pure churn.
			return nil
		}
		for k, v := range c.Claims {
			if now.Sub(time.UnixMilli(v.TsMs)) > cursorHookClaimTTL {
				delete(c.Claims, k)
			}
		}
		c.Claims[key] = cursorHookClaim{SessionID: sessionID, TsMs: now.UnixMilli()}
		c.V = cursorHookClaimsVersion
		data, err := json.Marshal(c)
		if err != nil {
			return nil
		}
		tmp := cursorHookClaimsPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return nil
		}
		_ = os.Rename(tmp, cursorHookClaimsPath())
		return nil
	})
}

// isCursorHookClaimed reports whether the hook rail already covers a transcript.
//
// IT MUST APPLY THE TTL ITSELF rather than trusting the ledger to have been
// pruned. Eviction happens on WRITE, and only the hook process writes — so the
// one scenario where a stale claim actually matters is precisely the scenario
// where no write will ever come again: hooks uninstalled, the binary moved out
// from under the registered command, Cursor updated and dropped its config.
// Without this check those transcripts would be skipped by the watcher forever
// and the session would be captured by neither rail, which is worse than the
// double-capture the ledger exists to prevent.
func isCursorHookClaimed(claims cursorHookClaims, key string) bool {
	c, ok := claims.Claims[key]
	if !ok {
		return false
	}
	return time.Since(time.UnixMilli(c.TsMs)) <= cursorHookClaimTTL
}
