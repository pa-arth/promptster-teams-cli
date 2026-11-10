package cli

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/policy"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// employeeNotice is the GDPR Art. 13 / 88 notice an engineer acknowledges
// before capture starts. Changing it materially means bumping
// state.NoticeVersion so everyone is asked again.
var employeeNotice = []string{
	"Before capture starts — what your employer will see:",
	"",
	"• What is captured: the prompts you send to Claude Code, Codex and Cursor,",
	"  tool-call metadata (file paths, line counts, commands, exit codes), token",
	"  usage, models and timestamps. On first run that includes the previous 28",
	"  days of sessions already on this machine. Secrets are redacted on this",
	"  device and source code (diffs, file contents, command output) is never sent.",
	"• Who can see it: this key links every session to you by name in your",
	"  employer's Promptster Teams org. Your org's admins can open any of your",
	"  sessions, including your prompt text.",
	"• Retention: Teams session data is deleted after 90 days.",
	"• Roles: your employer is the data controller; Promptster is its processor.",
	"• Your data: for a copy of your data or any privacy question, ask your admin",
	"  or email privacy@promptster.ai.",
	"• Control: `promptster-teams pause [--for 2h]` / `resume` pauses capture,",
	"  `promptster-teams stop` stops it, and an empty " + capture.IgnoreMarker + " file in",
	"  a repo (or a path in ~/.promptster-teams/exclude) keeps that tree out.",
}

// ensureNoticeAcknowledged shows the employee notice once per notice version
// and returns whether capture may proceed. accept is --accept-notice: the only
// way to acknowledge without a terminal, so a provisioning script has to say
// so explicitly rather than inheriting a default.
func ensureNoticeAcknowledged(accept bool) bool {
	if state.NoticeAcknowledged() {
		return true
	}
	fmt.Println()
	for _, line := range employeeNotice {
		printlnIndent(bodyStyle.Render(line))
	}
	fmt.Println()
	switch {
	case accept:
		// stated on the command line; record it below
	case !stdinIsTTY():
		fmt.Fprintf(os.Stderr, "  %s re-run with --accept-notice to acknowledge this notice non-interactively. Nothing was started.\n\n", errGlyph)
		return false
	default:
		fmt.Printf("  %s I have read this notice — start capture? [y/N] ", promptGlyph())
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Println()
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			printlnIndent(fmt.Sprintf("%s not acknowledged — nothing was started.", warnGlyph))
			fmt.Println()
			return false
		}
	}
	ack := state.LoadNoticeAck()
	ack.Version = state.NoticeVersion
	ack.AcknowledgedAt = time.Now().UTC()
	if err := state.SaveNoticeAck(ack); err != nil {
		// Proceeding unrecorded would leave the daemon treating this machine as
		// unacknowledged (no backfill) and asking again next time — the safe side.
		printlnIndent(fmt.Sprintf("%s could not record the acknowledgement: %v", warnGlyph, err))
	}
	return true
}

// takeFlag removes a boolean flag from args, reporting whether it was present.
// `start` forwards its args to the watch flag set, which does not know it.
func takeFlag(args []string, name string) ([]string, bool) {
	out := args[:0:0]
	found := false
	for _, a := range args {
		if a == name || a == "-"+strings.TrimLeft(name, "-") {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

func cmdPause(args []string) int {
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	forFlag := fs.Duration("for", 0, "Pause for this long (e.g. 30m, 2h); default until `resume`")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *forFlag < 0 {
		fmt.Fprintf(os.Stderr, "  %s --for must be positive\n", errGlyph)
		return 2
	}
	var until time.Time
	if *forFlag > 0 {
		until = time.Now().Add(*forFlag)
	}
	if err := state.PauseCapture(until); err != nil {
		fmt.Fprintf(os.Stderr, "  %s could not pause: %v\n", errGlyph, err)
		return 1
	}
	printlnIndent(fmt.Sprintf("%s %s", okGlyph, captureVisibilityLine(time.Now())))
	printlnIndent(dimStyle.Render("Nothing from your AI sessions is captured or sent while paused, and it is not sent later. The presence heartbeat continues and reports \"paused\"."))
	return 0
}

func cmdResume() int {
	if err := state.ResumeCapture(); err != nil {
		fmt.Fprintf(os.Stderr, "  %s could not resume: %v\n", errGlyph, err)
		return 1
	}
	printlnIndent(fmt.Sprintf("%s %s", okGlyph, captureVisibilityLine(time.Now())))
	return 0
}

// captureVisibilityLine is the one-line answer to "is my work being captured,
// and who can see it" that status and pause/resume print.
func captureVisibilityLine(now time.Time) string {
	paused, until := state.CapturePaused(now)
	switch {
	case !paused:
		return "capturing — your org admins can view these sessions"
	case until.IsZero():
		return "paused until you run `promptster-teams resume`"
	default:
		return "paused until " + until.Local().Format("Mon Jan 2 15:04 MST")
	}
}

// proseCapture asks the org policy whether assistant replies are captured,
// and prints a one-time notice when that turns on. Returns the status row value.
func proseCapture() string {
	token, _ := ingest.ResolveToken("")
	if token == "" {
		return "not captured"
	}
	r := policy.NewResolver(token)
	r.Refresh()
	on, known := r.CaptureAssistantProseDecision()
	ack := state.LoadNoticeAck()
	switch {
	case on && !ack.ProseNotified:
		fmt.Println()
		printlnIndent(fmt.Sprintf("%s Your org has turned on capture of AI assistant replies.", warnGlyph))
		printlnIndent(bodyStyle.Render("The text of Claude/Codex/Cursor responses (code stripped on-device) is now"))
		printlnIndent(bodyStyle.Render("sent along with your prompts, and your org's admins can read it."))
		fmt.Println()
		ack.ProseNotified = true
		_ = state.SaveNoticeAck(ack)
	case known && !on && ack.ProseNotified:
		// Turned off: reset so turning it on again is announced again.
		ack.ProseNotified = false
		_ = state.SaveNoticeAck(ack)
	}
	if on {
		return "captured — your org turned on assistant-reply capture (code stripped)"
	}
	return "not captured"
}
