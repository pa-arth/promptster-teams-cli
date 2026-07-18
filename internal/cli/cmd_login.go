package cli

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// cmdLogin onboards an individual contributor: it takes the per-engineer key
// (PSE-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX) their manager minted — pasted interactively
// or via --key —
// validates it, checks the ingest host is reachable, and persists it to
// ~/.promptster-teams/credentials (0600) so `watch` just works afterward.
func cmdLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	keyFlag := fs.String("key", "", "Developer key ("+ingest.KeyFormatHint+"); paste interactively if omitted")
	urlFlag := fs.String("api-url", "", "Override ingest base URL (default: hosted)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	fmt.Println()
	fmt.Println(brandBar("setup"))
	fmt.Println()
	printlnIndent(bodyStyle.Render("Capture your AI-coding sessions for your team. Paste the developer"))
	printlnIndent(bodyStyle.Render("key your manager gave you — it identifies your sessions, nothing else."))
	fmt.Println()

	key := strings.TrimSpace(*keyFlag)
	if key == "" {
		// Interactive paste — only when stdin is a terminal.
		if !stdinIsTTY() {
			fmt.Fprintf(os.Stderr, "  %s no key provided. Pass --key %s, or run `login` in a terminal.\n\n", errGlyph, ingest.KeyFormatHint)
			os.Exit(1)
		}
		fmt.Printf("  %s Paste your developer key: ", promptGlyph())
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() {
			key = strings.TrimSpace(sc.Text())
		}
		fmt.Println()
	}

	if !ingest.IsEngineerKey(key) {
		fmt.Printf("  %s that doesn't look like a developer key (expected %s).\n\n", errGlyph, ingest.KeyFormatHint)
		os.Exit(1)
	}

	apiURL := ingest.ResolveAPIURL(*urlFlag)

	// Best-effort reachability check — never blocks saving the key.
	if pingIngestHost(apiURL) {
		printlnIndent(fmt.Sprintf("%s reachable: %s", okGlyph, hostOf(apiURL)))
	} else {
		printlnIndent(fmt.Sprintf("%s couldn't reach %s — saved anyway; check the URL if capture fails", warnGlyph, hostOf(apiURL)))
	}

	creds := ingest.StoredCredentials{Token: key}
	// Only persist a non-default URL so the hosted default stays implicit.
	if apiURL != ingest.DefaultAPIURL {
		creds.ApiURL = apiURL
	}
	if err := ingest.SaveStoredCredentials(creds); err != nil {
		fmt.Printf("  %s could not save credentials: %v\n\n", errGlyph, err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println(indent(kvPanel("You're set",
		"key", ingest.MaskKey(key),
		"ingest", hostOf(apiURL),
		"stored", prettyHome(ingest.CredentialsPath()),
	)))
	fmt.Println()

	// Auto-start background capture so login is the only command a new engineer
	// has to run — capture "just works" afterward instead of waiting for a manual
	// `start`. StartDaemon is idempotent (no-ops if a supervisor is already alive)
	// and detaches, so it never holds the terminal.
	//
	// Scope the auto-started capture to the engineer's home directory rather than
	// login's incidental cwd. The watchers only capture transcripts whose recorded
	// cwd is inside the watch dir, so binding to wherever `login` happened to run
	// (an installer shell, /tmp, or a single repo) would silently miss their other
	// repos. Home spans them all. Only default when unset — an explicit
	// `PROMPTSTER_TEAMS_WATCH_DIR=/repo promptster-teams login` is respected, and
	// an explicit `promptster-teams start` from a repo still scopes narrowly.
	if os.Getenv("PROMPTSTER_TEAMS_WATCH_DIR") == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			_ = os.Setenv("PROMPTSTER_TEAMS_WATCH_DIR", home)
		}
	}
	pid, watchDir, already, startErr := capture.StartDaemon(nil)
	switch {
	case startErr != nil:
		printlnIndent(fmt.Sprintf("%s key saved, but couldn't auto-start capture: %v", warnGlyph, startErr))
		printlnIndent(dimStyle.Render("Start it yourself with ") + bodyStyle.Render("promptster-teams start") + dimStyle.Render("."))
	case already:
		printlnIndent(fmt.Sprintf("%s capture already running in the background (pid %d)", okGlyph, pid))
	default:
		printlnIndent(fmt.Sprintf("%s capturing in the background (pid %d)", okGlyph, pid))
		printlnIndent(dimStyle.Render("Watching ") + bodyStyle.Render(prettyHome(watchDir)) + dimStyle.Render(" · stop with ") + bodyStyle.Render("promptster-teams stop"))
	}

	if startErr == nil {
		enableAutostartOnLogin()
	}
	fmt.Println()
}

// enableAutostartOnLogin installs the login-time service so capture survives a
// reboot, and reports it. Without this, `login` starts a watcher that dies at
// the next restart and never returns — the engineer sees a success line, the
// dashboard silently goes quiet days later, and nobody connects the two. The
// service is the only way capture comes back, so login installs it rather than
// leaving it to an `autostart enable` most people never run.
//
// A failure here is a warning, not an error: capture IS running (the caller only
// reaches this after StartDaemon succeeded), so login has already delivered its
// main promise. Only the reboot guarantee is missing, and `autostart enable`
// recovers it.
//
// The service kicks off its own watcher immediately, which loses the
// single-instance lock race against the daemon we just started and exits 0. That
// bow-out is deliberate on both platforms: the mac plist's
// KeepAlive{SuccessfulExit:false} and the systemd unit's Restart=on-failure both
// decline to revive a clean exit, so the installed job simply idles until the
// next login instead of fighting the running daemon.
func enableAutostartOnLogin() {
	mgr := service.New()
	if err := mgr.Enable(); err != nil {
		printlnIndent(fmt.Sprintf("%s capture won't resume after a reboot: %v", warnGlyph, err))
		printlnIndent(dimStyle.Render("Retry with ") + bodyStyle.Render("promptster-teams autostart enable") + dimStyle.Render("."))
		return
	}
	printlnIndent(fmt.Sprintf("%s autostart enabled — capture resumes at every login", okGlyph))
	printlnIndent(dimStyle.Render("Turn it off with ") + bodyStyle.Render("promptster-teams autostart disable") + dimStyle.Render("."))
}

// stdinIsTTY reports whether stdin is an interactive terminal (vs a pipe/CI),
// so login only prompts when a human can actually type.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// pingIngestHost does a quick GET to the API base to confirm reachability. Any
// HTTP response (even 404) means the host is up; only transport errors fail.
func pingIngestHost(apiURL string) bool {
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// hostOf returns the host portion of a URL for display, falling back to the raw
// string if it doesn't parse.
func hostOf(u string) string {
	if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return u
}

// prettyHome abbreviates the user's home dir to ~ for display. Delegates to the
// shared state.HomeRelative so the display collapse and the workdir emitted on
// prompt events (normalize) stay one implementation.
func prettyHome(p string) string {
	return state.HomeRelative(p)
}
