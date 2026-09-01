package state

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestInstallationIDConcurrentFirstStartConverges(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)

	const callers = 32
	ids := make(chan string, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- InstallationID()
		}()
	}
	wg.Wait()
	close(ids)

	want := InstallationID()
	for got := range ids {
		if got != want {
			t.Fatalf("concurrent InstallationID() = %q, canonical value is %q", got, want)
		}
	}
}

func TestReadInstallationIDRejectsPartialValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installation-id")
	if err := os.WriteFile(path, []byte("ins-abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readInstallationID(path); got != "" {
		t.Fatalf("partial installation id accepted as %q", got)
	}
}
