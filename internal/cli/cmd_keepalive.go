package cli

import (
	"fmt"
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
)

// cmdKeepalive dispatches `keepalive enable|disable|status`: the opt-in for
// keeping idle Claude Code sessions' prompt cache warm (capture/cache_keepalive.go).
func cmdKeepalive(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "enable", "disable":
		if err := capture.SetCacheKeepalive(sub == "enable"); err != nil {
			printlnIndent(fmt.Sprintf("%s couldn't save the setting: %v", errGlyph, err))
			return 1
		}
	case "status", "":
	default:
		fmt.Fprintf(os.Stderr, "unknown keepalive subcommand: %s\n", sub)
		fmt.Fprintln(os.Stderr, "usage: promptster-teams keepalive <enable|disable|status>")
		return 1
	}
	fmt.Println()
	fmt.Println(brandBar("keepalive"))
	fmt.Println()
	printlnIndent(fmt.Sprintf("%s %s", okGlyph, keepaliveStatusLine()))
	if capture.CacheKeepaliveEnabled() {
		printlnIndent(dimStyle.Render("Idle Claude Code sessions get a one-word forked ping 45–60 min after their last use, for up to 4h,"))
		printlnIndent(dimStyle.Render("so resuming reads the cached context instead of paying to rebuild it. Your sessions aren't modified."))
		printlnIndent(dimStyle.Render("Each ping costs a cache read of the session's context and counts toward your Claude usage."))
	}
	fmt.Println()
	return 0
}

// keepaliveStatusLine is the one-line state shown here and on both status surfaces.
func keepaliveStatusLine() string {
	if capture.CacheKeepaliveEnabled() {
		return "on — idle Claude sessions' prompt cache kept warm for 4h"
	}
	return "off — `promptster-teams keepalive enable` to stop paying for cache rebuilds on resume"
}
