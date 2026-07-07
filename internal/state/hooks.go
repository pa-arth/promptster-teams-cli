package state

import (
	"os"
	"path/filepath"
	"runtime"
)

// promptsterBin returns the canonical path to the promptster-teams binary in
// the user's install directory (~/.promptster-teams/bin/promptster-teams).
// The watchers use it to re-exec themselves as background daemons.
func PromptsterBin() string {
	home, _ := os.UserHomeDir()
	name := "promptster-teams"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(home, ".promptster-teams", "bin", name)
}
