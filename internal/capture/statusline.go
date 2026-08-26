package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
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
	// Heals counts CONSECUTIVE automatic re-wraps (see RehealStatusline). It
	// exists only to bound a fight with another tool that also rewrites the slot,
	// and it resets to zero the moment a check finds our shim still in place —
	// which is what separates "something evicts us every single check" from "the
	// engineer re-ran another tool's setup once".
	Heals int `json:"heals,omitempty"`
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

// saveStatuslinePrior records a CHANGED prior. It drops the last-good cache,
// because serving the OLD statusline's output as a fallback for a NEW one is
// exactly the kind of quiet substitution that cache exists to prevent.
//
// Updating only the heal counter is not a prior change and must not clear the
// cache — that path calls writeStatuslinePriorFile directly.
func saveStatuslinePrior(rec statuslinePriorRecord) error {
	clearStatuslineLastGood()
	return writeStatuslinePriorFile(rec)
}

func writeStatuslinePriorFile(rec statuslinePriorRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	dir := filepath.Dir(statuslinePriorPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A per-write temp, not a shared `.tmp` path: several statusline shim
	// processes run at once on a machine with several Claude Code sessions open,
	// and a single shared temp name lets two of them interleave into one file
	// that then gets renamed into place. Exactly the defect already fixed for the
	// window spool (see writeClaudeWindowSpool) — same shape, same fix.
	return writeFileAtomic(dir, statuslinePriorPath(), data)
}

// clearStatuslinePrior drops the wrap record and the cached render together —
// once we are no longer wrapping, a remembered line belongs to nobody.
func clearStatuslinePrior() {
	_ = os.Remove(statuslinePriorPath())
	clearStatuslineLastGood()
}

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
	// Per-write temp for the same reason as the prior record — and this one
	// matters more now that the file has two writers (a foreground `statusline
	// enable` and the daemon), not just one.
	return writeFileAtomicMode(filepath.Dir(path), path, data, 0o644) // #nosec G306 -- settings.json is user config, world-readable by design.
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
	var res StatuslineEnableResult
	err := withStatuslineLock(func() error {
		var err error
		res, err = enableStatuslineLocked()
		return err
	})
	return res, err
}

