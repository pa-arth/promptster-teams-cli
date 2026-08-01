package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// `uninstall` — undo everything this CLI installed outside its own directory,
// and with --purge the directory too.
//
// WHY IT EXISTS. Installing enrolls the machine in three places the engineer did
// not create and will not think to clean up:
//
//	~/.cursor/hooks.json          a command Cursor runs inside the agent loop
//	~/.claude/settings.json       the wrapped statusLine (only if enabled)
//	launchd / systemd / schtasks  a unit that relaunches capture at every login
//
// Nothing removed any of it, and — measured against npm 11.5.2 in a sandboxed
// global prefix, not inferred from documentation — the package manager does not
// either:
//
//   - `npm rm -g` runs NO uninstall lifecycle script. preuninstall and
//     postuninstall fired ZERO times on removal, while postinstall fired on both
//     install and upgrade. So an npm-side uninstall hook is not a fix that was
//     skipped here; it is a fix that does not exist to be wired up.
//   - `npm rm -g` leaves files that postinstall created OUTSIDE node_modules.
//     The managed binary (~/.promptster-teams/bin) is exactly such a file, by
//     design — that indirection is what keeps `npm ls` honest.
//
// Those two compose into the worst outcome available: `npm rm -g
// @promptster/teams-cli` reports success, and capture keeps running, keeps
// self-updating from GitHub Releases, and comes back at the next login, from a
// binary the engineer believes they deleted. An uninstall that the package
// manager cannot perform has to be a command in the CLI.
//
// THE CURSOR HOOK OUTRANKS ALL OF THAT even though it is the smaller-looking
// item. Delete the binary by hand — the obvious move for a curl install — and
// the hooks.json entry survives, naming a path that no longer exists, and Cursor
// execs a missing command inside the engineer's agent loop on every prompt,
// every edit, every shell call. That degrades THEIR tool rather than our data,
// which is the one failure mode in this feature we do not get to trade away.
//
// INVARIANTS:
//
//   - Every step runs even if an earlier one fails. A partial uninstall that
//     stopped at the first error is how the Cursor hook survives a removal — the
//     exact thing this command exists to prevent. Failures are reported and the
//     exit code is non-zero, but nothing short-circuits.
//   - It reports what it OBSERVED, not what it attempted. "autostart was not
//     enabled" and "autostart disabled" are different facts and an uninstall
//     that claims to have edited a config it never wrote is worse than one that
//     admits it found nothing.
//   - It is idempotent. Running it twice is a legitimate thing to do when the
//     first run reported a failure.
//   - Without --purge it deletes NO data. The key and the unsent event queue
//     survive, so an accidental uninstall costs an enrollment, not a backlog.
func cmdUninstall(args []string) int {
	purge := false
	for _, a := range args {
		switch a {
		case "--purge":
			purge = true
		case "-h", "--help":
			fmt.Println("usage: promptster-teams uninstall [--purge]")
			fmt.Println()
			fmt.Println("  Stops capture, removes the autostart unit, unenrolls the Cursor hook,")
			fmt.Println("  and restores the Claude statusline.")
			fmt.Println("  --purge also deletes ~/.promptster-teams (your key, the unsent event")
			fmt.Println("  queue, and the managed binary itself).")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "unknown uninstall flag: %s\n", a)
			fmt.Fprintln(os.Stderr, "usage: promptster-teams uninstall [--purge]")
			return 1
		}
	}
	return runUninstall(os.Stdout, defaultUninstallDeps(), purge)
}

// uninstallDeps is the seam that makes this command testable on a developer
// machine WITHOUT touching their real capture.
//
// This is not decoration. `launchctl bootout` and `launchctl bootstrap` target
// gui/$UID by LABEL, so a test that sandboxes $HOME still tears down the
// developer's actual ai.promptster.teams job — a sandboxed HOME buys nothing
// there. Same for StopTeamsDaemon, which signals whatever live watcher it finds.
// The file-touching steps (Cursor hooks, statusline, purge) DO respect $HOME, so
// the tests drive those for real and fake only the two that reach outside it.
type uninstallDeps struct {
	stopCapture       func() error
	captureRunning    func() bool
	autostartStatus   func() (bool, string, error)
	autostartDisable  func() error
	removeCursorHooks func() (bool, error)
	statuslineWrapped func() bool
	disableStatusline func() error
	// purgeDirs are the directories --purge deletes, most specific first.
	purgeDirs func() []string
	// self is the running binary, so purge can report the one file it may not be
	// able to delete (Windows cannot unlink a running image).
	self string
}

