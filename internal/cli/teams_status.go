package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/selfupdate"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

// loadSession builds the teams capture context. The ingest credential is a
// per-engineer key (PSE-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX) resolved with flag > env > stored-file
// precedence (see credentials.go); the API URL resolves the same way, falling
// back to the hosted default. `runTeamsWatch` exports the resolved values into
// the environment before spawning the watchers, so this stays signatureless and
// the watchers (and the `claude-watch`/`codex-watch` subcommands) pick them up.

// keyDisplay renders the resolved key + where it came from for status/doctor.
func keyDisplay(token, source string) string {
	if token == "" {
		return "(not set)"
	}
	return fmt.Sprintf("%s  (%s)", ingest.MaskKey(token), source)
}

// cmdTeamsStatus shows capture status. By default it opens the live dashboard
// (a full-screen view that refreshes every second); with `--once`/`--plain`, or
// when stdout is not a TTY (pipe/CI), it prints a single static snapshot and
// exits so scripts and redirects stay clean.
func cmdTeamsStatus(args []string) {
	once := false
	for _, a := range args {
		if a == "--once" || a == "--plain" || a == "-1" {
			once = true
		}
	}
	if !once && stdoutIsTTY() {
		if err := runStatusTUI(); err == nil {
			return
		}
		// Fall through to the static print if the TUI couldn't start.
	}
	printStatusStatic()
}

// printStatusStatic prints the resolved configuration and local buffer count as
// a single snapshot.
func printStatusStatic() {
	token, source := ingest.ResolveToken("")
	apiURL := ingest.ResolveAPIURL("")
	// One snapshot for every row it feeds. Sampling per row let capture start or
	// exit between reads, printing a `watch` scope and a `daemon` liveness that
	// never held at the same instant.
	snap := capture.Snapshot()

	// Report what the LIVE daemon is watching, not what a daemon started from
	// here would watch. The watch dir gates which transcripts are captured, and
	// `login` scopes the daemon to $HOME — so recomputing from this process's cwd
	// showed `status` run inside a repo that repo's path, implying a scope the
	// running capture never had. Fall back to the env/cwd derivation only when no
	// capture is live, where it correctly previews the next `start`.
	root := snap.WatchDir
	if root == "" {
		root = os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR")
	}
	if root == "" {
		root, _ = os.Getwd()
	}

	daemon := "not running — `promptster-teams login` starts it, or `autostart enable` for reboots"
	if snap.Live {
		daemon = fmt.Sprintf("running (pid %d)", snap.DaemonPID)
	}

	autostart := "not enabled — `promptster-teams autostart enable` (so capture survives reboots)"
	if installed, detail, err := service.New().Status(); err == nil && installed && detail != "" {
		autostart = detail
	}

	fmt.Println()
	fmt.Println(brandBar("status"))
	fmt.Println()
	fmt.Println(indent(kvPanel("capture",
		"key", keyDisplay(token, source),
		"ingest", hostOf(apiURL),
		"watch", root,
		"daemon", daemon,
		"autostart", autostart,
		"device", capture.DeviceID(),
		"identity", "anonymous — device hash + team key, no email",
		"presence", fmt.Sprintf("heartbeat every %s during watch", humanInterval(capture.PresenceHeartbeatInterval)),
		"buffered", fmt.Sprintf("%d events", countBufferedEvents()),
	)))
	fmt.Println()
}

