package capture

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// IgnoreMarker is the file that keeps a directory tree out of capture: an AI
// session whose working directory is at or under a directory containing it is
// never captured. Drop an empty one at a repo root.
const IgnoreMarker = ".promptsterignore"

// ExcludeFilePath is the per-machine exclude list: one directory per line
// (`~/` expands to home, `#` starts a comment). Sessions working at or under
// any of them are never captured.
func ExcludeFilePath() string {
	return filepath.Join(state.GlobalPromptsterDir(), "exclude")
}

// excludePaths reads ExcludeFilePath. Missing file = no excludes.
func excludePaths() []string {
	data, err := os.ReadFile(ExcludeFilePath())
	if err != nil {
		return nil
	}
	home, _ := os.UserHomeDir()
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "~" || strings.HasPrefix(line, "~/") {
			line = filepath.Join(home, strings.TrimPrefix(line, "~"))
		}
		if !filepath.IsAbs(line) {
			continue
		}
		out = append(out, resolvePath(line))
	}
	return out
}

// captureExcluded reports whether a session working in dir (resolved) is kept
// out of capture by the exclude list or by an IgnoreMarker in dir or any
// ancestor. Called at classification time, whose answer is cached per
// transcript; editing the exclude list re-checks every cached answer (it is
// folded into captureRootsFingerprint).
//
// ponytail: a marker added to a repo AFTER a session there was classified only
// applies to that repo's later sessions; restart capture to re-check sooner.
func captureExcluded(dir string) bool {
	if dir == "" {
		return false
	}
	for _, p := range excludePaths() {
		if pathWithin(dir, p) {
			return true
		}
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, IgnoreMarker)); err == nil {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}
