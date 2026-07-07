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
		// Foreground capture: tails Claude Code + Codex transcript JSONL,
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
	case "status":
		cmdTeamsStatus()
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
  login        Save your developer key (PSE-XXXX-XXXX) — paste it or pass --key
  start        Capture in the background (detaches and returns your shell)
  stop         Stop background capture
  watch        Foreground capture — tail transcripts, redact on-device, ship to your team's backend (Ctrl-C to stop)
  status       Show capture status, whether the daemon is running, and event count
  doctor       Diagnose configuration (key, ingest URL, watched dirs)
  version      Print version
  help         Show this help

Getting started:
  promptster-teams login            # paste the key your manager gave you
  promptster-teams start            # capture from the current repo in the background
  promptster-teams stop             # stop when you're done

Your developer key is resolved from, in order: --key flag,
PROMPTSTER_TEAMS_TOKEN env, then ~/.promptster-teams/credentials (written by
login). PROMPTSTER_TEAMS_API_URL overrides the ingest URL (default: hosted).

Everything is captured locally and redacted on-device before anything is sent.
Source: https://github.com/pa-arth/promptster-teams-cli
`)
}
