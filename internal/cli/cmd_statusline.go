package cli

import (
	"fmt"
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
)

// cmdStatusline dispatches the `statusline` subcommands. `run` is the hot path
// (Claude Code invokes it every tick); the rest are one-shot management verbs.
func cmdStatusline(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "run":
		// The shim. Reads stdin, spools the window reading, passes the prior
		// statusline through. MUST stay fail-open + fast — always exits 0.
		return capture.RunStatuslineShim()
	case "enable":
		return statuslineEnable()
	case "disable":
		return statuslineDisable()
	case "status", "":
		return statuslineStatus()
	default:
		fmt.Fprintf(os.Stderr, "unknown statusline subcommand: %s\n", sub)
		fmt.Fprintln(os.Stderr, "usage: promptster-teams statusline <enable|disable|status>")
		return 1
	}
}

// statuslineEnable wraps (or installs) the Claude statusline shim, disclosing to
// the engineer what changed — this is the consented install: we never touch a
// statusline without telling them their own line still renders.
func statuslineEnable() int {
	res, err := capture.EnableStatusline()
	if err != nil {
		printlnIndent(fmt.Sprintf("%s couldn't enable statusline capture: %v", errGlyph, err))
		return 1
	}

	fmt.Println()
	fmt.Println(brandBar("statusline"))
	fmt.Println()
	switch {
	case res.AlreadyEnabled:
		printlnIndent(fmt.Sprintf("%s already tracking your Claude usage — nothing to change", okGlyph))
	case res.WrappedExisting:
		printlnIndent(fmt.Sprintf("%s wrapped your existing statusline to add usage tracking", okGlyph))
		printlnIndent(dimStyle.Render("Your statusline still renders — ") + bodyStyle.Render(truncateCmd(res.PriorCommand)))
	case res.Rewrapped:
		printlnIndent(fmt.Sprintf("%s your statusline changed — re-wrapped the new one", okGlyph))
		printlnIndent(dimStyle.Render("Still rendering ") + bodyStyle.Render(truncateCmd(res.PriorCommand)))
	case res.InstalledFresh:
		printlnIndent(fmt.Sprintf("%s installed a statusline that shows your 5h/weekly usage", okGlyph))
	}
	printlnIndent(dimStyle.Render("Only the two usage percentages + reset times leave your machine. Turn off with ") + bodyStyle.Render("promptster-teams statusline disable") + dimStyle.Render("."))
	fmt.Println()
	return 0
}

// statuslineDisable restores the prior statusline verbatim.
func statuslineDisable() int {
	if err := capture.DisableStatusline(); err != nil {
		printlnIndent(fmt.Sprintf("%s couldn't disable statusline capture: %v", errGlyph, err))
		return 1
	}
	fmt.Println()
	fmt.Println(brandBar("statusline"))
	fmt.Println()
	printlnIndent(fmt.Sprintf("%s Claude usage tracking off — your statusline is restored", okGlyph))
	fmt.Println()
	return 0
}

// statuslineStatus runs the effective-statusline drift check and reports it.
func statuslineStatus() int {
	dir, _ := os.Getwd()
	fmt.Println()
	fmt.Println(brandBar("statusline"))
	fmt.Println()
	for _, l := range capture.StatuslineDoctor(dir) {
		printlnIndent(fmt.Sprintf("%s %s", statuslineGlyph(l), l.Text))
	}
	fmt.Println()
	return 0
}

func statuslineGlyph(l capture.StatuslineDoctorLine) string {
	switch {
	case l.OK:
		return okGlyph
	case l.Warn:
		return warnGlyph
	default:
		return errGlyph
	}
}

// truncateCmd shortens a command string for one-line display.
func truncateCmd(s string) string {
	const max = 60
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
