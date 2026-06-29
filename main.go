package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		return
	}

	switch os.Args[1] {
	case "watch":
		// Foreground capture: tails Claude Code + Codex transcript JSONL,
		// normalizes, redacts on-device, signs, and ships to the configured
		// teams ingest endpoint.
		if err := runTeamsWatch(); err != nil {
			fmt.Fprintf(os.Stderr, "watch error: %v\n", err)
			os.Exit(1)
		}
	case "claude-watch":
		if err := runClaudeWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "claude watcher error: %v\n", err)
			os.Exit(1)
		}
	case "codex-watch":
		if err := runCodexWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "codex watcher error: %v\n", err)
			os.Exit(1)
		}
	case "status":
		cmdTeamsStatus()
	case "doctor":
		cmdTeamsDoctor()
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`promptster-teams — on-device AI coding capture for internal teams

Usage: promptster-teams <command>

Commands:
  watch        Tail Claude Code + Codex transcripts, redact on-device, ship to your team's backend
  status       Show capture status and event count
  doctor       Diagnose configuration (ingest URL, token, watched dirs)
  version      Print version
  help         Show this help

Configuration (environment):
  PROMPTSTER_TEAMS_API_URL   Team ingest base URL (required)
  PROMPTSTER_TEAMS_TOKEN     Org/device ingest auth token (required)

Everything is captured locally and redacted on-device before anything is sent.
Source: https://github.com/pa-arth/promptster-teams-cli
`)
}
