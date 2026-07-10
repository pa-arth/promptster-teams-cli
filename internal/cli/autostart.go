package cli

import (
	"fmt"
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
)

// cmdAutostart manages the per-user OS service that relaunches capture at login
// so it survives reboots with zero user action — the fix for capture silently
// dying on reboot and under-counting the seat-utilization metric. The service
// runs `promptster-teams watch` under the platform supervisor (launchd on macOS,
// systemd --user on Linux, Task Scheduler on Windows). Subcommands: enable,
// disable, status.
func cmdAutostart(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	mgr := service.New()

	switch sub {
	case "enable":
		return autostartEnable(mgr)
	case "disable":
		return autostartDisable(mgr)
	case "status", "":
		return autostartStatus(mgr)
	default:
		fmt.Fprintf(os.Stderr, "unknown autostart subcommand: %s\n", sub)
		fmt.Fprintln(os.Stderr, "usage: promptster-teams autostart <enable|disable|status>")
		return 1
	}
}

func autostartEnable(mgr service.Manager) int {
	fmt.Println()
	fmt.Println(brandBar("autostart"))
	fmt.Println()

	// A keyless service just crash-loops — the watcher exits immediately with no
	// credential. Require a resolvable key before installing.
	if token, _ := ingest.ResolveToken(""); token == "" {
		printlnIndent(fmt.Sprintf("%s no developer key configured — run `promptster-teams login` first.", errGlyph))
		printlnIndent(dimStyle.Render("Autostart would crash-loop without a key."))
		fmt.Println()
		return 1
	}

	if err := mgr.Enable(); err != nil {
		printlnIndent(fmt.Sprintf("%s could not enable autostart: %v", errGlyph, err))
		fmt.Println()
		return 1
	}

	printlnIndent(fmt.Sprintf("%s autostart enabled — capture runs at every login and is kept alive across reboots.", okGlyph))
	if _, detail, _ := mgr.Status(); detail != "" {
		printlnIndent(dimStyle.Render(detail))
	}
	fmt.Println()
	return 0
}

func autostartDisable(mgr service.Manager) int {
	fmt.Println()
	fmt.Println(brandBar("autostart"))
	fmt.Println()

	if err := mgr.Disable(); err != nil {
		printlnIndent(fmt.Sprintf("%s could not disable autostart: %v", errGlyph, err))
		fmt.Println()
		return 1
	}

	printlnIndent(fmt.Sprintf("%s autostart disabled — capture will no longer start at login.", okGlyph))
	printlnIndent(dimStyle.Render("Run `promptster-teams stop` to also stop any capture running now."))
	fmt.Println()
	return 0
}

func autostartStatus(mgr service.Manager) int {
	installed, detail, err := mgr.Status()

	fmt.Println()
	fmt.Println(brandBar("autostart"))
	fmt.Println()

	if err != nil {
		printlnIndent(fmt.Sprintf("%s could not read autostart status: %v", errGlyph, err))
		fmt.Println()
		return 1
	}

	glyph := warnGlyph
	if installed {
		glyph = okGlyph
	}
	printlnIndent(fmt.Sprintf("%s %s", glyph, detail))
	if !installed {
		printlnIndent(dimStyle.Render("Enable with `promptster-teams autostart enable`."))
	}
	fmt.Println()
	return 0
}
