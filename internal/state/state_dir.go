package state

import (
	"os"
	"path/filepath"
	"strings"
)

// globalPromptsterDir returns ~/.promptster-teams (global, persists across
// sessions). Kept distinct from the hiring CLI's ~/.promptster so a machine
// can run both without their state files colliding.
func GlobalPromptsterDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".promptster-teams")
}

// activeWorkspacePath returns the path to the pointer file that tells hooks
// which workspace is currently active.
func activeWorkspacePath() string {
	return filepath.Join(GlobalPromptsterDir(), "active-workspace")
}

// writeActiveWorkspace saves the workspace path so hooks can find state files.
func writeActiveWorkspace(workspacePath string) error {
	dir := GlobalPromptsterDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(activeWorkspacePath(), []byte(workspacePath), 0o600)
}

// readActiveWorkspace reads the pointer to the current workspace.
func readActiveWorkspace() string {
	data, err := os.ReadFile(activeWorkspacePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// clearActiveWorkspace removes the pointer file on session cleanup.
func clearActiveWorkspace() {
	_ = os.Remove(activeWorkspacePath())
}

// stateDir returns the directory for per-session state files.
// Priority: PROMPTSTER_STATE_DIR env > active-workspace pointer > ~/.promptster fallback.
func StateDir() string {
	// Env override (useful for tests)
	if p := os.Getenv("PROMPTSTER_STATE_DIR"); p != "" {
		return p
	}

	// Read the active workspace pointer
	ws := readActiveWorkspace()
	if ws != "" {
		return filepath.Join(ws, ".promptster")
	}

	// Fallback to global (pre-refactor compat)
	return GlobalPromptsterDir()
}
