package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// CursorHookDoctorLine is one diagnostic about the Cursor hook rail.
//
// It carries an Err level that StatuslineDoctorLine does not, because this
// surface has a state that is worse than "capture is off": an entry naming a
// binary that no longer exists makes CURSOR run a missing command inside the
// engineer's agent loop on every prompt, edit and shell call. That is their tool
// degraded by us, not our data missing, and it must not render with the same
// glyph as "not enrolled yet".
type CursorHookDoctorLine struct {
	OK   bool
	Warn bool
	Err  bool
	Text string
}

// CursorHooksDoctor reports the state of the user-scope Cursor hook enrollment.
//
// WHY DOCTOR IS THE RIGHT PLACE. The dangling-command state is created by
// deleting the binary WITHOUT running `uninstall` — a `rm -rf
// ~/.promptster-teams`, an image rebuild, a home directory restored from backup.
// Once the binary is gone none of our code runs, so nothing of ours can detect
// or repair it from the inside; the only moment we get to say anything is when a
// working binary is next present and someone asks what is wrong. `doctor` is the
// command engineers run while something is already wrong, which makes it the one
// place the message can land.
//
// It is READ-ONLY, like the rest of doctor. It never enrolls, never repairs, and
// never writes hooks.json — `watch` startup owns enrollment, and a diagnostic
// that silently mutates the thing it is diagnosing cannot be trusted to describe
// it.
func CursorHooksDoctor() []CursorHookDoctorLine {
	path := cursorUserHooksPath()
	if !dirExists(filepath.Dir(path)) {
		// No ~/.cursor at all. Not a problem — but say so, because "why wasn't my
		// Cursor session captured" is a real support question and "Cursor is not
		// installed for this user" answers it outright.
		return []CursorHookDoctorLine{{
			OK:   true,
			Text: "Cursor not installed for this user (no ~/.cursor) — nothing to enroll",
		}}
	}

	cfg, err := loadCursorHookConfig(path)
	if err != nil {
		// loadCursorHookConfig only errors on a file that exists and does not
		// parse. Enrollment refuses to touch that file rather than clobber an
		// engineer's own config, so it stays unenrolled until a human fixes it —
		// which they will never know to do unless it is said out loud.
		return []CursorHookDoctorLine{{
			Warn: true,
			Text: fmt.Sprintf("~/.cursor/hooks.json does not parse (%v) — the Cursor hook rail is off until it does; transcript capture still works", err),
		}}
	}

	var missing []string
	bins := map[string]bool{}
	for _, step := range cursorHookSteps {
		found := false
		for _, e := range cfg.Hooks[step] {
			if !isPromptsterCursorHook(e) {
				continue
			}
			found = true
			bins[cursorHookCommandBinary(e.Command)] = true
		}
		if !found {
			missing = append(missing, step)
		}
	}

	if len(missing) == len(cursorHookSteps) {
		// Nothing of ours is registered, so there is no command to validate.
		return []CursorHookDoctorLine{{
			Warn: true,
			Text: "Cursor hook not enrolled — start capture (`promptster-teams start`) to enroll it; transcript capture still works, without model attribution",
		}}
	}

	// EVERY REMAINING ENTRY IS VALIDATED, INCLUDING UNDER A PARTIAL ENROLLMENT.
	// Completeness and runnability are independent facts, and returning early on
	// the incomplete one hides the worse one: a machine with three of eight steps
	// registered against a deleted binary still execs a missing command inside the
	// agent loop on those three steps' events. Reporting only "some steps are
	// missing" there describes the least of that machine's problems. Raised by
	// review on PR #125.
	var dangling []string
	for bin := range bins {
		if bin == "" || !fileExists(bin) {
			dangling = append(dangling, bin)
		}
	}

	var lines []CursorHookDoctorLine
	if len(dangling) > 0 {
		sort.Strings(dangling)
		lines = append(lines, CursorHookDoctorLine{
			Err: true,
			Text: fmt.Sprintf(
				"Cursor is running a command that does not exist (%s) on every prompt, edit and shell call — run `promptster-teams uninstall` to unenroll this machine, or reinstall to the managed path",
				state.HomeRelative(strings.Join(dangling, ", ")),
			),
		})
	}
	if len(missing) > 0 {
		// A partial enrollment is what a hand-edited hooks.json looks like. The
		// watcher repairs it at the next startup, so this is a warning, not an
		// error — but it is named because the missing steps are silently absent
		// signals, not a loud failure.
		lines = append(lines, CursorHookDoctorLine{
			Warn: true,
			Text: fmt.Sprintf("Cursor hook enrolled for only some steps (missing: %s) — restart capture to repair", strings.Join(missing, ", ")),
		})
	}
	if len(lines) > 0 {
		return lines
	}

	// Enrolled everywhere and runnable. The only thing left is whether it points
	// at the managed path — an old install layout or a hand-edit does not, and
	// `watch` re-renders the command unconditionally at startup, so that is
	// information rather than a warning.
	canonical := cursorHookCommandBinary(cursorHookCommand())
	for bin := range bins {
		if filepath.Clean(bin) != filepath.Clean(canonical) {
			lines = append(lines, CursorHookDoctorLine{
				OK: true,
				Text: fmt.Sprintf("Cursor hook enrolled, pointed at %s (not the managed path) — capture restart re-points it",
					state.HomeRelative(bin)),
			})
			if l, ok := cursorUsageCoverageLine(); ok {
				lines = append(lines, l)
			}
			return lines
		}
	}

	lines = append(lines, CursorHookDoctorLine{
		OK:   true,
		Text: fmt.Sprintf("Cursor hook enrolled for all %d steps in ~/.cursor/hooks.json", len(cursorHookSteps)),
	})
	if l, ok := cursorUsageCoverageLine(); ok {
		lines = append(lines, l)
	}
	return lines
}

