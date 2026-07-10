package cli

import (
	"bufio"
	"fmt"
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// loadSession builds the teams capture context. The ingest credential is a
// per-engineer key (PSE-XXXX-XXXX) resolved with flag > env > stored-file
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

	daemon := "not running — start with `promptster-teams start`"
	if snap := capture.Snapshot(); snap.Live {
		daemon = fmt.Sprintf("running (pid %d)", snap.DaemonPID)
	}

	fmt.Println()
	fmt.Println(brandBar("status"))
	fmt.Println()
	fmt.Println(indent(kvPanel("capture",
		"key", keyDisplay(token, source),
		"ingest", hostOf(apiURL),
		"watch", root,
		"daemon", daemon,
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

	fmt.Println()
	if ok {
		printlnIndent(dimStyle.Render("Ready. Run ") + bodyStyle.Render("promptster-teams watch") + dimStyle.Render(" from a repo."))
	} else {
		printlnIndent(dimStyle.Render("Run ") + bodyStyle.Render("promptster-teams login") + dimStyle.Render(" to get set up."))
	}
	fmt.Println()
}

func countBufferedEvents() int {
	f, err := os.Open(state.HookBufferPath())
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
