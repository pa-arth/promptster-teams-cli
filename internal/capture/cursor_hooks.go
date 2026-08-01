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
// TOKENS ARE NOT AVAILABLE AND THIS IS SETTLED. The shipped dispatch code does
// construct input_tokens/output_tokens/cache_read_tokens/cache_write_tokens for
// the `stop` and `afterAgentResponse` steps, but neither step fires on the
// headless `cursor-agent -p` path — verified on a turn that ended
// final_status:"completed", not just on failed runs. Do not re-derive a token
// metric from this surface, and do not reintroduce those two steps below on the
// strength of reading Cursor's bundle: they were read there and they did not
// fire.
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
//   - stop / afterAgentResponse — do not fire on the headless path (see above).
//   - preToolUse — it gates the tool call: a slow or wedged hook there stalls the
//     engineer's agent before any work happens. postToolUse carries the same
//     identity fields plus the outcome, so the blocking one buys nothing.
//   - preToolUse — see above; postToolUse carries the same identity plus outcome.
//   - preCompact / subagentStart / subagentStop — real signals, but out of scope
//     for the first slice. preCompact in particular carries
//     context_usage_percent / context_tokens / context_window_size, which is the
//     one context-pressure metric this surface exposes. See the spec's follow-on.
var cursorHookSteps = []string{
	"sessionStart",
	"sessionEnd",
	"beforeSubmitPrompt",
	"afterFileEdit",
	"afterShellExecution",
	"postToolUseFailure",
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

type cursorHookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
	Model   string `json:"model,omitempty"`
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
	return c.Type == "command" && containsCursorHookSubcommand(c.Command)
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

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return cfg, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	for k, v := range raw {
		switch k {
		case "version":
			_ = json.Unmarshal(v, &cfg.Version)
		case "hooks":
			if err := json.Unmarshal(v, &cfg.Hooks); err != nil {
				return cfg, fmt.Errorf("%s has an unreadable \"hooks\" block: %w", path, err)
			}
		default:
			cfg.rest[k] = v
		}
	}
	if cfg.Hooks == nil {
		cfg.Hooks = map[string][]cursorHookCmd{}
	}
	if cfg.Version == 0 {
		cfg.Version = 1
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
			kept = append(kept, cursorHookCmd{Type: "command", Command: command})
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
	if !dirExists(filepath.Dir(cursorUserHooksPath())) {
		// No ~/.cursor at all — Cursor is not installed on this machine. Creating
		// the directory would leave a config for a product the engineer does not
		// use, so do nothing and report no change.
		return false, nil
	}
	path := cursorUserHooksPath()
	cfg, err := loadCursorHookConfig(path)
	if err != nil {
		return false, err
	}
	if !mergeCursorHooks(&cfg, cursorHookCommand()) {
		return false, nil
	}
	if err := saveCursorHookConfig(path, cfg); err != nil {
		return false, err
	}
	return true, nil
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

const (
	cursorHookClaimsVersion = 1
	// cursorHookClaimTTL evicts claims for sessions that are long over. It must
	// outlive a working session by a lot: a claim that expires while the session
	// is still running hands the transcript back to the watcher mid-flight and
	// the tail resumes from 0, re-emitting everything the hook already sent.
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
	_ = sign.WithBufferLock(cursorHookClaimsPath(), func() error {
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
func isCursorHookClaimed(claims cursorHookClaims, key string) bool {
	_, ok := claims.Claims[key]
	return ok
}
