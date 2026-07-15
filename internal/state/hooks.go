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

// CanonicalInstallBin is the MANAGED install path
// (~/.promptster-teams/bin/promptster-teams[.exe]) — the one location this CLI
// owns and updates in place.
//
// It used to be unexported, on the grounds that the layout "only holds for one
// channel" (the curl installer). That is no longer true: npm's postinstall
// installs here too, and the node_modules copy is only a launcher that execs
// this path. Both managed channels therefore land on the same file, which is
// what keeps npm's metadata honest — self-update rewrites THIS binary and never
// the one npm is tracking.
//
// Still not a substitute for SelfBin: a `go build` or a hand-placed binary runs
// from wherever it landed, and callers that want "the running image" want
// SelfBin. Use this only to ask "is self the managed install?" or as SelfBin's
// last-resort fallback where os.Executable fails.
func CanonicalInstallBin() string { return canonicalInstallBin() }

func canonicalInstallBin() string {
	home, _ := os.UserHomeDir()
	name := "promptster-teams"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(home, ".promptster-teams", "bin", name)
}
