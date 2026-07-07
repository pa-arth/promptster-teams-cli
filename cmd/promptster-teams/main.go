package main

import (
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args))
}
