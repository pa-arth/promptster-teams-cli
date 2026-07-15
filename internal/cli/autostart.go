package cli

import (
	"fmt"
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
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
	case "repair":
		return autostartRepair(mgr)
	default:
		fmt.Fprintf(os.Stderr, "unknown autostart subcommand: %s\n", sub)
		fmt.Fprintln(os.Stderr, "usage: promptster-teams autostart <enable|disable|status|repair>")
		return 1
	}
}

// autostartRepair re-points an ALREADY-ENABLED autostart unit at the binary
// running right now. It is what npm's postinstall calls, and it exists because
// the unit bakes an ABSOLUTE path (state.SelfBin()) at enable time and nothing
// ever revisits it.
//
// The concrete break it fixes: an npm install used to run the binary straight
// out of <pkg>/binaries/, so `autostart enable` wrote THAT path into the plist /
// systemd unit. The binary now lives in the managed dir and the package no
// longer ships binaries/ at all, so the next `npm i -g` deletes the exact file
// the supervisor is pointed at. Nothing fails loudly: the running daemon keeps
// its inode and capture looks fine until the next login, when launchd tries a
// path that is gone and capture silently never comes back. That is the failure
// autostart was built to prevent, reintroduced by moving the binary.
//
// It re-renders unconditionally rather than comparing the unit's current path to
// SelfBin(): reading the baked path back means parsing a plist, a systemd unit
// and schtasks XML — three parsers to avoid one idempotent re-render that is
// already a bootout/bootstrap cycle.
//
// NEVER returns non-zero. It runs inside `npm install`, where a non-zero exit
// aborts the install and leaves the engineer with no CLI at all — far worse than
// a stale unit path. It also deliberately skips autostartEnable's key check: the
// unit only exists because the engineer had a key when they enabled it, and a
// transient key problem must not be a reason to leave the path broken.
func autostartRepair(mgr service.Manager) int {
	enabled, _, err := mgr.Status()
	if err != nil || !enabled {
		return 0 // not enabled, or unsupported host — nothing to repair
	}
	if err := mgr.Enable(); err != nil {
		fmt.Fprintf(os.Stderr, "promptster-teams: could not re-point autostart at %s: %v\n", state.SelfBin(), err)
		fmt.Fprintln(os.Stderr, "promptster-teams: run `promptster-teams autostart enable` to fix it")
		return 0
	}
	fmt.Printf("promptster-teams: autostart re-pointed at %s\n", state.SelfBin())
	return 0
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
