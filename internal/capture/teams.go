package capture

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/policy"
	"github.com/pa-arth/promptster-teams-cli/internal/selfupdate"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
)

// loadSession builds the teams capture context. The ingest credential is a
// per-engineer key (PSE-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX) resolved with flag > env > stored-file
// precedence (see credentials.go); the API URL resolves the same way, falling
// back to the hosted default. `runTeamsWatch` exports the resolved values into
// the environment before spawning the watchers, so this stays signatureless and
// the watchers (and the `claude-watch`/`codex-watch` subcommands) pick them up.
func loadSession() (Session, error) {
	token, _ := ingest.ResolveToken("")
	if token == "" {
		return Session{}, fmt.Errorf("no developer key configured — run `promptster-teams login`, set PROMPTSTER_TEAMS_TOKEN, or pass --key " + ingest.KeyFormatHint)
	}
	apiURL := ingest.ResolveAPIURL("")

	root := os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR")
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}

	return Session{
		DeviceID:     DeviceID(),
		SessionToken: token,
		TaskRoot:     root,
		ApiURL:       apiURL,
		CaptureMode:  "transcript",
		StartedAt:    time.Now().UTC(),
	}, nil
}

// verboseWatch reports whether the watchers should emit chatty per-flush and
// per-startup progress lines ("sent N event(s) from X", "started, polling …").
// Off by default: those lines fire on every 3s poll and bury the useful
// startup/shutdown/error lines — and in detached mode they flood daemon.log.
// Set PROMPTSTER_DEBUG=1 to turn them back on. `status` surfaces the running
// event count instead. Errors, degraded/handoff, and shutdown lines are never
// gated (they're rare and actionable).
func verboseWatch() bool {
	return os.Getenv("PROMPTSTER_DEBUG") == "1"
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
func resolveWatchEnv(args []string) (token, apiURL, watchDir string, noAutoUpdate bool, err error) {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	keyFlag := fs.String("key", "", "Developer key ("+ingest.KeyFormatHint+"); overrides env/stored")
	urlFlag := fs.String("api-url", "", "Override ingest base URL")
	noUpdateFlag := fs.Bool("no-auto-update", false, "Disable silent self-update of the CLI while watching")
	if err := fs.Parse(args); err != nil {
		return "", "", "", false, err
	}

	token, _ = ingest.ResolveToken(*keyFlag)
	if token == "" {
		return "", "", "", false, fmt.Errorf("no developer key configured — run `promptster-teams login`, set PROMPTSTER_TEAMS_TOKEN, or pass --key " + ingest.KeyFormatHint)
	}
	apiURL = ingest.ResolveAPIURL(*urlFlag)

	watchDir = os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR")
	if watchDir == "" {
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			watchDir = cwd
		}
	}
	return token, apiURL, watchDir, *noUpdateFlag, nil
}

// runTeamsWatch runs the Claude + Codex transcript watchers concurrently in the
// foreground. Each tails its tool's .jsonl, normalizes, redacts on-device,
// signs, and ships to the configured ingest endpoint. Returns when either
// watcher exits (e.g. Ctrl-C).
func RunTeamsWatch(args []string) error {
	// Single-instance guard: only one supervisor may capture at a time, whatever
	// launched it (manual `start`, this foreground `watch`, or the autostart
	// service). A second watcher would double-count presence + events and corrupt
	// the seat-utilization metric, so bow out cleanly (exit 0 — launchd's
	// KeepAlive{SuccessfulExit:false} then won't respawn a duplicate).
	release, ok, err := acquireWatchLock()
	if err != nil {
		return fmt.Errorf("could not take capture lock: %w", err)
	}
	if !ok {
		if pid, running := watchRunning(); running {
			fmt.Fprintf(os.Stderr, "promptster-teams: capture already running (pid %d) — not starting a second watcher\n", pid)
		} else {
			fmt.Fprintln(os.Stderr, "promptster-teams: capture already running — not starting a second watcher")
		}
		return nil
	}
	defer release()

	// Resolve the credential up front (flag > env > stored) and export the
	// result so the child watchers — which call loadSession() — and apiURL()
	// all observe the same values, including a --key passed only to `watch`.
	token, apiURL, _, noAutoUpdate, err := resolveWatchEnv(args)
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

	// Silent self-update: on startup and every 24h, check GitHub Releases for a
	// newer signed CLI and swap in place (re-exec keeps capture running). Opt out
	// per-machine with --no-auto-update / PROMPTSTER_TEAMS_NO_AUTO_UPDATE, or
	// org-wide via the capture policy. A dedicated resolver refreshes the org
	// switch/pin off the hot path; fail-OPEN so a policy blip never strands the
	// fleet on an old binary (see selfupdate + policy.AutoUpdateEnabled).
	updatePolicy := policy.NewResolver(cfg.SessionToken)
	policyCtx, cancelPolicy := context.WithCancel(context.Background())
	defer cancelPolicy()
	updatePolicy.StartBackground(policyCtx)
	stopUpdate := selfupdate.StartAutoUpdate(noAutoUpdate, updatePolicy)
	defer stopUpdate()

	errCh := make(chan error, 2)
	go func() { errCh <- RunClaudeWatcher() }()
	go func() { errCh <- RunCodexWatcher() }()
	return <-errCh
}
