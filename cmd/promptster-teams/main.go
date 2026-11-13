package main

import (
	"os"

	"github.com/promptster-ai/promptster-teams-cli/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args))
}
