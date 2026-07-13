package selfupdate

import "testing"

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "promptster-teams-linux-x64"},
		{"linux", "arm64", "promptster-teams-linux-arm64"},
		{"darwin", "amd64", "promptster-teams-darwin-x64"},
		{"darwin", "arm64", "promptster-teams-darwin-arm64"},
		{"windows", "amd64", "promptster-teams-win32-x64.exe"},
		{"windows", "arm64", "promptster-teams-win32-arm64.exe"},
	}
	for _, c := range cases {
		got, err := assetName(c.goos, c.goarch)
		if err != nil {
			t.Errorf("assetName(%q,%q) unexpected error: %v", c.goos, c.goarch, err)
			continue
		}
		if got != c.want {
			t.Errorf("assetName(%q,%q)=%q want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestAssetNameUnsupported(t *testing.T) {
	if _, err := assetName("plan9", "amd64"); err == nil {
		t.Error("expected error for unsupported OS")
	}
	if _, err := assetName("linux", "riscv64"); err == nil {
		t.Error("expected error for unsupported arch")
	}
}

// TestAssetNameThisHost pins the mapping for the machine this suite runs on
// (darwin/arm64 during development) — a dry manual check that the real download
// URL would resolve.
func TestAssetNameDarwinArm64(t *testing.T) {
	got, err := assetName("darwin", "arm64")
	if err != nil || got != "promptster-teams-darwin-arm64" {
		t.Fatalf("darwin/arm64 asset = %q, err %v; want promptster-teams-darwin-arm64", got, err)
	}
}
