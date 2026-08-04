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
	if st, _ := (windowsManager{}).Status(); !st.Installed {
		return nil
	}
	// /End exits non-zero when the task isn't currently running, which is the
	// desired end state — best-effort, not an error.
	// #nosec G204 -- constant subcommands + fixed task name.
	_ = exec.Command("schtasks", "/End", "/TN", taskName).Run()
	return nil
}

func (windowsManager) Disable() error {
	if st, _ := (windowsManager{}).Status(); !st.Installed {
		return nil
	}
	// #nosec G204 -- constant subcommands + fixed task name.
	if out, err := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks /Delete failed: %v: %s", err, out)
	}
	return nil
}

// Status reports the ONLOGON task. Loaded tracks Installed here because Task
// Scheduler has no separate armed/unarmed state — a registered ONLOGON task runs
// at the next logon, full stop.
//
// ProgramPath stays EMPTY (unknown) on purpose. /Query /XML would have to be
// parsed back out of a schtasks document whose <Command> we wrote quoted, and
// this is precisely the shape that produced the Cursor-hook false alarm: a naive
// unquote returns a path with doubled separators, os.Stat calls it missing, and
// every healthy Windows machine prints doctor's most alarming line. Silence beats
// a confident wrong answer.
func (windowsManager) Status() (State, error) {
	// #nosec G204 -- constant subcommands + fixed task name. /Query exits
	// non-zero when the task is absent.
	if err := exec.Command("schtasks", "/Query", "/TN", taskName).Run(); err != nil {
		return State{Detail: "not enabled"}, nil
	}
	return State{
		Installed: true,
		Loaded:    true,
		Detail:    "enabled (Task Scheduler, runs at logon)",
	}, nil
}
