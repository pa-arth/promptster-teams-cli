package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Cursor's hooks.json validation is ALL-OR-NOTHING, and we share the file.
//
// ~/.cursor/hooks.json is user-scope: every tool an engineer runs writes into
// the same file, the way ~/.claude/settings.json is shared. Cursor validates the
// whole document and, on ONE bad entry anywhere in it, discards the ENTIRE
// config — not the entry, not the step, the file. Observed verbatim:
//
//	[2026-08-22T01:20:39.961Z] ERROR: Invalid user config: sessionStart[1]:
//	  Invalid hook type: "". Must be "command", "prompt", or omitted
//	  (defaults to "command")
//	ERROR: Failed to parse user hooks configuration
//	                (~/Library/Application Support/Cursor/logs/*/window*/
//	                 output_*/cursor.hooks.workspaceId-*.log)
//
// One malformed entry took down all 17 hooks on that machine — ours and three
// other tools' — for two days, with no signal anywhere except that log.
//
// WE WROTE THE MALFORMED ENTRY. This is not a defence against bad neighbours;
// it is a defect that was ours. Cursor's schema lets an entry OMIT `type`
// (it defaults to "command"), so `{"command": "..."}` is legal and common.
// cursorHookCmd modelled `type` as a plain string with no omitempty, so reading
// a legal type-less entry and writing the file back turned it into
// `"type": ""` — which is invalid, which killed the file. The machine above:
//
//	Aug 18 16:36  neighbour entry reads {"command": "bash …"}      (legal)
//	Aug 18-20     our watcher round-trips the file                 (we break it)
//	Aug 20 21:41  Cursor: Invalid user config: sessionStart[1]     (all hooks off)
//
// So there are two obligations here, and the second is the one with teeth:
//
//  1. NEVER WRITE A CONFIG CURSOR WOULD REJECT. saveCursorHookConfig validates
//     what it is about to persist and refuses rather than writing it. A guard on
//     the READ side would not have caught this — the file we read was fine.
//  2. Repair the damage already on disk, since the fleet has been writing
//     `"type": ""` for as long as the struct has looked like that.
//
// THE VALIDATOR IS DELIBERATELY ONE-DIRECTIONAL, and this is the part to keep if
// anything here is rewritten. It answers "can I PROVE Cursor rejects this?",
// never "do I recognise this?". A hook type we do not know is UNKNOWN, not
// invalid, because the alternative is a static map that lags the vendor's next
// release and starts condemning entries that work — the same failure as every
// hardcoded support table that outlived its vendor. Being wrong in that
// direction would have us "fixing" a working hook. Being wrong in the other
// direction only makes us quiet.
//
// Transcribed from Cursor 3.12.17's own validator (workbench.desktop.main.js,
// module "hooksConfig.ts", functions aKu/cKu/rKu/TUs). Re-read it there before
// changing a rule; it is the only authority, and the doc pages disagree with it.
type cursorHookVerdict int

const (
	cursorEntryValid cursorHookVerdict = iota
	// cursorEntryInvalid means Cursor is provably rejecting the whole file.
	cursorEntryInvalid
	// cursorEntryUnknown means we cannot tell. Never repaired, never counted as
	// healthy — reported as the remaining explanation when nothing else fits.
	cursorEntryUnknown
)

// cursorHookDefect is one entry Cursor can be proven to reject.
type cursorHookDefect struct {
	Step   string
	Index  int
	Reason string
	// Fixable marks the one defect we know how to repair without inventing an
	// engineer's intent: see repairCursorHookConfig.
	Fixable bool
}

func (d cursorHookDefect) String() string {
	return fmt.Sprintf("%s[%d]: %s", d.Step, d.Index, d.Reason)
}

// validateCursorHookEntry classifies a single hook entry.
func validateCursorHookEntry(c cursorHookCmd) (cursorHookVerdict, string) {
	if c.notObject != nil {
		return cursorEntryInvalid, "hook script must be an object"
	}
	switch {
	case !c.has("type"):
		// Legal: Cursor defaults a type-less entry to "command".
		return validateCursorCommandHook(c)
	case !c.isString("type"):
		return cursorEntryInvalid, `"type" must be a string`
	case strings.TrimSpace(c.Type) == "":
		// Safe to call invalid in any future version: an empty type can never
		// name a hook kind. This is the one we manufactured.
		return cursorEntryInvalid, `"type" is empty — Cursor accepts "command", "prompt", or no "type" key at all`
	case c.Type == "command":
		return validateCursorCommandHook(c)
	case c.Type == "prompt":
		return validateCursorPromptHook(c)
	default:
		// Today's Cursor rejects this too. We decline to say so: the set of hook
		// types is the vendor's to grow, and a wrong "invalid" here would have us
		// repairing a hook that works. See the one-directional note above.
		return cursorEntryUnknown, fmt.Sprintf("hook type %q is not one we know how to check", c.Type)
	}
}

