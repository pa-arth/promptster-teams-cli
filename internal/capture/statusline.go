package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Claude Code statusline capture — the wrap/unwrap + effective-resolution logic.
//
// Claude Code renders a status line by running a configured `statusLine.command`
// on every tick, piping a JSON blob (session + model + `rate_limits`) to its
// stdin and displaying its stdout. That stdin is the ONLY channel that carries an
// engineer's own 5-hour / weekly window usage on a subscription account, so we
// capture it by WRAPPING that command: our shim reads the blob, spools the window
// reading for the watcher (claudeWindowSpoolPath), then runs the engineer's prior
// command and passes its output straight through — the statusline the engineer
// already had keeps rendering.
//
// SETTINGS LAYERS: Claude Code resolves settings across five layers, highest
// precedence first: Managed policy > command-line args > Local project
// (.claude/settings.local.json) > Project (.claude/settings.json) > User
// (~/.claude/settings.json). We only OWN the User layer, so our shim only runs
// when no higher layer defines its own statusLine. resolveEffectiveStatusLine
// computes which layer actually wins, and the doctor drift check uses it to warn
// when our shim is shadowed (a project-layer statusLine) or overwritten.

// statusLineConfig is Claude Code's statusLine settings object.
type statusLineConfig struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command,omitempty"`
	Padding *int   `json:"padding,omitempty"`
}

// shimMarker is the stable substring that identifies OUR command in a settings
// file, so enable is idempotent (never double-wraps) and doctor can tell our shim
// from a third-party one. The `statusline run` subcommand only ever exists as our
// shim, so it is a reliable self-signature regardless of the binary's path.
const shimMarker = "statusline run"

// isOurShim reports whether a statusLine command string is our wrapper.
func isOurShim(command string) bool {
	return strings.Contains(command, shimMarker)
}

// --- settings file layer plumbing --------------------------------------------

// userSettingsPath is the User-layer settings.json we own
// (CLAUDE_CONFIG_DIR or ~/.claude). claudeConfigDir already honors the override.
func userSettingsPath() string {
	return filepath.Join(claudeConfigDir(), "settings.json")
}

// managedSettingsPath is the OS-level managed-policy settings file, the highest
// precedence layer. Its location is platform-specific and it is usually absent.
func managedSettingsPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	case "windows":
		return `C:\ProgramData\ClaudeCode\managed-settings.json`
	default:
		return "/etc/claude-code/managed-settings.json"
	}
}

// projectSettingsPaths returns the Project and Local-project settings files for a
// working directory, in precedence order (Local first — it outranks Project).
// Empty dir → no project layers.
func projectSettingsPaths(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{
		filepath.Join(dir, ".claude", "settings.local.json"),
		filepath.Join(dir, ".claude", "settings.json"),
	}
}

