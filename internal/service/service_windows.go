//go:build windows

package service

import (
	"fmt"
	"os/exec"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

type windowsManager struct{}

// New returns the Task Scheduler-backed autostart manager for Windows.
func New() Manager { return windowsManager{} }

func (windowsManager) Enable() error {
	// #nosec G204 -- constant subcommands; the only interpolated value is
	// state.SelfBin() (our own running binary), not user input.
	if out, err := exec.Command("schtasks", renderTaskArgs(state.SelfBin())...).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks /Create failed: %v: %s", err, out)
	}
	// The task only auto-runs at the next logon, so start it now too.
	// #nosec G204 -- constant subcommands + fixed task name.
	_ = exec.Command("schtasks", "/Run", "/TN", taskName).Run()
	return nil
}

// Stop ends the running task instance but leaves it registered, so it runs
// again at the next logon. The ONLOGON task carries no restart-on-failure
// policy, so Windows never resurrects a killed watcher the way launchd and
// systemd do — this exists to keep the Manager contract uniform and to end the
// instance Task Scheduler is tracking.
func (windowsManager) Stop() error {
	if installed, _, _ := (windowsManager{}).Status(); !installed {
		return nil
	}
	// /End exits non-zero when the task isn't currently running, which is the
	// desired end state — best-effort, not an error.
	// #nosec G204 -- constant subcommands + fixed task name.
	_ = exec.Command("schtasks", "/End", "/TN", taskName).Run()
	return nil
}

func (windowsManager) Disable() error {
	if installed, _, _ := (windowsManager{}).Status(); !installed {
		return nil
	}
	// #nosec G204 -- constant subcommands + fixed task name.
	if out, err := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks /Delete failed: %v: %s", err, out)
	}
	return nil
}

func (windowsManager) Status() (bool, string, error) {
	// #nosec G204 -- constant subcommands + fixed task name. /Query exits
	// non-zero when the task is absent.
	if err := exec.Command("schtasks", "/Query", "/TN", taskName).Run(); err != nil {
		return false, "not enabled", nil
	}
	return true, "enabled (Task Scheduler, runs at logon)", nil
}