// validateCursorCommandHook checks a command hook. Cursor requires `command` to
// be a string and checks nothing else about it — an EMPTY command passes its
// validator, so we must not invent a stricter rule than the one that decides
// whether the file loads.
func validateCursorCommandHook(c cursorHookCmd) (cursorHookVerdict, string) {
	if !c.has("command") {
		return cursorEntryInvalid, `a command hook needs a "command" property`
	}
	if !c.isString("command") {
		return cursorEntryInvalid, `"command" must be a string`
	}
	return validateCursorCommonFields(c)
}

func validateCursorPromptHook(c cursorHookCmd) (cursorHookVerdict, string) {
	if !c.has("prompt") || !c.isString("prompt") {
		return cursorEntryInvalid, `a prompt hook needs a "prompt" property (string)`
	}
	if strings.TrimSpace(c.Prompt) == "" {
		return cursorEntryInvalid, `prompt hook "prompt" cannot be empty`
	}
	if c.has("model") {
		if !c.isString("model") {
			return cursorEntryInvalid, `prompt hook "model" must be a string if provided`
		}
		if strings.TrimSpace(c.Model) == "" {
			return cursorEntryInvalid, `prompt hook "model" cannot be an empty string`
		}
	}
	return validateCursorCommonFields(c)
}

// validateCursorCommonFields checks the optional fields Cursor applies to both
// hook kinds.
//
// `matcher` IS CHECKED FOR STRINGNESS ONLY, DELIBERATELY. Cursor additionally
// compiles it with JavaScript's RegExp; Go's regexp is RE2 and rejects
// constructs JS accepts (backreferences, lookahead), so compiling it here would
// condemn matchers that Cursor loads happily. A validator in a different regex
// dialect than the thing it is predicting is worse than no validator.
func validateCursorCommonFields(c cursorHookCmd) (cursorHookVerdict, string) {
	if c.has("matcher") && !c.isString("matcher") {
		return cursorEntryInvalid, `"matcher" must be a string if provided`
	}
	if c.has("timeout") {
		n, ok := c.number("timeout")
		if !ok {
			return cursorEntryInvalid, `"timeout" must be a number (seconds)`
		}
		if n <= 0 {
			return cursorEntryInvalid, `"timeout" must be a positive number`
		}
	}
	if c.has("loop_limit") && !c.isNull("loop_limit") {
		n, ok := c.number("loop_limit")
		if !ok || n != float64(int64(n)) || n <= 0 {
			return cursorEntryInvalid, `"loop_limit" must be a positive integer or null`
		}
	}
	if c.has("failClosed") && !c.isBool("failClosed") {
		return cursorEntryInvalid, `"failClosed" must be a boolean`
	}
	return cursorEntryValid, ""
}

// cursorHookConfigDefects returns everything provably wrong with a config, plus
// the count of entries we could not judge.
//
// The version check is Cursor's: a config whose `version` is absent, non-numeric
// or below 1 is rejected exactly as harshly as a bad entry. Ours is written by
// saveCursorHookConfig, so the only way to meet a bad one is a foreign write.
func cursorHookConfigDefects(cfg cursorHookConfig) (defects []cursorHookDefect, unknown int) {
	if cfg.Version < 1 {
		defects = append(defects, cursorHookDefect{
			Step:    "version",
			Index:   -1,
			Reason:  "config version must be a positive integer",
			Fixable: true,
		})
	}
	for _, step := range sortedCursorSteps(cfg.Hooks) {
		for i, e := range cfg.Hooks[step] {
			verdict, reason := validateCursorHookEntry(e)
			switch verdict {
			case cursorEntryInvalid:
				defects = append(defects, cursorHookDefect{
					Step:    step,
					Index:   i,
					Reason:  reason,
					Fixable: cursorHookDropTypeRepairs(e),
				})
			case cursorEntryUnknown:
				unknown++
			}
		}
	}
	return defects, unknown
}

func sortedCursorSteps(hooks map[string][]cursorHookCmd) []string {
	steps := make([]string, 0, len(hooks))
	for s := range hooks {
		steps = append(steps, s)
	}
	// Sorted so the reported defect list is stable across runs; a diagnostic that
	// reorders itself reads as a changing problem.
	sort.Strings(steps)
	return steps
}

// cursorHookDropTypeRepairs reports whether deleting a meaningless `type` key
// makes the entry valid.
//
// This is the ONLY repair we perform on an entry, and the narrowness is the
// point. Deleting the key is not a guess about intent: Cursor documents a
// type-less entry as meaning "command", so removing an empty `type` restores the
// vendor's own default rather than inventing a value. Anything else — an entry
// with no `command`, a numeric `timeout`, a `prompt` hook missing its prompt —
// needs a decision about what a DIFFERENT tool's hook is supposed to do, and we
// do not get to make that call on someone else's config. Those are reported and
// left alone; see CursorHooksDoctor.
func cursorHookDropTypeRepairs(c cursorHookCmd) bool {
	if c.notObject != nil || !c.has("type") {
		return false
	}
	if v, _ := validateCursorHookEntry(c); v != cursorEntryInvalid {
		return false
	}
	stripped := c.withoutKey("type")
	v, _ := validateCursorHookEntry(stripped)
	return v == cursorEntryValid
}

