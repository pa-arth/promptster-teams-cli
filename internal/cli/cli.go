package cli

import (
	"fmt"
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

// Main is the CLI entry point. It parses argv (os.Args, including argv[0]) and
// returns the process exit code, so the thin cmd/promptster-teams wrapper is
// just os.Exit(cli.Main(os.Args)).
func Main(argv []string) int {
	if len(argv) < 2 {
		printUsage()
		return 0
	}

	switch argv[1] {
	case "login":
		// One-time setup: paste the per-engineer key your manager minted (or
		// pass --key), validate it, and store it locally so `watch` just works.
		cmdLogin(argv[2:])
	case "watch":
		// Foreground capture: tails Claude Code + Codex + Cursor transcript JSONL,
		// normalizes, redacts on-device, signs, and ships to the configured
		// teams ingest endpoint. Holds the terminal until Ctrl-C.
		if err := capture.RunTeamsWatch(argv[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "watch error: %v\n", err)
			return 1
		}
	case "start":
		// Background capture: spawn a detached `watch` supervisor and return
		// the shell. `stop` tears it down; `status` shows whether it's alive.
		if err := capture.StartTeamsDaemon(argv[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "start error: %v\n", err)
			return 1
		}
	case "stop":
		if err := capture.StopTeamsDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "stop error: %v\n", err)
			return 1
		}
	case "claude-watch":
		if err := capture.RunClaudeWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "claude watcher error: %v\n", err)
			return 1
		}
	case "codex-watch":
		if err := capture.RunCodexWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "codex watcher error: %v\n", err)
			return 1
		}
	case "cursor-hook":
		// Invoked BY CURSOR, once per hook step, with the payload on stdin.
		// Registered in ~/.cursor/hooks.json by EnsureCursorHooks. Never call it
		// by hand except to test. It always exits 0 — it runs inside the
		// engineer's agent loop and must not be able to break their tool.
		if err := capture.RunCursorHook(); err != nil {
			return 0
		}
	case "cursor-watch":
		if err := capture.RunCursorWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "cursor watcher error: %v\n", err)
			return 1
		}
	case "git-watch":
		// Out-of-band git watcher: detect new commits per root on a ~60s timer
		// and advance a persisted per-root HEAD cursor. Detection only — emits
		// nothing. Runs foreground until interrupted.
		if err := capture.RunGitWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "git watcher error: %v\n", err)
			return 1
		}
	case "autostart":
		// Register/remove the per-user OS service that relaunches capture at
		// login so it survives reboots (launchd / systemd --user / Task Scheduler).
		return cmdAutostart(argv[2:])
	case "statusline":
		// Claude Code rate-limit WINDOW capture. `enable`/`disable` wrap/unwrap the
		// engineer's statusLine command; `run` is the shim Claude Code invokes each
		// tick (reads stdin, spools the window reading, passes the prior line
		// through); `status` reports the effective-statusline drift check.
		return cmdStatusline(argv[2:])
	case "uninstall":
		// Undo the install: stop capture, deregister the autostart unit, unenroll
		// the Cursor hook, restore the statusline. --purge also deletes
		// ~/.promptster-teams. This is the ONLY uninstall path that exists —
		// `npm rm -g` runs no uninstall script and leaves the managed binary in
		// place, so removing the package alone stops nothing.
		return cmdUninstall(argv[2:])
	case "status":
		cmdTeamsStatus(argv[2:])
	case "doctor":
		cmdTeamsDoctor()
	case "version", "--version", "-v":
		fmt.Println(version.Version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", argv[1])
		printUsage()
		return 1
	}
	return 0
}

func printUsage() {
	fmt.Print(`promptster-teams — on-device AI coding capture for internal teams

Usage: promptster-teams <command>

Commands:
  login        Save your developer key (PSE-XXXX-…) — paste it or pass --key
  start        Capture in the background (detaches and returns your shell)
  stop         Stop background capture
  autostart    Keep capture alive across reboots (enable|disable|status|repair) — starts at login
  statusline   Track your Claude 5h/weekly usage via the statusline (enable|disable|status)
  watch        Foreground capture — tail Claude Code + Codex + Cursor transcripts, redact on-device, ship to your team's backend (Ctrl-C to stop)
  status       Show capture status, whether the daemon is running, and event count
  doctor       Diagnose configuration (key, ingest URL, watched dirs)
  uninstall    Undo the install — stop capture, remove autostart, unenroll the Cursor hook, restore the statusline (--purge also deletes ~/.promptster-teams)
  version      Print version
  help         Show this help

Getting started:
  promptster-teams login            # paste your key — capture starts in the background automatically
  promptster-teams autostart enable # keep capturing across reboots (starts at login)
  promptster-teams status           # confirm capture is running
  promptster-teams stop             # stop when you're done
  promptster-teams uninstall        # remove it — npm rm / deleting the binary does NOT stop capture

Capture runs detached and silent. Set PROMPTSTER_DEBUG=1 before watch/start
to see per-event watcher logging.

The CLI checks GitHub Releases on a 30m cadence while watching and asks before
installing a newer signed release, with a link to its release notes. Declining
only skips that cycle. Opt out with watch/start --no-auto-update or
PROMPTSTER_TEAMS_NO_AUTO_UPDATE=1; your org can also disable or pin the version.

Your developer key is resolved from, in order: --key flag,
PROMPTSTER_TEAMS_TOKEN env, then ~/.promptster-teams/credentials (written by
login). PROMPTSTER_TEAMS_API_URL overrides the ingest URL (default: hosted).

Everything is captured locally and redacted on-device before anything is sent.
Source: https://github.com/pa-arth/promptster-teams-cli
`)
}
