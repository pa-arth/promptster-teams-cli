package cli

import (
	"fmt"
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/selfupdate"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
)

// This file answers ONE question doctor could not previously ask: is the binary
// that is capturing the same binary you are talking to?
//
// Every other line in doctor describes the foreground process. Under `npx` that
// process is a build npm downloaded seconds earlier, so "version 0.12.2" and
// "auto-update on — up to date" can both be true of a machine whose daemon has
// been running a months-old build the whole time. That exact combination cost a
// customer their entire Cursor rail and cost us the support thread that followed:
// capture was live, doctor was green, and a feature that enrolls itself at watch
// startup had simply never run.
//
// The lines below are the diagnosis. They are pure functions over already-read
// state so they can be tested on any host, and they are silent whenever they
// cannot prove something — an unknown is reported as unknown, never as agreement.

// doctorLine is a rendered diagnostic plus its severity.
type doctorLine struct {
	Text string
	Warn bool
	Err  bool
}

func (l doctorLine) glyph() string {
	switch {
	case l.Err:
		return errGlyph
	case l.Warn:
		return warnGlyph
	default:
		return okGlyph
	}
}

// captureProcessLines describes the live capture process against the binary
// printing the report.
//
// The four outcomes are deliberately distinct, because collapsing any two of
// them is how this stayed invisible:
//
//   - not running at all — doctor never said so before, and "no events" reads
//     identically to "capture is running but nothing happened";
//   - running an OLDER build — the real bug, and the only one that is silent;
//   - running an unknown build — every daemon started before this record existed,
//     which is exactly the population most likely to be stale;
//   - running a NEWER build — not a daemon problem at all: the foreground copy is
//     the stale one, and telling someone to restart capture would be wrong.
func captureProcessLines(cp capture.CaptureProcess, ours string) []doctorLine {
	if !cp.Running {
		return []doctorLine{{
			Text: "capture not running — start it with `promptster-teams start`",
			Warn: true,
		}}
	}

	where := ""
	if cp.Exe != "" {
		where = fmt.Sprintf("  running from %s", cp.Exe)
	}

	switch {
	case cp.Version == "":
		return []doctorLine{{
			Text: fmt.Sprintf("capture running (pid %d) but it did not record its build — it predates this check, so it is at least that old. Restart it: `promptster-teams stop && promptster-teams start`", cp.PID),
			Warn: true,
		}}
	case comparableVersions(cp.Version, ours) && selfupdate.IsNewer(cp.Version, ours):
		return []doctorLine{{
			Text: fmt.Sprintf("capture running (pid %d) is %s — OLDER than this binary (%s). It is missing everything shipped since, and features that install at startup never ran. Fix: `promptster-teams stop && promptster-teams start`%s", cp.PID, cp.Version, ours, where),
			Err:  true,
		}}
	case comparableVersions(cp.Version, ours) && selfupdate.IsNewer(ours, cp.Version):
		return []doctorLine{{
			Text: fmt.Sprintf("capture running (pid %d) is %s — NEWER than this binary (%s), so this copy is the stale one, not capture. Nothing to restart.%s", cp.PID, cp.Version, ours, where),
			Warn: true,
		}}
	default:
		return []doctorLine{{
			Text: fmt.Sprintf("capture running (pid %d, %s) — same build as this binary", cp.PID, cp.Version),
		}}
	}
}

// comparableVersions reports whether two version strings can be ordered at all.
// An unstamped local build ("dev", "") parses as 0.0.0, which would make every
// release look newer than a developer's own working copy and print an alarming
// line on the one machine where it means nothing.
func comparableVersions(a, b string) bool {
	for _, v := range []string{a, b} {
		if v == "" || v == "dev" {
			return false
		}
	}
	return true
}

// autostartLines describes what will happen at the NEXT login, which is a
// different question from what is happening now and has its own failure mode:
// the unit bakes an absolute path at enable time and nothing revisits it, so the
// file it names can be deleted (an `npm i -g` that removed the old layout) while
// the running daemon holds its inode and everything looks fine.
//
// capturing decides the severity of an unloaded unit, and getting that wrong in
// either direction is costly. exists is injected so the whole thing is testable
// without a real filesystem.
func autostartLines(st service.State, capturing bool, self, canonical string, exists func(string) bool) []doctorLine {
	if !st.Installed {
		return []doctorLine{{
			Text: "autostart not enabled — run `promptster-teams autostart enable` so capture survives reboots",
			Warn: true,
		}}
	}

	lines := []doctorLine{{Text: "autostart " + st.Detail}}
	switch {
	case st.Loaded:
	case capturing:
		// NOT a warning, and this is deliberate. A live watcher holds the
		// single-instance lock, so the supervisor's own spawn exits 0 and the job
		// reads as not-running — the documented SUCCESS case, reached on Linux by
		// every `stop && start` (systemd's copy exits immediately, leaving the unit
		// inactive) and on macOS between a `stop` and the next login. Capture is
		// running and the supervisor takes back over at login; crying wolf here
		// would fire on healthy machines and train people to skim past the line
		// that actually matters.
		lines[0].Text = "autostart " + st.Detail + " — capture is running outside it and it takes over at the next login"
	default:
		// Nothing is capturing AND nothing is watching for that. This is the state
		// where a machine silently stops reporting until someone next logs in.
		lines[0].Text = "autostart " + st.Detail + " — nothing is capturing and nothing will restart it before the next login"
		lines[0].Warn = true
	}

	// Silence on unknown. ProgramPath is empty on Windows by design (see
	// service_windows.go): a wrong guess here prints the most alarming line
	// doctor has on a perfectly healthy machine.
	if st.ProgramPath == "" {
		return lines
	}
	switch {
	case !exists(st.ProgramPath):
		lines = append(lines, doctorLine{
			Text: fmt.Sprintf("autostart will run %s at login — that file no longer exists, so capture will NOT come back after a reboot. Fix: `promptster-teams autostart enable`", st.ProgramPath),
			Err:  true,
		})
	case st.ProgramPath != canonical && st.ProgramPath != self:
		lines = append(lines, doctorLine{
			Text: fmt.Sprintf("autostart will run %s at login, which is neither this binary nor the managed one (%s) — re-point it with `promptster-teams autostart enable`", st.ProgramPath, canonical),
			Warn: true,
		})
	}
	return lines
}

// fileExists is autostartLines' real-filesystem probe.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