// cmdTeamsDoctor diagnoses the credential, ingest reachability, the transcript
// dir, and the delivery queue. The reachability probe is a plain GET to the API
// base (not an auth probe against the ingest endpoint), and the queue check only
// stats files: doctor never advances the cursor, compacts the queue, touches the
// ledger, or sends anything. (Reading capture state can clear a stale supervisor
// pidfile — DaemonStatus's own long-standing self-heal, not a queue write.)
func cmdTeamsDoctor() {
	token, source := ingest.ResolveToken("")
	apiURL := ingest.ResolveAPIURL("")
	ok := true

	fmt.Println()
	fmt.Println(brandBar("doctor"))
	fmt.Println()

	printlnIndent(fmt.Sprintf("%s version %s", okGlyph, version.Version))
	printAutoUpdateStatus()

	switch {
	case token == "":
		printlnIndent(fmt.Sprintf("%s no developer key — run `promptster-teams login`", errGlyph))
		ok = false
	case ingest.IsEngineerKey(token):
		printlnIndent(fmt.Sprintf("%s key %s  (%s)", okGlyph, ingest.MaskKey(token), source))
	default:
		printlnIndent(fmt.Sprintf("%s key set but not a PSE- developer key (%s): %s", warnGlyph, source, ingest.MaskKey(token)))
	}

	if pingIngestHost(apiURL) {
		printlnIndent(fmt.Sprintf("%s ingest reachable: %s", okGlyph, hostOf(apiURL)))
	} else {
		printlnIndent(fmt.Sprintf("%s ingest not reachable: %s", warnGlyph, hostOf(apiURL)))
	}

	if _, err := os.Stat(capture.ClaudeProjectsDir()); err == nil {
		printlnIndent(fmt.Sprintf("%s Claude Code transcripts: %s", okGlyph, capture.ClaudeProjectsDir()))
	} else {
		printlnIndent(fmt.Sprintf("%s Claude Code transcript dir not found yet: %s", warnGlyph, capture.ClaudeProjectsDir()))
	}

	printlnIndent(fmt.Sprintf("%s presence heartbeat every %s while watching — device + tools only, no identity/email", okGlyph, humanInterval(capture.PresenceHeartbeatInterval)))

	// Delivery-queue health. Deliberately does not touch `ok`: a stuck or full
	// queue is not a login problem, and `ok` only chooses between the "run watch"
	// and "run login" closing lines.
	for _, l := range checkQueueHealth(gatherQueueInputs(time.Now(), capture.Snapshot())) {
		printlnIndent(fmt.Sprintf("%s %s", l.glyph(), l.text))
	}

	if installed, detail, serr := service.New().Status(); serr == nil && installed {
		printlnIndent(fmt.Sprintf("%s autostart %s", okGlyph, detail))
	} else {
		printlnIndent(fmt.Sprintf("%s autostart not enabled — run `promptster-teams autostart enable` so capture survives reboots", warnGlyph))
	}

	// Cursor hook rail. The state worth catching here is an entry naming a binary
	// that no longer exists: it is created by deleting the binary WITHOUT running
	// `uninstall`, and once the binary is gone none of our code runs, so nothing
	// of ours can find it from the inside. Doctor — run from a working binary,
	// while something is already wrong — is the only place the message can land.
	for _, l := range capture.CursorHooksDoctor() {
		printlnIndent(fmt.Sprintf("%s %s", cursorHookGlyph(l), l.Text))
	}

	// Claude statusline window-capture drift check: resolve the EFFECTIVE
	// statusline across all settings layers (not just our file) so we catch a
	// project-layer shadow or an overwrite that silently stops window capture.
	dir, _ := os.Getwd()
	for _, l := range capture.StatuslineDoctor(dir) {
		glyph := okGlyph
		if l.Warn {
			glyph = warnGlyph
		}
		printlnIndent(fmt.Sprintf("%s %s", glyph, l.Text))
	}

	fmt.Println()
	if ok {
		printlnIndent(dimStyle.Render("Ready. Run ") + bodyStyle.Render("promptster-teams watch") + dimStyle.Render(" from a repo."))
	} else {
		printlnIndent(dimStyle.Render("Run ") + bodyStyle.Render("promptster-teams login") + dimStyle.Render(" to get set up."))
	}
	fmt.Println()
}

// cursorHookGlyph maps a Cursor hook diagnostic to its glyph. The Err level is
// distinct on purpose: a dangling command degrades the ENGINEER'S tool on every
// event, which must not render the same as "not enrolled yet".
func cursorHookGlyph(l capture.CursorHookDoctorLine) string {
	switch {
	case l.Err:
		return errGlyph
	case l.Warn:
		return warnGlyph
	default:
		return okGlyph
	}
}

