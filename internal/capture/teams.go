package capture

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
)

// loadSession builds the teams capture context. The ingest credential is a
// per-engineer key (PSE-XXXX-XXXX) resolved with flag > env > stored-file
// precedence (see credentials.go); the API URL resolves the same way, falling
// back to the hosted default. `runTeamsWatch` exports the resolved values into
// the environment before spawning the watchers, so this stays signatureless and
// the watchers (and the `claude-watch`/`codex-watch` subcommands) pick them up.
func loadSession() (Session, error) {
	token, _ := ingest.ResolveToken("")
	if token == "" {
		return Session{}, fmt.Errorf("no developer key configured — run `promptster-teams login`, set PROMPTSTER_TEAMS_TOKEN, or pass --key PSE-XXXX-XXXX")
	}
	apiURL := ingest.ResolveAPIURL("")

	root := os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR")
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}

	return Session{
		SessionID:    DeviceID(),
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
func DeviceID() string {
	fp := ingest.CollectDeviceFingerprint()
	if fp.MachineIDHash != "" {
		return "dev-" + fp.MachineIDHash[:16]
	}
	return "dev-" + ingest.Sha256Hex(fp.HostnameHash + fp.UsernameHash)[:16]
}

// resolveWatchEnv parses the shared `watch`/`start` flags (--key, --api-url),
// resolves the credential (flag > env > stored) and ingest URL, and reports the
// directory to watch (PROMPTSTER_TEAMS_WATCH_DIR env, else cwd). It does NOT
// mutate the environment — callers decide whether to export into their own env
// (foreground `watch`) or hand the values to a detached child (`start`), so the
// two entry points can't drift on how a credential is resolved.
func resolveWatchEnv(args []string) (token, apiURL, watchDir string, err error) {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	keyFlag := fs.String("key", "", "Developer key (PSE-XXXX-XXXX); overrides env/stored")
	urlFlag := fs.String("api-url", "", "Override ingest base URL")
	if err := fs.Parse(args); err != nil {
		return "", "", "", err
	}

	token, _ = ingest.ResolveToken(*keyFlag)
	if token == "" {
		return "", "", "", fmt.Errorf("no developer key configured — run `promptster-teams login`, set PROMPTSTER_TEAMS_TOKEN, or pass --key PSE-XXXX-XXXX")
	}
	apiURL = ingest.ResolveAPIURL(*urlFlag)

	watchDir = os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR")
	if watchDir == "" {
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			watchDir = cwd
		}
	}
	return token, apiURL, watchDir, nil
}

// runTeamsWatch runs the Claude + Codex transcript watchers concurrently in the
// foreground. Each tails its tool's .jsonl, normalizes, redacts on-device,
// signs, and ships to the configured ingest endpoint. Returns when either
// watcher exits (e.g. Ctrl-C).
func RunTeamsWatch(args []string) error {
	// Resolve the credential up front (flag > env > stored) and export the
	// result so the child watchers — which call loadSession() — and apiURL()
	// all observe the same values, including a --key passed only to `watch`.
	token, apiURL, _, err := resolveWatchEnv(args)
	if err != nil {
		return err
	}
	_ = os.Setenv("PROMPTSTER_TEAMS_TOKEN", token)
	_ = os.Setenv("PROMPTSTER_TEAMS_API_URL", apiURL)
	_ = os.Setenv("PROMPTSTER_API_URL", apiURL)

	cfg, err := loadSession()
	if err != nil {
		return err
	}

	// Ensure a per-device signing keypair exists so every event is signed into
	// a tamper-evident chain (`prevSig` links each event to the last). This is
	// a trust feature: a team can verify the stream wasn't altered in transit.
	if priv, _ := sign.LoadSessionKeypair(); priv == nil {
		if _, err := sign.GenerateSessionKeypair(); err != nil {
			fmt.Fprintf(os.Stderr, "promptster-teams: warning: could not create signing key (events will be unsigned): %v\n", err)
		}
	}

	fmt.Fprintf(os.Stderr, "promptster-teams: capturing transcripts under %s → %s\n", cfg.TaskRoot, ingest.APIHost())
	fmt.Fprintf(os.Stderr, "promptster-teams: everything is redacted on-device before it leaves this machine. Ctrl-C to stop.\n")

	// Announce presence on start and periodically while running, so the backend
	// can tell "installed but idle" from "never installed" even when no
	// transcripts are being written. Device + environment metadata only — no
	// transcript content, no identity (see presence.go).
	stopPresence := StartPresenceHeartbeat(cfg)
	defer stopPresence()

	// Config census: one inventory of the local agent config (token counts +
	// names ONLY, never file contents — see census.go) on startup, then every
	// 24h while watching.
	stopCensus := StartConfigCensus(cfg)
	defer stopCensus()

	errCh := make(chan error, 2)
	go func() { errCh <- RunClaudeWatcher() }()
	go func() { errCh <- RunCodexWatcher() }()
	return <-errCh
}
