package state

import (
	"os"
	"path/filepath"
	"runtime"
)

// SelfBin returns the promptster-teams binary to re-exec for background work:
// `start`'s detached `watch` child and the command autostart registers with the
// platform supervisor.
//
// It resolves the actual running executable rather than the canonical install
// path, because the two diverge on every channel except the curl installer: npm
// runs the binary out of node_modules, `go build` runs it from wherever it
// landed. Naming the canonical path there hands exec.Command — and
// launchd/systemd/Task Scheduler — a file that does not exist, which surfaces as
// `start error: ... no such file or directory` or a supervisor that silently
// never comes up. Symlinks are resolved because the npm global bin is one, and a
// supervisor should point at the real image rather than a link that a later
// install can repoint.
func SelfBin() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			return resolved
		}
		return exe
	}
	return canonicalInstallBin()
}

// canonicalInstallBin is where the curl installer puts the binary
// (~/.promptster-teams/bin/promptster-teams). It is deliberately unexported: it
// is an assumption about the install layout that only holds for one channel, so
// it is sound only as SelfBin's last-resort fallback for a host where
// os.Executable fails. Callers that want "our binary" want SelfBin.
func canonicalInstallBin() string {
	home, _ := os.UserHomeDir()
	name := "promptster-teams"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(home, ".promptster-teams", "bin", name)
}
