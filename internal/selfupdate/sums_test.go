package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleSums = "d3d92c63b0a7aae9d72fc98410d8f7eb2a5b527fa4c3babd1af022698d69a1b1  promptster-teams-darwin-arm64\n" +
	"ac336521afe9133ddcbbcd73dad41938f46cceffc04050008300e3df482e6121  promptster-teams-linux-x64\n"

func TestExpectedSum(t *testing.T) {
	hex, err := expectedSum([]byte(sampleSums), "promptster-teams-darwin-arm64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hex != "d3d92c63b0a7aae9d72fc98410d8f7eb2a5b527fa4c3babd1af022698d69a1b1" {
		t.Errorf("got %q", hex)
	}
}

func TestExpectedSumMissing(t *testing.T) {
	if _, err := expectedSum([]byte(sampleSums), "promptster-teams-win32-x64.exe"); err == nil {
		t.Error("expected error for asset absent from SHA256SUMS")
	}
}

func TestVerifyFileSum(t *testing.T) {
	dir := t.TempDir()
	// "darwin-binary-content-v1\n" hashes to the darwin line above.
	p := filepath.Join(dir, "bin")
	if err := os.WriteFile(p, []byte("darwin-binary-content-v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyFileSum(p, "d3d92c63b0a7aae9d72fc98410d8f7eb2a5b527fa4c3babd1af022698d69a1b1"); err != nil {
		t.Errorf("valid checksum should verify: %v", err)
	}
	if err := verifyFileSum(p, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Error("mismatched checksum must be rejected")
	}
}
