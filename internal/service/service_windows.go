//go:build windows

package service

import (
	"fmt"
	"os/exec"
)

type windowsManager struct{}

// New returns the Task Scheduler-backed autostart manager for Windows.
func New() Manager { return windowsManager{} }

func (windowsManager) Enable() error {
	// #nosec G204 -- constant subcommands; the only interpolated value is
	// state.PromptsterBin() (the install path), not user input.
	if out, err := exec.Command("schtasks", renderTaskArgs(binPath())...).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks /Create failed: %v: %s", err, out)
	}
	// The task only auto-runs at the next logon, so start it now too.
	// #nosec G204 -- constant subcommands + fixed task name.
	_ = exec.Command("schtasks", "/Run", "/TN", taskName).Run()
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
