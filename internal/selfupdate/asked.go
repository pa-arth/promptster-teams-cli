package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// The ask-once-per-VERSION record, and the reason ConsentAsk is survivable.
//
// The update check runs every 30 minutes. Without this, an engineer who chose
// "ask me each release" and clicked Later would get the same dialog again half
// an hour later, and again after that, forever — a background process popping an
// "install this?" box six times a workday. That is indistinguishable from
// malware, and it trains the engineer to dismiss the dialog on sight, which ends
// with a fleet that never updates: the bug this whole change set exists to fix,
// re-created out of good intentions.
//
// So a declined version is declined until a DIFFERENT version appears. The
// engineer is asked once per release, which is what "ask me each release" says
// on the tin.
//
// It records only the version, not the answer. An accepted prompt installs
// immediately and the process re-execs, so "accepted" has no future to affect;
// the only state worth keeping is "this version has already had its turn".
type askedRecord struct {
	Version string `json:"version"`
}

func askedPath() string {
	return filepath.Join(state.GlobalPromptsterDir(), "update-asked.json")
}

// loadAskedVersion returns the version the engineer was last prompted about, or
// "" if none. Every failure returns "" — the benign direction, since the cost of
// forgetting is one extra dialog and the cost of a false positive is silently
// skipping a release the engineer never saw.
func loadAskedVersion() string {
	data, err := os.ReadFile(askedPath())
	if err != nil {
		return ""
	}
	var rec askedRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return ""
	}
	return strings.TrimSpace(rec.Version)
}

// saveAskedVersion records that this version has had its prompt. Best-effort: a
// write failure costs a repeated dialog, which is annoying but not harmful, and
// must never take down capture.
func saveAskedVersion(version string) {
	data, err := json.Marshal(askedRecord{Version: version})
	if err != nil {
		return
	}
	p := askedPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, data, 0o600)
}
