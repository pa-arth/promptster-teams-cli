package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallationIDPersistsAndIsStateScoped(t *testing.T) {
	firstDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", firstDir)
	first := InstallationID()
	if !strings.HasPrefix(first, "ins-") {
		t.Fatalf("InstallationID() = %q, want opaque ins-* id", first)
	}
	if again := InstallationID(); again != first {
		t.Fatalf("second InstallationID() = %q, want %q", again, first)
	}
	info, err := os.Stat(filepath.Join(firstDir, "installation-id"))
	if err != nil {
		t.Fatalf("installation id was not persisted: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("installation-id mode = %o, want 600", info.Mode().Perm())
	}

	secondDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", secondDir)
	second := InstallationID()
	if second == first {
		t.Fatalf("two state directories share installation id %q", first)
	}
}