func defaultUninstallDeps() uninstallDeps {
	mgr := service.New()
	return uninstallDeps{
		stopCapture:       capture.StopTeamsDaemon,
		captureRunning:    capture.CaptureRunning,
		autostartStatus:   mgr.Status,
		autostartDisable:  mgr.Disable,
		removeCursorHooks: capture.RemoveCursorHooks,
		statuslineWrapped: capture.StatuslineWrapped,
		disableStatusline: capture.DisableStatusline,
		purgeDirs:         purgeDirs,
		self:              state.SelfBin(),
	}
}

// purgeDirs lists the state directories --purge removes. StateDir() is normally
// GlobalPromptsterDir(), but PROMPTSTER_STATE_DIR (and, in a future per-workspace
// mode, the active-workspace pointer) can move it — and that is where the outbox
// and the Cursor hook claims ledger live. Purging only the global dir there would
// leave the queue behind while reporting a clean removal.
func purgeDirs() []string {
	dirs := []string{state.GlobalPromptsterDir()}
	if sd := state.StateDir(); sd != "" && filepath.Clean(sd) != filepath.Clean(dirs[0]) {
		dirs = append([]string{sd}, dirs...)
	}
	return dirs
}

func runUninstall(out io.Writer, d uninstallDeps, purge bool) int {
	failed := false
	line := func(s string) { fmt.Fprintln(out, "  "+s) }
	ok := func(format string, a ...any) { line(okGlyph + " " + fmt.Sprintf(format, a...)) }
	skip := func(format string, a ...any) { line(dimStyle.Render("· " + fmt.Sprintf(format, a...))) }
	bad := func(format string, a ...any) {
		line(errGlyph + " " + fmt.Sprintf(format, a...))
		failed = true
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, brandBar("uninstall"))
	fmt.Fprintln(out)

	// Autostart first, and DISABLE rather than stop: disable deregisters the unit
	// so the supervisor cannot bring capture back at the next login. Doing it
	// before the kill also means the process we signal below is not one launchd
	// will read as a crash and respawn.
	//
	// A FAILED STATUS PROBE STILL ATTEMPTS THE REMOVAL. Disable is idempotent on
	// every platform (launchctl bootout's error is ignored, os.Remove tolerates
	// IsNotExist), so calling it blind costs nothing — while skipping it because
	// we could not read the status leaves the single worst residue an uninstall
	// can leave: a registered unit that brings capture back at the next login.
	// "I don't know whether it's installed" is a reason to remove it, not a reason
	// to stop.
	installed, _, statusErr := d.autostartStatus()
	switch {
	case statusErr == nil && !installed:
		skip("autostart was not enabled")
	default:
		if err := d.autostartDisable(); err != nil {
			bad("could not remove the autostart unit: %v", err)
			line(dimStyle.Render("  capture will start again at your next login until this is fixed"))
			break
		}
		if statusErr != nil {
			ok("autostart unit removed (its status could not be read: %v)", statusErr)
			break
		}
		ok("autostart unit removed — capture no longer starts at login")
	}

	// Liveness is read BEFORE and AFTER the stop, because those are two different
	// facts and only the pair says whether the uninstall worked. "stopped" printed
	// over a machine where nothing was running is a lie in the harmless direction;
	// "stopped" printed over capture that is still alive is a lie in the direction
	// that matters, and it is the one an engineer would act on.
	wasRunning := d.captureRunning()
	stopErr := d.stopCapture()
	switch {
	case stopErr != nil:
		bad("could not stop background capture: %v", stopErr)
	case d.captureRunning():
		bad("capture is STILL running — find it with `pgrep -fl promptster-teams` and stop it manually")
	case wasRunning:
		ok("background capture stopped")
	default:
		skip("no background capture was running")
	}

	// The Cursor hook. Removed AFTER the daemon is down: `watch` re-enrolls at
	// startup, so unenrolling while it is alive is safe today but becomes a race
	// the moment anything re-enrolls on a timer.
	if changed, err := d.removeCursorHooks(); err != nil {
		bad("could not unenroll the Cursor hook: %v", err)
		line(dimStyle.Render("  edit ~/.cursor/hooks.json and delete the promptster-teams entries by hand"))
	} else if changed {
		ok("Cursor hook unenrolled from ~/.cursor/hooks.json")
	} else {
		skip("no Cursor hook to unenroll")
	}

	wrapped := d.statuslineWrapped()
	if err := d.disableStatusline(); err != nil {
		bad("could not restore your Claude statusline: %v", err)
	} else if wrapped {
		ok("Claude statusline restored")
	} else {
		skip("Claude statusline was not wrapped")
	}

	fmt.Fprintln(out)
	switch {
	case purge:
		purgeState(d, line, ok, skip, bad)
	case dirExists(state.GlobalPromptsterDir()):
		line(dimStyle.Render("Left alone: ") + bodyStyle.Render(state.HomeRelative(state.GlobalPromptsterDir())) +
			dimStyle.Render(" — your key, the unsent event queue, and the binary."))
		line(dimStyle.Render("Delete those too with ") + bodyStyle.Render("promptster-teams uninstall --purge") + dimStyle.Render("."))
	}

	// The npm line is unconditional and it is not padding. `npm rm -g` neither
	// runs an uninstall script nor deletes the managed binary (both measured), so
	// an engineer who removes the package and nothing else still has this CLI
	// installed. Nothing in the path of the running binary distinguishes an npm
	// install from a curl one — npm's launcher execs the same managed path — so
	// guessing which channel they used would be a coin flip; saying it plainly and
	// letting them skip it is the honest version.
	fmt.Fprintln(out)
	line(dimStyle.Render("If you installed with npm, also run ") + bodyStyle.Render("npm rm -g @promptster/teams-cli") + dimStyle.Render(" —"))
	line(dimStyle.Render("npm removes its own copy, never the managed binary above."))
	fmt.Fprintln(out)

	if failed {
		return 1
	}
	return 0
}

