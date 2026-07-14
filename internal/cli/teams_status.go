package cli

import (
	"bufio"
	"fmt"
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/selfupdate"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
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
	root := os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR")
	if root == "" {
		root, _ = os.Getwd()
	}

	daemon := "not running — `promptster-teams login` starts it, or `autostart enable` for reboots"
	if snap := capture.Snapshot(); snap.Live {
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
		"presence", fmt.Sprintf("heartbeat every %s during watch", capture.PresenceHeartbeatInterval),
		"buffered", fmt.Sprintf("%d events", countBufferedEvents()),
	)))
	fmt.Println()
}

// cmdTeamsDoctor diagnoses the credential, ingest reachability, and transcript
// dir. The reachability probe is a plain GET to the API base (not an auth probe
// against the ingest endpoint), so it never writes anything.
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

	printlnIndent(fmt.Sprintf("%s presence heartbeat every %s while watching — device + tools only, no identity/email", okGlyph, capture.PresenceHeartbeatInterval))

	if installed, detail, serr := service.New().Status(); serr == nil && installed {
		printlnIndent(fmt.Sprintf("%s autostart %s", okGlyph, detail))
	} else {
		printlnIndent(fmt.Sprintf("%s autostart not enabled — run `promptster-teams autostart enable` so capture survives reboots", warnGlyph))
	}

	fmt.Println()
	if ok {
		printlnIndent(dimStyle.Render("Ready. Run ") + bodyStyle.Render("promptster-teams watch") + dimStyle.Render(" from a repo."))
	} else {
		printlnIndent(dimStyle.Render("Run ") + bodyStyle.Render("promptster-teams login") + dimStyle.Render(" to get set up."))
	}
	fmt.Println()
}

// printAutoUpdateStatus renders the self-updater's read-only state for doctor:
// whether it is on, disabled by the per-machine env opt-out, or (best-effort)
// whether a newer release exists. Org-policy disable/pin is resolved only while
// watching (it needs an authenticated fetch), so doctor reports the machine-
// local switch and a short-timeout latest-version probe that degrades silently.
func printAutoUpdateStatus() {
	if v := os.Getenv("PROMPTSTER_TEAMS_NO_AUTO_UPDATE"); v == "1" || v == "true" || v == "yes" || v == "on" {
		printlnIndent(fmt.Sprintf("%s auto-update disabled (PROMPTSTER_TEAMS_NO_AUTO_UPDATE set)", warnGlyph))
		return
	}
	if version.Version == "dev" || version.Version == "" {
		printlnIndent(fmt.Sprintf("%s auto-update inactive for dev build", warnGlyph))
		return
	}
	if latest, ok := selfupdate.LatestVersionBestEffort(3 * time.Second); ok {
		if latest != version.Version {
			printlnIndent(fmt.Sprintf("%s auto-update on — newer release available (%s); it installs on the next 24h check while watching", okGlyph, latest))
		} else {
			printlnIndent(fmt.Sprintf("%s auto-update on — up to date (%s)", okGlyph, latest))
		}
		return
	}
	printlnIndent(fmt.Sprintf("%s auto-update on — silent self-update while watching (org policy may disable or pin)", okGlyph))
}

// countBufferedEvents counts every retained ledger segment, not just the live
// one: the ledger rotates once it gets large, and counting only the live segment
// would make the number collapse at each rotation as if events had been lost.
func countBufferedEvents() int {
	n := 0
	for seg := 0; seg <= state.LedgerRetainedSegments; seg++ {
		n += countSegmentLines(state.LedgerSegmentPath(seg))
	}
	return n
}

func countSegmentLines(path string) int {
	// #nosec G304 -- path is derived from state.HookBufferPath(), not user input.
	f, err := os.Open(path)
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
