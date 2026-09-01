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
	"github.com/pa-arth/promptster-teams-cli/internal/state"
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

// DeviceID returns a stable, anonymous per-INSTALLATION identifier. The
// installation id separates independent home-scoped daemons on one physical
// machine; the machine fingerprint keeps continuity tied to this machine
// without exposing either input. Presence and every captured event carry this
// field, giving the backend the key it needs for per-installation health rows.
func DeviceID() string {
	fp := ingest.CollectDeviceFingerprint()
	machine := fp.MachineIDHash
	if machine == "" {
		machine = ingest.Sha256Hex(fp.HostnameHash + fp.UsernameHash)
	}
	return "dev-" + ingest.Sha256Hex(machine + ":" + state.InstallationID())[:16]
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
	noUpdateFlag := fs.Bool("no-auto-update", false, "Disable update checks and prompts while watching")
	if err := fs.Parse(args); err != nil {
		return "", "", "", false, err
	}

	token, _ = ingest.ResolveToken(*keyFlag)
	if token == "" {
		return "", "", "", false, fmt.Errorf("no developer key configured — run `promptster-teams login`, set PROMPTSTER_TEAMS_TOKEN, or pass --key " + ingest.KeyFormatHint)
	}
	apiURL = ingest.ResolveAPIURL(*urlFlag)

	return token, apiURL, watchDirFromEnv(), *noUpdateFlag, nil
}

// watchDirFromEnv reports the directory this invocation would capture:
// PROMPTSTER_TEAMS_WATCH_DIR, else the cwd. Split out of resolveWatchEnv
// because the already-running paths need the directory WITHOUT resolving a
// credential — they only want to register it as a capture root.
func watchDirFromEnv() string {
	if dir := os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR"); dir != "" {
		return dir
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
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
	// Read and clear the handoff marker before anything else, so it cannot ride
	// os.Environ() into a re-exec of our own and turn a later ordinary start into
	// a silent waiter.
	handoff := consumeHandoffMarker()
	release, ok, err := acquireWatchLock()
	if err != nil {
		return fmt.Errorf("could not take capture lock: %w", err)
	}
	if !ok && handoff {
		// We were spawned to REPLACE the watcher that holds this lock (a Windows
		// self-update or on-disk catch-up, which cannot execve and so must exit a
		// parent that is still holding it). Bowing out here is the one case where
		// "already running" is wrong in a way that costs capture entirely: the
		// parent is on its way out, nobody is left, and nothing tries again until
		// the next login. So wait for the handle to drop.
		release, ok, err = awaitWatchLock(handoffLockWait)
		if err != nil {
			return fmt.Errorf("could not take capture lock: %w", err)
		}
	}
	if !ok {
		// Bowing out must not mean this directory goes uncaptured: register it so
		// the watcher that DOES hold the lock picks it up on its next poll. This
		// is the autostart path's version of the second `start` — a per-tree
		// launchd/npm watcher in a second checkout otherwise exited silently and
		// that tree's sessions were never captured by anyone.
		dir := watchDirFromEnv()
		_, _, rootErr := RegisterCaptureRoot(dir)
		pid, running := watchRunning()
		who := "promptster-teams: capture already running"
		if running {
			who = fmt.Sprintf("promptster-teams: capture already running (pid %d)", pid)
		}
		if rootErr != nil {
			// Bowing out is fine; bowing out while claiming this tree was handed
			// over is not — that reads as success and leaves it uncaptured.
			fmt.Fprintf(os.Stderr, "%s, but %s could NOT be handed to it: %v\n", who, dir, rootErr)
		} else {
			fmt.Fprintf(os.Stderr, "%s — %s handed to it instead of starting a second watcher\n", who, dir)
		}
		printWatchedRoots(os.Stderr)
		return nil
	}
	defer release()

	// Stamp WHICH BUILD is capturing, now that this process owns the lock. A
	// self-update re-execs into RunTeamsWatch again, so the record follows the
	// binary rather than the start. See watch_runtime.go for why nothing else on
	// the machine can answer this question.
	recordWatchRuntime()
	defer clearWatchRuntime()

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

	// On startup and every selfupdate.CheckInterval, check GitHub Releases for a
	// newer signed CLI and ask before swapping it in place (re-exec keeps capture
	// running). Detached watchers safely decline until an interactive cycle. It
	// is NOT on the census's 24h clock above. Opt out
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

	// Out-of-band git watcher: on a ~60s timer, detect new commits per root,
	// advance a persisted per-root HEAD cursor, and emit content-free
	// commit_attribution events reconciling AI evidence against the committed
	// diff (see commit_attribution.go). Off the latency-sensitive path.
	stopGitWatch := StartGitWatch(cfg)
	defer stopGitWatch()

	errCh := make(chan error, 3)
	go func() { errCh <- RunClaudeWatcher() }()
	go func() { errCh <- RunCodexWatcher() }()
	// Cursor runs TWO rails: the transcript tail this shares with the other two,
	// plus a USER-SCOPE hook (~/.cursor/hooks.json) that RunCursorWatcher enrolls
	// at startup. The project-local <workspace>/.cursor/hooks.json is the scope
	// this CLI must never write — a tracked file inside the customer's repo,
	// enrolled per-repo, so every repo an engineer forgot would read as "captured
	// nothing". See CLAUDE.md and internal/capture/cursor_hooks.go.
	//
	// That enrollment living at watch startup is why a daemon that never restarts
	// never gets the rail, however current the binary on disk is — the failure
	// watch_runtime.go exists to make visible.
	go func() { errCh <- RunCursorWatcher() }()
	return <-errCh
}