// readStatusLine parses the statusLine object from a settings file. ok=false when
// the file is absent, unparseable, or has no statusLine key — i.e. that layer
// does not define a status line.
func readStatusLine(path string) (statusLineConfig, bool) {
	data, err := os.ReadFile(path) // #nosec G304 -- a Claude settings.json path, read-only, only the statusLine object is used.
	if err != nil {
		return statusLineConfig{}, false
	}
	var settings struct {
		StatusLine *statusLineConfig `json:"statusLine"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return statusLineConfig{}, false
	}
	if settings.StatusLine == nil {
		return statusLineConfig{}, false
	}
	return *settings.StatusLine, true
}

// EffectiveStatusLine names the layer whose statusLine actually runs, and whether
// that resolved config is our shim. layer is one of "managed", "project",
// "local-project", "user", or "" (none configured anywhere).
type EffectiveStatusLine struct {
	Layer   string
	Config  statusLineConfig
	IsShim  bool
	Present bool
}

// resolveEffectiveStatusLine walks the settings layers highest-precedence first
// and returns the winning statusLine. dir is the working directory the engineer
// runs Claude Code from (its project layers can shadow our user-layer shim);
// pass "" to consider only the machine-global layers. Command-line-arg overrides
// cannot be observed from outside a running Claude Code process, so they are
// noted in doctor copy rather than resolved here.
func resolveEffectiveStatusLine(dir string) EffectiveStatusLine {
	type candidate struct {
		layer string
		path  string
	}
	candidates := []candidate{{"managed", managedSettingsPath()}}
	for i, p := range projectSettingsPaths(dir) {
		layer := "local-project"
		if i == 1 {
			layer = "project"
		}
		candidates = append(candidates, candidate{layer, p})
	}
	candidates = append(candidates, candidate{"user", userSettingsPath()})

	for _, c := range candidates {
		if cfg, ok := readStatusLine(c.path); ok {
			return EffectiveStatusLine{
				Layer:   c.layer,
				Config:  cfg,
				IsShim:  isOurShim(cfg.Command),
				Present: true,
			}
		}
	}
	return EffectiveStatusLine{}
}

// --- prior-command storage ---------------------------------------------------

// statuslinePriorPath stores the statusLine we wrapped, so disable restores it
// verbatim and the shim knows what to run. A sentinel (Wrapped=true, Prior nil)
// records "there was no prior statusLine — we installed ours", so disable removes
// the key rather than restoring a fabricated one.
func statuslinePriorPath() string {
	return filepath.Join(state.StateDir(), "statusline-prior.json")
}

type statuslinePriorRecord struct {
	Wrapped bool              `json:"wrapped"`
	Prior   *statusLineConfig `json:"prior,omitempty"`
}

func loadStatuslinePrior() (statuslinePriorRecord, bool) {
	data, err := os.ReadFile(statuslinePriorPath()) // #nosec G304 -- fixed path under the state dir.
	if err != nil {
		return statuslinePriorRecord{}, false
	}
	var rec statuslinePriorRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return statuslinePriorRecord{}, false
	}
	return rec, true
}

func saveStatuslinePrior(rec statuslinePriorRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	dir := filepath.Dir(statuslinePriorPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := statuslinePriorPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statuslinePriorPath())
}

func clearStatuslinePrior() { _ = os.Remove(statuslinePriorPath()) }

// --- settings.json mutation (preserving unknown keys) ------------------------

// readSettingsMap loads a settings.json as a generic map so unknown keys survive
// a round-trip. A missing file yields an empty map (a fresh settings file).
func readSettingsMap(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- a Claude settings.json path.
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]interface{}{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]interface{}{}, nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]interface{}{}
	}
	return m, nil
}

// writeSettingsMap atomically writes a settings map back as indented JSON.
func writeSettingsMap(path string, m map[string]interface{}) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- ~/.claude is a user config dir, not secret material.
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { // #nosec G306 -- settings.json is user config, world-readable by design.
		return err
	}
	return os.Rename(tmp, path)
}

// shimCommand is the statusLine command we install: the running binary invoked
// with `statusline run`. Quoted so a path with spaces still execs.
func shimCommand() string {
	return fmt.Sprintf("%q statusline run", state.SelfBin())
}

// StatuslineEnableResult reports what enable did, for the CLI to render an
// honest, consented message.
type StatuslineEnableResult struct {
	// AlreadyEnabled: our shim was already installed and pointed at a valid prior
	// — nothing changed.
	AlreadyEnabled bool
	// WrappedExisting: an engineer's own statusLine was present and is now
	// wrapped (it still renders). PriorCommand names it for the disclosure.
	WrappedExisting bool
	PriorCommand    string
	// InstalledFresh: no statusLine existed; ours was installed to render the
	// engineer's own 5h/weekly %.
	InstalledFresh bool
	// Rewrapped: the wrapped command changed since we last wrapped (the engineer
	// swapped their statusLine); we re-wrapped the NEW one rather than dropping it.
	Rewrapped bool
}

// EnableStatusline wraps the engineer's User-layer statusLine command with our
// shim (or installs ours if none), storing the prior verbatim. Idempotent:
// re-running with our shim already in place is a no-op unless the underlying
// command changed, in which case it re-wraps the new one. It only ever touches
// the User layer we own — a project/managed layer that shadows us is a doctor
// concern, not something enable silently rewrites.
func EnableStatusline() (StatuslineEnableResult, error) {
	path := userSettingsPath()
	m, err := readSettingsMap(path)
	if err != nil {
		return StatuslineEnableResult{}, fmt.Errorf("read %s: %w", path, err)
	}

	current, hasCurrent := readStatusLine(path)

	// Already our shim.
	if hasCurrent && isOurShim(current.Command) {
		// Ensure the stored prior is still coherent; if not, record the sentinel.
		if _, ok := loadStatuslinePrior(); !ok {
			_ = saveStatuslinePrior(statuslinePriorRecord{Wrapped: true})
		}
		return StatuslineEnableResult{AlreadyEnabled: true}, nil
	}

	res := StatuslineEnableResult{}
	prior := statuslinePriorRecord{Wrapped: true}
	if hasCurrent {
		// An engineer's own (or third-party) statusLine — wrap it.
		p := current
		prior.Prior = &p
		res.PriorCommand = current.Command
		// If we had wrapped a DIFFERENT command before, this is a re-wrap.
		if old, ok := loadStatuslinePrior(); ok && old.Prior != nil && old.Prior.Command != current.Command {
			res.Rewrapped = true
		} else {
			res.WrappedExisting = true
		}
	} else {
		res.InstalledFresh = true
	}

	if err := saveStatuslinePrior(prior); err != nil {
		return StatuslineEnableResult{}, fmt.Errorf("store prior statusLine: %w", err)
	}

	shim := statusLineConfig{Type: "command", Command: shimCommand()}
	if hasCurrent && current.Padding != nil {
		shim.Padding = current.Padding
	}
	m["statusLine"] = statusLineToMap(shim)
	if err := writeSettingsMap(path, m); err != nil {
		return StatuslineEnableResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	return res, nil
}

// DisableStatusline restores the wrapped statusLine verbatim, or removes the key
// if we had installed ours where none existed. A round-trip enable→disable leaves
// the statusLine key byte-equivalent to its pre-enable state.
func DisableStatusline() error {
	path := userSettingsPath()
	m, err := readSettingsMap(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	current, hasCurrent := readStatusLine(path)
	// Only touch the slot if it is actually our shim — never clobber a statusLine
	// the engineer set after us.
	if hasCurrent && !isOurShim(current.Command) {
		clearStatuslinePrior()
		return nil
	}

	rec, ok := loadStatuslinePrior()
	switch {
	case ok && rec.Prior != nil:
		m["statusLine"] = statusLineToMap(*rec.Prior)
	default:
		// We installed ours (or lost the record) — remove the key entirely.
		delete(m, "statusLine")
	}
	if err := writeSettingsMap(path, m); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	clearStatuslinePrior()
	return nil
}

// statusLineToMap renders a statusLineConfig as a map with only the set keys, so
// a restored prior does not gain a `padding: null` it never had (byte-equivalence
// for the disable round-trip).
func statusLineToMap(c statusLineConfig) map[string]interface{} {
	out := map[string]interface{}{}
	if c.Type != "" {
		out["type"] = c.Type
	}
	if c.Command != "" {
		out["command"] = c.Command
	}
	if c.Padding != nil {
		out["padding"] = *c.Padding
	}
	return out
}

// StatuslineDoctorLine is one diagnostic about statusline capture health.
type StatuslineDoctorLine struct {
	OK   bool
	Warn bool
	Text string
}

// StatuslineDoctor resolves the EFFECTIVE statusline across all layers for the
// given working dir and returns human-readable diagnostics — the drift check.
// It catches (a) our shim being overwritten in the user layer and (b) a
// project/managed layer shadowing it, either of which means capture won't run.
func StatuslineDoctor(dir string) []StatuslineDoctorLine {
	eff := resolveEffectiveStatusLine(dir)
	_, weAreInstalled := loadStatuslinePrior()

	switch {
	case !eff.Present && !weAreInstalled:
		return []StatuslineDoctorLine{{
			Warn: true,
			Text: "Claude window capture off — run `promptster-teams statusline enable` to track your 5h/weekly usage",
		}}
	case eff.IsShim:
		// Our shim wins — capture will run.
		return []StatuslineDoctorLine{{
			OK:   true,
			Text: fmt.Sprintf("Claude window capture active (statusline shim, %s layer)", eff.Layer),
		}}
	case weAreInstalled && eff.Present && !eff.IsShim && eff.Layer == "user":
		// Our record says we wrapped, but the user-layer statusLine is no longer
		// our shim — something overwrote it.
		return []StatuslineDoctorLine{{
			Warn: true,
			Text: "Claude window capture displaced — your statusLine was overwritten; re-enable with `promptster-teams statusline enable`",
		}}
	case weAreInstalled && eff.Present && !eff.IsShim:
		// A higher-precedence layer shadows our user-layer shim.
		return []StatuslineDoctorLine{{
			Warn: true,
			Text: fmt.Sprintf("Claude window capture shadowed — a %s statusLine overrides ours here; capture won't run in this project. Re-enable or move the shim: `promptster-teams statusline enable`", eff.Layer),
		}}
	default:
		return []StatuslineDoctorLine{{
			Warn: true,
			Text: "Claude window capture off — run `promptster-teams statusline enable`",
		}}
	}
}