// purgeState deletes the state directories. Failures are recorded by `bad`,
// which owns the exit code — this returns nothing so there is one place that
// decides whether the run failed rather than two that can disagree.
//
// A run that finds nothing to delete still says so. Silence here would be the
// only step with no output, which reads as "the purge did not happen" when it in
// fact had nothing to do — and this is exactly the state a second run lands in.
func purgeState(d uninstallDeps, line func(string), ok, skip, bad func(string, ...any)) {
	deleted := 0
	for _, dir := range d.purgeDirs() {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		deleted++
		left, err := removeStateDir(dir, d.self)
		switch {
		case err != nil:
			bad("could not delete %s: %v", state.HomeRelative(dir), err)
		case left != "":
			ok("deleted %s", state.HomeRelative(dir))
			line(dimStyle.Render("  except ") + bodyStyle.Render(state.HomeRelative(left)) +
				dimStyle.Render(" — Windows cannot delete a running program; delete it after this exits"))
		default:
			ok("deleted %s — key, event queue, and binary", state.HomeRelative(dir))
		}
	}
	if deleted == 0 {
		skip("nothing to delete — %s does not exist", state.HomeRelative(state.GlobalPromptsterDir()))
	}
}

// removeStateDir deletes dir, returning the one path it had to leave behind.
//
// Unix unlinks a running binary happily (the kernel keeps the inode until the
// process exits), so the whole tree goes. Windows refuses to delete a running
// image, and an os.RemoveAll that aborts on it would leave an ARBITRARY amount
// of the tree behind — including, quite possibly, the credentials the engineer
// asked to be purged. So on Windows the running binary is skipped explicitly and
// named in the output, and everything else is still removed.
func removeStateDir(dir, self string) (leftBehind string, err error) {
	if runtime.GOOS == "windows" && self != "" && pathUnder(self, dir) {
		if err := removeAllExcept(dir, self); err != nil {
			return "", err
		}
		return self, nil
	}
	return "", os.RemoveAll(dir)
}

// removeAllExcept empties dir of everything but keep and keep's parent chain.
func removeAllExcept(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		switch {
		case filepath.Clean(p) == filepath.Clean(keep):
			continue
		case e.IsDir() && pathUnder(keep, p):
			if err := removeAllExcept(p, keep); err != nil {
				return err
			}
		default:
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// pathUnder reports whether p is dir or sits inside it. The separator check
// stops /home/x/.promptster-teams-old from reading as inside
// /home/x/.promptster-teams.
func pathUnder(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(os.PathSeparator))
}