// cursorUsageCoverageLine reports what a probe measured, after the probe is gone.
//
// TWO NUMBERS, BOTH OF WHICH ARE PREMISES THIS RAIL RESTS ON RATHER THAN
// STATISTICS ABOUT IT.
//
//  1. MODEL COVERAGE. A usage row whose generation never produced an
//     afterAgentThought is emitted with tokens and no model, and the backend
//     declines to price it. How often that happens was measured once, by a probe
//     that gets torn down; counting it here means the answer stays available on
//     any enrolled machine instead of expiring into a sentence in a spec.
//
//  2. THE PER-REQUEST PREMISE. usageEvent tags every row `usageScope:
//     "request"`, which is a MEASUREMENT (Cursor 3.12.17, 2026-08-18: output
//     fell 902 -> 525 across consecutive generations) and not an invariant. A
//     cumulative counter cannot decrease, so each observed decrease is evidence
//     for the tag. The sibling premise on the codex rail was verified true,
//     recorded only as prose, and was 18.89% false four days later — with a live
//     customer's published spend as the cost. Prose has no expiry; a counter
//     does.
//
//     REFUTED IF this machine reports many comparisons and zero decreases after
//     a Cursor upgrade. That is the signal to re-probe before trusting any
//     per-turn figure, not a reason to keep asserting the tag.
func cursorUsageCoverageLine() (CursorHookDoctorLine, bool) {
	c := loadCursorGenerations()
	if c.UsageRows == 0 {
		// Nothing captured yet. Saying "0 of 0 rows" would read as a problem.
		return CursorHookDoctorLine{}, false
	}
	text := fmt.Sprintf("Cursor usage rows captured: %d (%d with no model to price)",
		c.UsageRows, c.ModellessRows)
	if c.PerRequestComparisons > 0 {
		text += fmt.Sprintf("; per-turn counts confirmed by %d of %d output decreases",
			c.PerRequestDecreases, c.PerRequestComparisons)
	}
	// Modelless rows are a WARNING past a third of the traffic: at that point the
	// device join is not carrying the rail and the fix is a different step, not a
	// bigger cache.
	warn := c.ModellessRows*3 > c.UsageRows
	if warn {
		text += " — most rows cannot be priced; the generation model join is not covering this machine"
	}
	return CursorHookDoctorLine{OK: !warn, Warn: warn, Text: text}, true
}

// cursorHookCommandBinary extracts the program from a registered hook command.
//
// cursorHookCommand renders the path with %q, so on Windows it arrives
// backslash-ESCAPED (`"C:\\Users\\x\\bin\\promptster-teams.exe" cursor-hook`).
// Scanning naively to the next quote would hand back a path with doubled
// separators — which os.Stat would report as missing, turning a perfectly
// healthy Windows enrollment into a false "the binary is gone" alarm, the single
// most alarming line this function can print. So the quoted token is closed with
// escape awareness and unquoted properly.
func cursorHookCommandBinary(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if !strings.HasPrefix(cmd, `"`) {
		// Unquoted (a hand-written entry). Take the first field; a path with
		// spaces is unrecoverable here, and guessing would be worse than "".
		if f := strings.Fields(cmd); len(f) > 0 {
			return f[0]
		}
		return ""
	}
	for i := 1; i < len(cmd); i++ {
		switch cmd[i] {
		case '\\':
			i++ // skip the escaped byte
		case '"':
			if s, err := strconv.Unquote(cmd[:i+1]); err == nil {
				return s
			}
			return ""
		}
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
