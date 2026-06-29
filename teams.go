package main

import (
	"bufio"
	"fmt"
	"os"
	"time"
)

// loadSession builds the teams capture context from environment configuration.
// It replaces the hiring CLI's redeem-key session: there is no server round
// trip, no key, no consent gate — just "where do I send, with what token, and
// which directory's transcripts do I watch."
func loadSession() (Session, error) {
	apiURL := os.Getenv("PROMPTSTER_TEAMS_API_URL")
	token := os.Getenv("PROMPTSTER_TEAMS_TOKEN")
	if apiURL == "" || token == "" {
		return Session{}, fmt.Errorf("set PROMPTSTER_TEAMS_API_URL and PROMPTSTER_TEAMS_TOKEN before running `promptster-teams watch`")
	}

	root := os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR")
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}

	return Session{
		SessionID:    deviceID(),
		SessionToken: token,
		TaskRoot:     root,
		ApiURL:       apiURL,
		CaptureMode:  "transcript",
		StartedAt:    time.Now().UTC(),
	}, nil
}

// deviceID returns a stable, anonymous per-device identifier (a hash of the
// machine id, falling back to hostname+user). It is the only identity stamped
// on events in this phase; org -> team -> developer enrollment lands with the
// backend.
func deviceID() string {
	fp := collectDeviceFingerprint()
	if fp.MachineIDHash != "" {
		return "dev-" + fp.MachineIDHash[:16]
	}
	return "dev-" + sha256Hex(fp.HostnameHash + fp.UsernameHash)[:16]
}

// runTeamsWatch runs the Claude + Codex transcript watchers concurrently in the
// foreground. Each tails its tool's .jsonl, normalizes, redacts on-device,
// signs, and ships to the configured ingest endpoint. Returns when either
// watcher exits (e.g. Ctrl-C).
func runTeamsWatch() error {
	cfg, err := loadSession()
	if err != nil {
		return err
	}
	// Point apiURL() at the team's backend for both watchers.
	os.Setenv("PROMPTSTER_API_URL", cfg.ApiURL)

	// Ensure a per-device signing keypair exists so every event is signed into
	// a tamper-evident chain (`prevSig` links each event to the last). This is
	// a trust feature: a team can verify the stream wasn't altered in transit.
	if priv, _ := loadSessionKeypair(); priv == nil {
		if _, err := generateSessionKeypair(); err != nil {
			fmt.Fprintf(os.Stderr, "promptster-teams: warning: could not create signing key (events will be unsigned): %v\n", err)
		}
	}

	fmt.Fprintf(os.Stderr, "promptster-teams: capturing transcripts under %s → %s\n", cfg.TaskRoot, apiHost())
	fmt.Fprintf(os.Stderr, "promptster-teams: everything is redacted on-device before it leaves this machine. Ctrl-C to stop.\n")

	errCh := make(chan error, 2)
	go func() { errCh <- runClaudeWatcher() }()
	go func() { errCh <- runCodexWatcher() }()
	return <-errCh
}

// cmdTeamsStatus prints the current configuration and local buffer event count.
func cmdTeamsStatus() {
	apiURL := os.Getenv("PROMPTSTER_TEAMS_API_URL")
	token := os.Getenv("PROMPTSTER_TEAMS_TOKEN")
	root := os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR")
	if root == "" {
		root, _ = os.Getwd()
	}

	fmt.Println()
	fmt.Println("  promptster-teams status")
	fmt.Printf("  ingest URL:  %s\n", orNotSet(apiURL))
	fmt.Printf("  token:       %s\n", maskToken(token))
	fmt.Printf("  watch dir:   %s\n", root)
	fmt.Printf("  device id:   %s\n", deviceID())
	fmt.Printf("  events buffered locally: %d\n", countBufferedEvents())
	fmt.Println()
}

// cmdTeamsDoctor checks that the minimum configuration is present.
func cmdTeamsDoctor() {
	ok := true
	fmt.Println()
	fmt.Println("  promptster-teams doctor")
	if os.Getenv("PROMPTSTER_TEAMS_API_URL") == "" {
		fmt.Println("  ✗ PROMPTSTER_TEAMS_API_URL is not set")
		ok = false
	} else {
		fmt.Printf("  ✓ ingest URL: %s\n", os.Getenv("PROMPTSTER_TEAMS_API_URL"))
	}
	if os.Getenv("PROMPTSTER_TEAMS_TOKEN") == "" {
		fmt.Println("  ✗ PROMPTSTER_TEAMS_TOKEN is not set")
		ok = false
	} else {
		fmt.Println("  ✓ ingest token is set")
	}
	if _, err := os.Stat(claudeProjectsDir()); err == nil {
		fmt.Printf("  ✓ Claude Code transcript dir found: %s\n", claudeProjectsDir())
	} else {
		fmt.Printf("  • Claude Code transcript dir not found yet: %s\n", claudeProjectsDir())
	}
	fmt.Println()
	if ok {
		fmt.Println("  Ready. Run `promptster-teams watch` from a repo to begin capturing.")
	} else {
		fmt.Println("  Set the missing variables above, then re-run doctor.")
	}
	fmt.Println()
}

func countBufferedEvents() int {
	f, err := os.Open(hookBufferPath())
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n
}

func orNotSet(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}

func maskToken(s string) string {
	if s == "" {
		return "(not set)"
	}
	if len(s) <= 6 {
		return "******"
	}
	return s[:3] + "…" + s[len(s)-3:]
}
