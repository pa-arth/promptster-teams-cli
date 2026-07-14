//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const unitName = "promptster-teams.service"

type linuxManager struct{}

// New returns the systemd --user-backed autostart manager for Linux.
func New() Manager { return linuxManager{} }

// unitPath is ~/.config/systemd/user/promptster-teams.service.
func unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", unitName), nil
}

func (linuxManager) Enable() error {
	p, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(renderUnit(state.SelfBin())), 0o600); err != nil {
		return err
	}
	// #nosec G204 -- constant subcommands, no user input.
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload failed: %v: %s", err, out)
	}
	// #nosec G204 -- constant subcommands + fixed unit name.
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user enable --now failed: %v: %s", err, out)
	}
	return nil
}

// Stop halts the unit without disabling it: the default.target.wants symlink
// stays, so systemd starts it again at the next login. `stop` (unlike `disable
// --now`) also means Restart=on-failure won't revive it — a systemd-initiated
// stop is never a failure, whatever exit status or signal follows.
func (linuxManager) Stop() error {
	if installed, _, _ := (linuxManager{}).Status(); !installed {
		return nil
	}
	// #nosec G204 -- constant subcommands + fixed unit name.
	if out, err := exec.Command("systemctl", "--user", "stop", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user stop failed: %v: %s", err, out)
	}
	return nil
}

func (linuxManager) Disable() error {
	p, err := unitPath()
	if err != nil {
		return err
	}
	// #nosec G204 -- constant subcommands + fixed unit name.
	_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	// #nosec G204 -- constant subcommands.
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func (linuxManager) Status() (bool, string, error) {
	p, err := unitPath()
	if err != nil {
		return false, "", err
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return false, "not enabled", nil
		}
		return false, "", err
	}
	active := "inactive"
	// #nosec G204 -- constant subcommands + fixed unit name. is-active exits
	// non-zero when inactive, so we read stdout regardless of the error.
	if out, _ := exec.Command("systemctl", "--user", "is-active", unitName).Output(); len(out) > 0 {
		active = strings.TrimSpace(string(out))
	}
	return true, fmt.Sprintf("enabled (systemd --user, %s)", active), nil
}
