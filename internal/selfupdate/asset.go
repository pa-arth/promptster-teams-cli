package selfupdate

import "fmt"

// assetBaseName is the release-asset name prefix, matching npm/scripts/build.js.
const assetBaseName = "promptster-teams"

// assetName maps a Go GOOS/GOARCH pair to the release asset name published by
// npm/scripts/build.js: "promptster-teams-{os}-{arch}" where os is the npm/node
// platform token (linux, darwin, win32) and arch is x64 or arm64; windows adds
// ".exe". It errors on any platform the release pipeline does not build, so the
// updater refuses to guess a filename that won't exist.
func assetName(goos, goarch string) (string, error) {
	var osToken string
	switch goos {
	case "linux":
		osToken = "linux"
	case "darwin":
		osToken = "darwin"
	case "windows":
		osToken = "win32"
	default:
		return "", fmt.Errorf("selfupdate: unsupported OS %q", goos)
	}

	var archToken string
	switch goarch {
	case "amd64":
		archToken = "x64"
	case "arm64":
		archToken = "arm64"
	default:
		return "", fmt.Errorf("selfupdate: unsupported architecture %q", goarch)
	}

	name := fmt.Sprintf("%s-%s-%s", assetBaseName, osToken, archToken)
	if goos == "windows" {
		name += ".exe"
	}
	return name, nil
}
