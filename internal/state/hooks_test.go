package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSelfBinExists guards the npm-path trap that broke `start` for every
// non-installer channel: the binary we hand to exec.Command / launchd / systemd
// / Task Scheduler must actually exist. os.Executable() (the running binary)
// always does; the canonical ~/.promptster-teams/bin path does not for an npm or
// `go build` install, and naming it produced
// `start error: ... no such file or directory`.
func TestSelfBinExists(t *testing.T) {
	p := SelfBin()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("SelfBin()=%q must point at an existing executable: %v", p, err)
	}
}

// TestSelfBinIsRunningExecutable pins the property the guard above cannot see on
// a developer machine that happens to have the canonical install present: SelfBin
// must track the RUNNING binary, not the install-layout guess. Under `go test`
// the running binary is the test harness in a temp dir, so any drift back toward
// the hardcoded path fails here rather than only on a user's npm install.
func TestSelfBinIsRunningExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve os.Executable on this host: %v", err)
	}
	want, err := filepath.EvalSymlinks(exe)
	if err != nil {
		want = exe
	}
	if got := SelfBin(); got != want {
		t.Errorf("SelfBin()=%q, want the running executable %q", got, want)
	}
}

// TestCanonicalInstallBinIsInstallerLayout keeps the fallback honest: it is the
// curl installer's path, and SelfBin leans on it only when os.Executable fails.
func TestCanonicalInstallBinIsInstallerLayout(t *testing.T) {
	p := canonicalInstallBin()
	if !strings.Contains(filepath.ToSlash(p), "/.promptster-teams/bin/") {
		t.Errorf("canonicalInstallBin()=%q, want the installer's ~/.promptster-teams/bin layout", p)
	}
}
