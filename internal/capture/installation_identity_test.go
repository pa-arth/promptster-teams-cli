package capture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeviceIDIsStablePerInstallationAndDistinctAcrossHomes(t *testing.T) {
	firstState := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", firstState)
	first := DeviceID()
	if again := DeviceID(); again != first {
		t.Fatalf("DeviceID changed within one installation: %q then %q", first, again)
	}

	secondState := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", secondState)
	second := DeviceID()
	if second == first {
		t.Fatalf("independent installations on one machine share device id %q", first)
	}
	if _, err := os.Stat(filepath.Join(secondState, "installation-id")); err != nil {
		t.Fatalf("second installation did not persist its identity: %v", err)
	}
}
