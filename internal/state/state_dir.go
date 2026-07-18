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

// HomeRelative collapses a $HOME prefix to "~" for display/transport, so a path
// can name WHERE a session ran without leaking the OS username the absolute path
// carries. Empty in → empty out; exact $HOME → "~"; a path under $HOME → "~/…";
// anything else (an absolute path outside home, e.g. /tmp/ws — no username in it)
// is returned unchanged. The boundary check requires a separator after $HOME so a
// sibling like /Users/foobar is not mangled into ~bar.
func HomeRelative(p string) string {
	if p == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}

// activeWorkspacePath returns the path to the pointer file that tells hooks
// which workspace is currently active.
func activeWorkspacePath() string {
	return filepath.Join(GlobalPromptsterDir(), "active-workspace")
}

// readActiveWorkspace reads the pointer to the current workspace, if one was
// set. Today nothing writes it (capture keeps state in the global dir), so this
// consistently returns "" and StateDir falls through to the global fallback;
// the branch stays so a future per-workspace mode can light it up.
func readActiveWorkspace() string {
	data, err := os.ReadFile(activeWorkspacePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
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