func enableStatuslineLocked() (StatuslineEnableResult, error) {
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

// StatuslineWrapped reports whether the user-scope statusLine is OUR shim right
// now.
//
// It exists so `uninstall` can say what it actually changed rather than what it
// attempted. DisableStatusline is deliberately a no-op in two cases (never
// enabled, or the engineer replaced our shim with their own line afterwards),
// and it returns nil in both — so a caller with only the error cannot tell
// "restored your statusline" from "there was nothing of ours to restore", and
// reporting the wrong one of those is how an uninstall gets accused of touching
// a config it never wrote.
func StatuslineWrapped() bool {
	cur, ok := readStatusLine(userSettingsPath())
	return ok && isOurShim(cur.Command)
}

// DisableStatusline restores the wrapped statusLine verbatim, or removes the key
// if we had installed ours where none existed. A round-trip enable→disable leaves
// the statusLine key byte-equivalent to its pre-enable state.
func DisableStatusline() error {
	return withStatuslineLock(disableStatuslineLocked)
}

func disableStatuslineLocked() error {
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
	// An ABSENT statusLine is also not ours to write. Deleting the key removes our
	// shim as surely as replacing it does, and the engineer who did it wants no
	// statusline — restoring the command we saved when we wrapped them would
	// resurrect configuration they deliberately removed. The stored prior is
	// dropped for the same reason: it describes a slot nobody is holding any more.
	//
	// Reached on every `uninstall`, not just a deliberate `statusline disable`,
	// which is what turned this from a corner into something an engineer would
	// actually hit.
	if !hasCurrent {
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
		// Our shim wins — capture will run. Two lines, not one: the first says
		// the CONFIG is right, the second says it has actually PRODUCED
		// something. Those are different claims, and only the second can tell a
		// working install from one that has simply never been ticked. A doctor
		// that reports only the configuration is how "declared true" gets read
		// as "observed true".
		return []StatuslineDoctorLine{
			{
				OK:   true,
				Text: fmt.Sprintf("Claude window capture active (statusline shim, %s layer)", eff.Layer),
			},
			claudeContextDoctorLine(time.Now()),
		}
	case weAreInstalled && eff.Present && !eff.IsShim && eff.Layer == "user":
		// Our record says we wrapped, but the user-layer statusLine is no longer
		// our shim — something overwrote it. The watcher repairs this on its own
		// within statuslineHealInterval, so the honest report is "displaced, being
		// repaired" rather than a chore for the engineer — UNLESS we have already
		// given up, in which case there is a genuine fight and only a human can
		// pick the winner.
		if rec, ok := loadStatuslinePrior(); ok && rec.Heals >= maxAutoHeals {
			return []StatuslineDoctorLine{{
				Warn: true,
				Text: "Claude window capture displaced repeatedly — another tool rewrites your statusLine on a timer, so we stopped re-wrapping. Pick one, then run `promptster-teams statusline enable`",
			}}
		}
		return []StatuslineDoctorLine{{
			Warn: true,
			Text: "Claude window capture displaced — something overwrote your statusLine; capture resumes within 5 minutes (or now, with `promptster-teams statusline enable`)",
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

// --- re-healing a displaced shim ---------------------------------------------
//
// THE SLOT HAS ONE OWNER AND SEVERAL CLAIMANTS. `statusLine` is a single key in
// a single file, and every tool that wants a status line writes it directly —
// claude-hud's `/claude-hud:setup` sets `statusLine.command` to its own command,
// full stop. So re-running another tool's setup silently evicts our shim, and
// with it every Claude window and context-window reading on that machine.
//
// The eviction is invisible from the engineer's side: their statusline looks
// FINE (it is the other tool's, rendering normally), and the only symptom is
// data that stops arriving on someone else's dashboard days later. `doctor`
// would say so, but nothing makes anyone run `doctor`. Before this, capture
// stayed dead until the next `login`.
//
// Re-wrapping is a low-harm repair ONLY because the wrap is genuinely
// transparent — the engineer's line still renders, byte for byte. If that ever
// stops being true, this heal becomes a takeover on a timer and must be removed
// with it. See runPriorStatusline in statusline_shim.go.
//
// WHAT WE WILL NOT DO, and why each one is a real case rather than a corner:
//
//   - No prior record => never enabled, or `statusline disable` cleared it.
//     Disable is the off switch, and an off switch that something reverses on a
//     timer is not an off switch.
//   - An ABSENT statusLine => the engineer deleted the key. They want NO status
//     line, and installing ours into the hole would be inventing configuration
//     they removed on purpose. Same reasoning as DisableStatusline's.
//   - A MANAGED-policy statusLine outranks the user layer we own, so re-wrapping
//     ours would change nothing except churn in their settings file. That stays
//     a doctor warning.
//   - Project layers are deliberately NOT consulted: they apply per-cwd, and the
//     daemon has no single cwd to judge from. Guessing one would make the heal
//     fire or not fire based on which repo the daemon happened to start in.

// statuslineHealInterval is how often the watcher checks the slot. It bounds how
// long capture can be dead after an eviction; it is not latency-sensitive, and a
// settings.json read is far too cheap to be worth doing on the 3s poll.
const statuslineHealInterval = 5 * time.Minute

// maxAutoHeals bounds a fight. If some other tool ALSO re-writes the slot on a
// timer, healing forever would churn the engineer's settings.json indefinitely
// and neither tool would win. Five consecutive displacements is well past any
// plausible one-off, so we stop and say so rather than keep swinging.
const maxAutoHeals = 5

// StatuslineHealResult reports what a heal attempt did, for logs and doctor.
type StatuslineHealResult struct {
	// Rewrapped: we were displaced and put the shim back, wrapping whatever had
	// taken the slot so that command still renders.
	Rewrapped bool
	// Command is what we re-wrapped — the displacing tool's statusline.
	Command string
	// GaveUp: we have been displaced maxAutoHeals times running. Something else
	// owns this slot on a timer and a human needs to pick a winner.
	GaveUp bool
}

// RehealStatusline re-wraps our shim if another tool evicted it. Safe to call on
// a timer: it is a no-op in every case above, and a no-op also RESETS the heal
// counter, so an occasional eviction never accumulates toward the give-up bound.
func RehealStatusline() (StatuslineHealResult, error) {
	var res StatuslineHealResult
	err := withStatuslineLock(func() error {
		var err error
		res, err = rehealStatuslineLocked()
		return err
	})
	return res, err
}

func rehealStatuslineLocked() (StatuslineHealResult, error) {
	rec, ok := loadStatuslinePrior()
	if !ok || !rec.Wrapped {
		return StatuslineHealResult{}, nil
	}
	if _, managed := readStatusLine(managedSettingsPath()); managed {
		return StatuslineHealResult{}, nil
	}
	cur, hasCur := readStatusLine(userSettingsPath())
	if !hasCur {
		return StatuslineHealResult{}, nil
	}
	if isOurShim(cur.Command) {
		// In place. Whatever fight there may have been is over — a heal counter
		// that only ever climbs would eventually refuse to repair a machine that
		// is not being contested at all.
		if rec.Heals != 0 {
			rec.Heals = 0
			if err := writeStatuslinePriorFile(rec); err != nil {
				// A reset that silently fails keeps a stale count, which retires
				// the healer early on a machine nothing is contesting.
				return StatuslineHealResult{}, fmt.Errorf("could not reset the heal counter: %w", err)
			}
		}
		return StatuslineHealResult{}, nil
	}

	if rec.Heals >= maxAutoHeals {
		return StatuslineHealResult{GaveUp: true, Command: cur.Command}, nil
	}

	// Displaced. Wrap whatever holds the slot now — EnableStatusline stores it as
	// the new prior, so the displacing tool's line keeps rendering.
	healed := rec.Heals + 1
	if _, err := enableStatuslineLocked(); err != nil {
		return StatuslineHealResult{}, err
	}
	// enableStatuslineLocked writes a fresh record (Heals zeroed, which is right
	// for an engineer-initiated enable), so the count is restored after it, not
	// before.
	//
	// A failure here is REPORTED, not swallowed. Leaving Heals at zero silently
	// un-bounds the fight this counter exists to bound: a competing tool would be
	// re-wrapped every five minutes forever and never reach the give-up limit,
	// which is the exact failure the limit was added to prevent.
	next, ok := loadStatuslinePrior()
	if !ok {
		return StatuslineHealResult{Rewrapped: true, Command: cur.Command},
			fmt.Errorf("re-wrapped, but the heal counter could not be read back — the give-up limit is not being counted")
	}
	next.Heals = healed
	if err := writeStatuslinePriorFile(next); err != nil {
		return StatuslineHealResult{Rewrapped: true, Command: cur.Command},
			fmt.Errorf("re-wrapped, but the heal counter could not be stored (%w) — the give-up limit is not being counted", err)
	}
	return StatuslineHealResult{Rewrapped: true, Command: cur.Command}, nil
}

// statuslineHealer holds the heal throttle for one watcher. Zero value is ready
// to use and checks on its first tick.
type statuslineHealer struct {
	lastCheck time.Time
}

func (s *statuslineHealer) maybe(now time.Time) {
	if !s.lastCheck.IsZero() && now.Sub(s.lastCheck) < statuslineHealInterval {
		return
	}
	s.lastCheck = now
	res, err := RehealStatusline()
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "claude-watcher: could not re-wrap the displaced statusline: %v\n", err)
	case res.GaveUp:
		fmt.Fprintf(os.Stderr, "claude-watcher: statusline displaced %d times running — leaving it alone; run `promptster-teams statusline enable` once whatever else writes it has settled\n", maxAutoHeals)
	case res.Rewrapped:
		fmt.Fprintf(os.Stderr, "claude-watcher: statusline had been replaced — re-wrapped it, and %s still renders\n", sanitizeForLog(res.Command))
	}
}

// sanitizeForLog renders a statusline command safe to write to the daemon log.
//
// The string is arbitrary content from settings.json, and it reaches stderr — a
// file people read in a terminal. Three separate problems, and truncation alone
// solved only the first:
//
//   - LENGTH: a real statusline command is a 500-character shell pipeline
//     (claude-hud's is 526) and it would be logged on every heal.
//   - CONTROL BYTES: a newline or carriage return lets the command forge extra
//     log lines, and an ESC sequence repaints or hides text in the terminal of
//     whoever reads the log. Both are escaped, not stripped, so what was there is
//     still visible as itself.
//   - SECRETS: statusline commands do carry inline tokens (`--api-key=…`), and a
//     daemon log outlives the tick. Truncating to a 60-byte prefix is what keeps
//     this bounded — enough to recognise WHICH tool took the slot, which is all
//     the log needs to say, and far too little to be a usable credential dump.
//     If this ever needs to log more of the command, it needs redaction first.
func sanitizeForLog(cmd string) string {
	const max = 60
	truncated := false
	if len(cmd) > max {
		cmd, truncated = cmd[:max], true
	}
	var b strings.Builder
	for _, r := range cmd {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	if truncated {
		b.WriteString("…")
	}
	return b.String()
}

// --- the mutation lock -------------------------------------------------------

// statuslineLockPath guards every mutation of the statusLine slot and its prior
// record. One path, one lock, all writers.
func statuslineLockPath() string {
	return filepath.Join(state.StateDir(), "statusline.lock")
}

// withStatuslineLock serialises enable / disable / reheal ACROSS PROCESSES.
//
// The race it closes is not theoretical and it runs the wrong way: `statusline
// disable` is a foreground command, the healer runs in the daemon, and between
// the healer clearing its gates and calling enable, a disable can complete. The
// healer then writes the shim back over a slot the engineer just turned off —
// an off switch reversed by a background process, which is the one outcome the
// heal design says must never happen.
//
// It is a BLOCKING lock. Nothing here is on a latency path (the shim's own hot
// path takes no lock at all), and the alternative — try-lock and skip — would
// mean a heal silently declining to repair, which is the failure we are fixing.
//
// RE-ENTRANCY: flock blocks against a second fd on the same file from the SAME
// process, so a locked function must never call another locked function. That is
// why the public entry points are thin wrappers and every body is `...Locked`:
// rehealStatuslineLocked calls enableStatuslineLocked, never EnableStatusline.
func withStatuslineLock(fn func() error) error {
	return sign.WithBufferLock(statuslineLockPath(), fn)
}
