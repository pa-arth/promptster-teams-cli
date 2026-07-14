//go:build darwin

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

type darwinManager struct{}

// New returns the launchd-backed autostart manager for macOS.
func New() Manager { return darwinManager{} }

// logPath is where the launchd job tees the watcher's stdout/stderr (macOS
// only; systemd logs to journald and Task Scheduler inherits). It sits alongside
// the manual daemon's log under ~/.promptster-teams.
func logPath() string { return filepath.Join(state.GlobalPromptsterDir(), "daemon.log") }

// plistPath is ~/Library/LaunchAgents/ai.promptster.teams.plist.
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// guiTarget is the launchd domain/service specifier for the current user's GUI
// session, e.g. gui/501 or gui/501/ai.promptster.teams.
func guiTarget(withLabel bool) string {
	t := fmt.Sprintf("gui/%d", os.Getuid())
	if withLabel {
		t += "/" + label
	}
	return t
}

func (darwinManager) Enable() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(renderPlist(state.SelfBin(), logPath(), home)), 0o600); err != nil {
		return err
	}
	// Clear any stale registration first (ignore errors — it may not be loaded),
	// then load the fresh plist and kick it running now.
	// #nosec G204 -- constant subcommands; target is gui/<uid>/<label>, not user input.
	_ = exec.Command("launchctl", "bootout", guiTarget(true)).Run()
	// #nosec G204 -- p is plistPath() under the user's home; target is uid-derived.
	if out, err := exec.Command("launchctl", "bootstrap", guiTarget(false), p).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %v: %s", err, out)
	}
	// #nosec G204 -- constant subcommands; target is uid/label-derived.
	_ = exec.Command("launchctl", "kickstart", "-k", guiTarget(true)).Run()
	return nil
}

// Stop boots the job out of the current GUI domain, which disarms KeepAlive and
// terminates the watcher, but leaves the plist on disk. launchd re-bootstraps
// everything in ~/Library/LaunchAgents at the next login, so autostart survives
// — this only ends the current session's capture.
func (darwinManager) Stop() error {
	if installed, _, _ := (darwinManager{}).Status(); !installed {
		return nil
	}
	// #nosec G204 -- constant subcommands; target is gui/<uid>/<label>, not user input.
	if out, err := exec.Command("launchctl", "bootout", guiTarget(true)).CombinedOutput(); err != nil {
		// bootout exits non-zero when the job isn't loaded — that's the desired
		// end state, not a failure. Only a still-loaded job is worth reporting.
		if err := exec.Command("launchctl", "print", guiTarget(true)).Run(); err == nil {
			return fmt.Errorf("launchctl bootout failed: %s", out)
		}
	}
	return nil
}

func (darwinManager) Disable() error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	// #nosec G204 -- constant subcommands; target is uid/label-derived.
	_ = exec.Command("launchctl", "bootout", guiTarget(true)).Run()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (darwinManager) Status() (bool, string, error) {
	p, err := plistPath()
	if err != nil {
		return false, "", err
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return false, "not enabled", nil
		}
		return false, "", err
	}
	// #nosec G204 -- constant subcommands; target is uid/label-derived.
	if err := exec.Command("launchctl", "print", guiTarget(true)).Run(); err != nil {
		return true, "installed but not loaded (try re-running enable)", nil
	}
	return true, "enabled (launchd, runs at login)", nil
}
