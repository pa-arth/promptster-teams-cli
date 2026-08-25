package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

	// THE WHOLE-FILE VERDICT COMES FIRST, because it outranks everything below
	// it: while Cursor is rejecting the file, our enrollment is irrelevant — none
	// of it runs, and neither does any other tool's. Reporting "enrolled for all
	// 8 steps" on a machine whose hooks.json Cursor threw away is precisely the
	// false green this check exists to end.
	//
	// It is reported here as well as repaired at watch startup because THE DAEMON
	// IS A DIFFERENT BINARY. A fleet machine running a months-old daemon never
	// executes the repair, and doctor — typed at a fresh binary while something
	// is already wrong — is the only place that machine hears about it.
	// TWO ACCUMULATORS, BECAUSE THE TAIL BELOW GATES ON "DID I FIND A PROBLEM?"
	// AND USED TO ASK IT AS "IS THE LIST NON-EMPTY?".
	//
	// Those are different questions the moment any line is informational. The
	// repair note is: it says something we did, not something wrong. Appended to
	// the same slice it silently satisfied `len(lines) > 0`, and every machine
	// that had ever been repaired lost its enrollment verdict AND its usage
	// coverage numbers from then on — permanently, since the repair record is
	// permanent. Caught in review on PR #173.
	var problems []CursorHookDoctorLine // something is wrong; suppresses the healthy tail
	var notes []CursorHookDoctorLine    // always rendered, gates nothing
	// finish composes a report: problems, then notes, then whatever the caller
	// concluded. Every return goes through it so a new line cannot be added to
	// one exit and forgotten at the other four.
	finish := func(extra ...CursorHookDoctorLine) []CursorHookDoctorLine {
		out := make([]CursorHookDoctorLine, 0, len(problems)+len(notes)+len(extra))
		out = append(out, problems...)
		out = append(out, notes...)
		return append(out, extra...)
	}

	defects, unknown := cursorHookConfigDefects(cfg)
	if len(defects) > 0 {
		reasons := make([]string, 0, len(defects))
		fixable := true
		for _, d := range defects {
			reasons = append(reasons, d.String())
			if !d.Fixable {
				fixable = false
			}
		}
		remedy := "an entry must be corrected by hand before ANY hook in that file runs again"
		if fixable {
			remedy = "restart capture (`promptster-teams stop` then `promptster-teams start`) to repair it"
		}
		problems = append(problems, CursorHookDoctorLine{
			Err: true,
			Text: fmt.Sprintf(
				"Cursor is rejecting ALL of ~/.cursor/hooks.json (%s) — every hook in it is off, other tools' included; %s",
				strings.Join(reasons, "; "), remedy),
		})
	}
	if l, ok := cursorHookRepairLine(); ok {
		notes = append(notes, l)
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
		// Nothing of ours is registered, so there is no command to validate. Any
		// whole-file verdict above still stands and is returned with it.
		return finish(CursorHookDoctorLine{
			Warn: true,
			Text: "Cursor hook not enrolled — start capture (`promptster-teams start`) to enroll it; transcript capture still works, without model attribution",
		})
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
		if bin == "" || !isRunnable(bin) {
			dangling = append(dangling, bin)
		}
	}

	if len(dangling) > 0 {
		sort.Strings(dangling)
		problems = append(problems, CursorHookDoctorLine{
			Err: true,
			Text: fmt.Sprintf(
				"Cursor is running a command it cannot execute (%s) on every prompt, edit and shell call — run `promptster-teams uninstall` to unenroll this machine, or reinstall to the managed path",
				state.HomeRelative(strings.Join(dangling, ", ")),
			),
		})
	}
	if len(missing) > 0 {
		// A partial enrollment is what a hand-edited hooks.json looks like. The
		// watcher repairs it at the next startup, so this is a warning, not an
		// error — but it is named because the missing steps are silently absent
		// signals, not a loud failure.
		problems = append(problems, CursorHookDoctorLine{
			Warn: true,
			Text: fmt.Sprintf("Cursor hook enrolled for only some steps (missing: %s) — restart capture to repair", strings.Join(missing, ", ")),
		})
	}
	if len(problems) > 0 {
		return finish()
	}

	// Enrolled everywhere and runnable. The only thing left is whether it points
	// at the managed path — an old install layout or a hand-edit does not, and
	// `watch` re-renders the command unconditionally at startup, so that is
	// information rather than a warning.
	canonical := cursorHookCommandBinary(cursorHookCommand())
	for bin := range bins {
		if filepath.Clean(bin) != filepath.Clean(canonical) {
			offPath := CursorHookDoctorLine{
				OK: true,
				Text: fmt.Sprintf("Cursor hook enrolled, pointed at %s (not the managed path) — capture restart re-points it",
					state.HomeRelative(bin)),
			}
			if l, ok := cursorUsageCoverageLine(); ok {
				return finish(offPath, l)
			}
			return finish(offPath)
		}
	}

	healthy := fmt.Sprintf("Cursor hook enrolled for all %d steps in ~/.cursor/hooks.json", len(cursorHookSteps))
	if unknown > 0 {
		// We found nothing provably wrong, but we did not check everything: our
		// validator refuses to judge a hook type it does not recognise, so that it
		// can never condemn one the vendor added after this binary shipped. Say so
		// rather than let the OK line imply a completeness it does not have — if
		// Cursor IS rejecting this file, these entries are where to look.
		healthy += fmt.Sprintf(" (%d entr%s there use a hook type this build cannot check)",
			unknown, map[bool]string{true: "y", false: "ies"}[unknown == 1])
	}
	healthyLine := CursorHookDoctorLine{OK: true, Text: healthy}
	if l, ok := cursorUsageCoverageLine(); ok {
		return finish(healthyLine, l)
	}
	return finish(healthyLine)
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
//  2. THE NON-CUMULATIVE PREMISE. usageEvent tags every row `usageScope:
//     "request"`, which is a MEASUREMENT (Cursor 3.12.17, 2026-08-18: output
//     fell 902 -> 525 across consecutive generations) and not an invariant. A
//     cumulative counter cannot decrease, so each observed decrease is evidence
//     for the tag. The sibling premise on the codex rail was verified true,
//     recorded only as prose, and was 18.89% false four days later — with a live
//     customer's published spend as the cost. Prose has no expiry; a counter
//     does.
//
//     ⚠ IT IS THE ONLY THING THE TAG CLAIMS, and this line said otherwise until
//     2026-08-22. `usageScope: "request"` means "do not difference me". It does
//     NOT mean the row is one request, and on this rail it is not: Cursor's
//     stop hook emits a per-TURN sum across the turn's model calls, measured at
//     5.55x the vendor's own recorded resident context on one turn and ~75x on
//     the largest. Nothing here can check that, and the calibration above could
//     not have either — its probe turns were one to two calls each, where a
//     turn sum and a request's input are the same number. The per-request
//     reading is a separate premise, and it is refuted: see the backend's
//     TURN_SUM_INTEGRATIONS, which is why a Cursor row now publishes no
//     resident context at all.
//
//     REFUTED IF this machine reports many comparisons and zero decreases after
//     a Cursor upgrade. That is the signal to re-probe before trusting any
//     per-turn figure, not a reason to keep asserting the tag.
func cursorUsageCoverageLine() (CursorHookDoctorLine, bool) {
	c := loadCursorGenerations()
	if c.UsageRows == 0 && c.StopSeen == 0 {
		// Nothing captured yet. Saying "0 of 0 rows" would read as a problem.
		return CursorHookDoctorLine{}, false
	}
	text := fmt.Sprintf("Cursor usage rows captured: %d (%d with no model to price)",
		c.UsageRows, c.ModellessRows)
	// The denominator, when this machine has one. A build older than the
	// counters reports StopSeen 0 with real UsageRows, and printing "0 stops"
	// beside them would be a false statement rather than a missing one — so the
	// coverage clause appears only when the machine actually counted stops.
	warnCoverage := false
	if c.StopSeen > 0 {
		text += fmt.Sprintf("; %d of %d stop hooks produced one", c.UsageRows, c.StopSeen)
		if c.StopEmpty > 0 {
			text += fmt.Sprintf(" (%d turns reported nothing to price)", c.StopEmpty)
		}
		// The residual: parsed, not empty, and still no row. Should be zero, and a
		// nonzero one is OUR defect rather than an aborted turn — so it is named
		// separately from StopEmpty and it is what raises the warning.
		lost := c.StopSeen - c.StopEmpty - c.UsageRows
		if lost > 0 {
			text += fmt.Sprintf("; %d lost between the normalizer and the queue", lost)
			warnCoverage = true
		}
	}
	if c.Unparsed > 0 {
		// Everything above is conditional on a parse, so this says the numbers are
		// describing a subset of the traffic.
		text += fmt.Sprintf("; %d payloads never named a step", c.Unparsed)
		warnCoverage = true
	}
	if o := loadCursorHookOverruns(); o.Overruns > 0 {
		// Always a warning, at any count. An overrun is not a dropped
		// measurement, it is us stalling the engineer's agent for two seconds and
		// then dropping the measurement anyway.
		text += fmt.Sprintf("; %d hook invocations hit the %s budget and were abandoned",
			o.Overruns, cursorHookBudget)
		warnCoverage = true
	}
	if c.NonCumulativeComparisons > 0 {
		text += fmt.Sprintf("; per-turn counts confirmed by %d of %d output decreases",
			c.NonCumulativeDecreases, c.NonCumulativeComparisons)
	}
	// Modelless rows are a WARNING past a third of the traffic: at that point the
	// device join is not carrying the rail and the fix is a different step, not a
	// bigger cache.
	warn := c.ModellessRows*3 > c.UsageRows
	if warn {
		text += " — most rows cannot be priced; the generation model join is not covering this machine"
	}
	warn = warn || warnCoverage
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

// isRunnable reports whether Cursor could actually exec this path.
//
// PRESENT IS NOT THE SAME AS RUNNABLE, and the gap is a false green: a hook
// binary restored from an archive that dropped its mode bits, or copied by an
// installer that did not chmod it, sits there perfectly readable while Cursor
// fails to launch it on every event. The rail then reports `ok` and produces
// nothing, which is precisely the shape of failure this whole change exists to
// end. Raised in review on PR #176.
//
// THE EXECUTABLE BIT IS CHECKED ON UNIX ONLY. Windows has no such bit — Go
// reports a mode without it — so applying the test there would call every
// healthy Windows install unrunnable and print the most alarming line this
// package has, on machines that are fine. Same trap as the escaped-path scan in
// cursorHookCommandBinary, one field over.
func isRunnable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode().Perm()&0o111 != 0
}

// cursorHookRepairLine reports that we edited a file we do not own.
//
// A repair is invisible once it succeeds — the file is correct afterwards and
// nothing on disk admits we changed it. That silence is the same material this
// defect was made of, so the record outlives the repair and doctor reads it
// back. It names the untouched original too: an engineer told "we rewrote your
// hook entry" is owed the copy of what it said before.
func cursorHookRepairLine() (CursorHookDoctorLine, bool) {
	l := loadCursorHookRepairLog()
	if len(l.Repairs) == 0 {
		return CursorHookDoctorLine{}, false
	}
	last := l.Repairs[len(l.Repairs)-1]
	return CursorHookDoctorLine{
		OK: true,
		Text: fmt.Sprintf(
			"repaired %d entr%s in ~/.cursor/hooks.json that Cursor was rejecting (last: %s — %s); original kept at %s",
			len(l.Repairs),
			map[bool]string{true: "y", false: "ies"}[len(l.Repairs) == 1],
			last.Step, last.Action, state.HomeRelative(l.Backup),
		),
	}, true
}
