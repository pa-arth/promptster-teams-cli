package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"os"
)

func cmdBlameBackfill(args []string) int {
	fs := flag.NewFlagSet("blame-backfill", flag.ContinueOnError)
	root := fs.String("repo", ".", "local Git checkout")
	limit := fs.Int("limit", 100, "recent first-parent commits (1..500)")
	dry := fs.Bool("dry-run", false, "print metadata without queueing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		return 2
	}
	receipts, err := capture.RunLineOriginBackfill(*root, *limit, *dry)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *dry {
		_ = json.NewEncoder(os.Stdout).Encode(receipts)
	} else {
		fmt.Printf("Queued %d local blame receipts for delivery by the capture daemon.\n", len(receipts))
	}
	return 0
}
