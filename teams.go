package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"time"
)

// loadSession builds the teams capture context. The ingest credential is a
// per-engineer key (PSE-XXXX-XXXX) resolved with flag > env > stored-file
// precedence (see credentials.go); the API URL resolves the same way, falling
// back to the hosted default. `runTeamsWatch` exports the resolved values into
// the environment before spawning the watchers, so this stays signatureless and
// the watchers (and the `claude-watch`/`codex-watch` subcommands) pick them up.
func loadSession() (Session, error) {
	token, _ := resolveToken("")
	if token == "" {
		return Session{}, fmt.Errorf("no developer key configured — run `promptster-teams login`, set PROMPTSTER_TEAMS_TOKEN, or pass --key PSE-XXXX-XXXX")
	}
	apiURL := resolveAPIURL("")

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
func runTeamsWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	keyFlag := fs.String("key", "", "Developer key (PSE-XXXX-XXXX); overrides env/stored")
	urlFlag := fs.String("api-url", "", "Override ingest base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Resolve the credential up front (flag > env > stored) and export the
	// result so the child watchers — which call loadSession() — and apiURL()
	// all observe the same values, including a --key passed only to `watch`.
	token, _ := resolveToken(*keyFlag)
	if token == "" {
		return fmt.Errorf("no developer key configured — run `promptster-teams login`, set PROMPTSTER_TEAMS_TOKEN, or pass --key PSE-XXXX-XXXX")
	}
	apiURL := resolveAPIURL(*urlFlag)
	os.Setenv("PROMPTSTER_TEAMS_TOKEN", token)
	os.Setenv("PROMPTSTER_TEAMS_API_URL", apiURL)
	os.Setenv("PROMPTSTER_API_URL", apiURL)

	cfg, err := loadSession()
	if err != nil {
		return err
	}

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

	// Announce presence on start and periodically while running, so the backend
	// can tell "installed but idle" from "never installed" even when no
	// transcripts are being written. Device + environment metadata only — no
	// transcript content, no identity (see presence.go).
	stopPresence := startPresenceHeartbeat(cfg)
	defer stopPresence()

	errCh := make(chan error, 2)
	go func() { errCh <- runClaudeWatcher() }()
	go func() { errCh <- runCodexWatcher() }()
	return <-errCh
}

// keyDisplay renders the resolved key + where it came from for status/doctor.
func keyDisplay(token, source string) string {
	if token == "" {
		return "(not set)"
	}
	return fmt.Sprintf("%s  (%s)", maskKey(token), source)
}

// cmdTeamsStatus prints the resolved configuration and local buffer count.
func cmdTeamsStatus() {
	token, source := resolveToken("")
	apiURL := resolveAPIURL("")
	root := os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR")
	if root == "" {
		root, _ = os.Getwd()
	}

	fmt.Println()
	fmt.Println(brandBar("status"))
	fmt.Println()
	fmt.Println(indent(kvPanel("capture",
		"key", keyDisplay(token, source),
		"ingest", hostOf(apiURL),
		"watch", root,
		"device", deviceID(),
		"identity", "anonymous — device hash + team key, no email",
		"presence", fmt.Sprintf("heartbeat every %s during watch", presenceHeartbeatInterval),
		"buffered", fmt.Sprintf("%d events", countBufferedEvents()),
	)))
	fmt.Println()
}

// cmdTeamsDoctor diagnoses the credential, ingest reachability, and transcript
// dir. The reachability probe is a plain GET to the API base (not an auth probe
// against the ingest endpoint), so it never writes anything.
func cmdTeamsDoctor() {
	token, source := resolveToken("")
	apiURL := resolveAPIURL("")
	ok := true

	fmt.Println()
	fmt.Println(brandBar("doctor"))
	fmt.Println()

	switch {
	case token == "":
		printlnIndent(fmt.Sprintf("%s no developer key — run `promptster-teams login`", errGlyph))
		ok = false
	case isEngineerKey(token):
		printlnIndent(fmt.Sprintf("%s key %s  (%s)", okGlyph, maskKey(token), source))
	default:
		printlnIndent(fmt.Sprintf("%s key set but not a PSE- developer key (%s): %s", warnGlyph, source, maskKey(token)))
	}

	if pingIngestHost(apiURL) {
		printlnIndent(fmt.Sprintf("%s ingest reachable: %s", okGlyph, hostOf(apiURL)))
	} else {
		printlnIndent(fmt.Sprintf("%s ingest not reachable: %s", warnGlyph, hostOf(apiURL)))
	}

	if _, err := os.Stat(claudeProjectsDir()); err == nil {
		printlnIndent(fmt.Sprintf("%s Claude Code transcripts: %s", okGlyph, claudeProjectsDir()))
	} else {
		printlnIndent(fmt.Sprintf("%s Claude Code transcript dir not found yet: %s", warnGlyph, claudeProjectsDir()))
	}

	printlnIndent(fmt.Sprintf("%s presence heartbeat every %s while watching — device + tools only, no identity/email", okGlyph, presenceHeartbeatInterval))

	fmt.Println()
	if ok {
		printlnIndent(dimStyle.Render("Ready. Run ") + bodyStyle.Render("promptster-teams watch") + dimStyle.Render(" from a repo."))
	} else {
		printlnIndent(dimStyle.Render("Run ") + bodyStyle.Render("promptster-teams login") + dimStyle.Render(" to get set up."))
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