// cursorHookRepair is one edit we made to a file we do not own.
type cursorHookRepair struct {
	TsMs   int64  `json:"tsMs"`
	Step   string `json:"step"`
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	Action string `json:"action"`
}

// repairCursorHookConfig applies every safe repair and returns what it did.
//
// It NEVER deletes an entry. The tempting argument for deletion is that a
// rejected file runs nothing, so removing the offender costs its owner nothing
// and revives everyone else — which is true, and still not worth it: it is only
// true while our validator is right, and the cost of being wrong is a working
// hook silently deleted from a config its owner never suspected we touch. Down
// and loud beats quietly destructive.
func repairCursorHookConfig(cfg *cursorHookConfig) []cursorHookRepair {
	var repairs []cursorHookRepair
	now := time.Now().UnixMilli()

	if cfg.Version < 1 {
		cfg.Version = 1
		repairs = append(repairs, cursorHookRepair{
			TsMs: now, Step: "version", Index: -1,
			Reason: "config version must be a positive integer",
			Action: "set version to 1",
		})
	}

	for _, step := range sortedCursorSteps(cfg.Hooks) {
		entries := cfg.Hooks[step]
		for i := range entries {
			if !cursorHookDropTypeRepairs(entries[i]) {
				continue
			}
			_, reason := validateCursorHookEntry(entries[i])
			entries[i] = entries[i].withoutKey("type")
			repairs = append(repairs, cursorHookRepair{
				TsMs: now, Step: step, Index: i,
				Reason: reason,
				Action: `removed the "type" key so Cursor's default ("command") applies`,
			})
		}
		cfg.Hooks[step] = entries
	}
	return repairs
}

// --- repair record ------------------------------------------------------------

// cursorHookRepairLogPath records repairs where a HUMAN can find them later.
//
// A repair is invisible by construction: it happens at daemon start, it fixes
// the file, and afterwards nothing on disk says it ever happened. That is the
// same silence this whole defect was made of, so the record is not optional —
// it is what lets `status` say "we edited your neighbour's hook entry" days
// later. It lives in OUR state dir, not next to hooks.json: ~/.cursor belongs to
// Cursor, and the one file of ours that has any business in it is hooks.json.
func cursorHookRepairLogPath() string {
	return filepath.Join(state.StateDir(), "cursor-hook-repairs.json")
}

// cursorHookBackupPath holds the file exactly as it was before our first repair.
func cursorHookBackupPath() string {
	return filepath.Join(state.StateDir(), "cursor-hooks-prerepair.json")
}

type cursorHookRepairLog struct {
	V       int                `json:"v"`
	Path    string             `json:"path"`
	Backup  string             `json:"backup"`
	Repairs []cursorHookRepair `json:"repairs"`
}

const (
	cursorHookRepairLogVersion = 1
	// cursorHookRepairLogMax bounds the log. Repairs should be one-shot, but a
	// neighbour that rewrites its broken entry on every launch would otherwise
	// grow this file forever — and an unbounded diagnostic is one nobody reads.
	cursorHookRepairLogMax = 50
)

func loadCursorHookRepairLog() cursorHookRepairLog {
	l := cursorHookRepairLog{V: cursorHookRepairLogVersion}
	data, err := os.ReadFile(cursorHookRepairLogPath()) // #nosec G304 -- state dir path.
	if err != nil {
		return l
	}
	_ = json.Unmarshal(data, &l)
	return l
}

// recordCursorHookRepairs appends to the log and preserves the pre-repair file.
// Best-effort throughout: failing to write a diagnostic must never stop the
// repair it describes.
func recordCursorHookRepairs(hooksPath string, original []byte, repairs []cursorHookRepair) {
	if len(repairs) == 0 {
		return
	}
	if err := os.MkdirAll(state.StateDir(), 0o700); err != nil {
		return
	}
	backup := cursorHookBackupPath()
	if _, err := os.Stat(backup); os.IsNotExist(err) && len(original) > 0 {
		// Only the FIRST repair writes a backup: it is the last copy of the file
		// as its owners left it, and a later repair overwriting it with our own
		// output would quietly destroy the thing it exists to preserve.
		// #nosec G703 -- state-dir path with a constant basename. The taint gosec
		// follows is state.StateDir() itself, which reads the active-workspace
		// pointer file; every state file this package writes shares that flow, and
		// nothing from hooks.json reaches the path — only the bytes.
		_ = os.WriteFile(backup, original, 0o600)
	}

	l := loadCursorHookRepairLog()
	l.V = cursorHookRepairLogVersion
	l.Path = hooksPath
	l.Backup = backup
	l.Repairs = append(l.Repairs, repairs...)
	if n := len(l.Repairs); n > cursorHookRepairLogMax {
		l.Repairs = l.Repairs[n-cursorHookRepairLogMax:]
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return
	}
	tmp := cursorHookRepairLogPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, cursorHookRepairLogPath())
}