// printAutoUpdateStatus renders the self-updater's read-only state for doctor:
// whether it is on, disabled by the per-machine env opt-out, or (best-effort)
// whether a newer release exists. Org-policy disable/pin is resolved only while
// watching (it needs an authenticated fetch), so doctor reports the machine-
// local switch and a short-timeout latest-version probe that degrades silently.
func printAutoUpdateStatus() {
	env := os.Getenv(selfupdate.EnvNoAutoUpdate)
	// Only probe the network when the answer can still depend on it — the two
	// early branches below decide without it. The condition MUST match
	// autoUpdateStatusLine's own gates, or doctor degrades to the vaguest line
	// for a set-but-falsy opt-out (PROMPTSTER_TEAMS_NO_AUTO_UPDATE=0).
	latest, latestOK := "", false
	if !selfupdate.EnvDisablesAutoUpdate(env) && version.Version != "dev" && version.Version != "" {
		latest, latestOK = selfupdate.LatestVersionBestEffort(3 * time.Second)
	}
	printlnIndent(autoUpdateStatusLine(env, version.Version, latest, latestOK))
}

// humanInterval renders a cadence the way a person writes one. Go's
// Duration.String gives "30m0s" and "5m0s" — machine-shaped noise on a screen
// engineers read while something is already wrong, and the reason these
// intervals used to be retyped into the copy by hand in the first place.
// Anything not a whole number of hours or minutes falls back to Duration's own
// formatting rather than inventing a rounding rule.
func humanInterval(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return d.String()
	}
}

// autoUpdateStatusLine builds doctor's auto-update line. Pure so every branch is
// assertable without a network probe or an environment — the wrong branch here
// is not a cosmetic bug, it is doctor telling an engineer the opposite of what
// the daemon will do.
//
// Every fact in the line is READ from selfupdate rather than restated here, and
// all three restatements this replaces were wrong at the time of writing:
//
//   - "is there an update for me?" now asks selfupdate.IsNewer, the predicate
//     that authorizes an actual swap. The string comparison it replaces told a
//     machine running AHEAD of the published release that a newer one existed.
//   - the cadence is selfupdate.CheckInterval. The hardcoded "24h" here outlived
//     the move to 30m by several releases.
//   - the opt-out is selfupdate.EnvDisablesAutoUpdate, which trims and folds
//     case. The inline comparison it replaces did neither, so a machine set to
//     `TRUE` had auto-update off and was told it was on.
func autoUpdateStatusLine(envOptOut, current, latest string, latestOK bool) string {
	if selfupdate.EnvDisablesAutoUpdate(envOptOut) {
		return fmt.Sprintf("%s auto-update disabled (%s set)", warnGlyph, selfupdate.EnvNoAutoUpdate)
	}
	if current == "dev" || current == "" {
		return fmt.Sprintf("%s auto-update inactive for dev build", warnGlyph)
	}
	if latestOK {
		if selfupdate.IsNewer(current, latest) {
			return fmt.Sprintf("%s auto-update on — newer release available (%s); it installs on the next check (every %s) while watching",
				okGlyph, latest, humanInterval(selfupdate.CheckInterval))
		}
		// Not newer covers both equal and ahead-of-latest (a local build, a
		// yanked release). Report the version actually RUNNING: claiming to be
		// "up to date (0.12.1)" while running 0.13.0 is the same lie in reverse.
		return fmt.Sprintf("%s auto-update on — up to date (%s)", okGlyph, current)
	}
	return fmt.Sprintf("%s auto-update on — silent self-update while watching (org policy may disable or pin)", okGlyph)
}

// countBufferedEvents reports how many captured events are still waiting to be
// uploaded — the undelivered depth of the send queue.
//
// It used to count the LEDGER (buffer.jsonl + its rotated segments), which was
// never an upload backlog: the ledger is an append-only audit trail that
// nothing drains, so the panel reported every event ever captured as "pending
// upload" indefinitely, and could only say "all events shipped" on a device
// that had captured nothing at all. The outbox is the real queue — ask it.
func countBufferedEvents() int {
	return outbox.PendingCount()
}
