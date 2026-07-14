package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubPolicy implements PolicyView for gate tests.
type stubPolicy struct {
	enabled bool
	pinned  string
}

func (s stubPolicy) AutoUpdateEnabled() bool  { return s.enabled }
func (s stubPolicy) PinnedCliVersion() string { return s.pinned }

// buildUpdater is the primary test constructor: it returns the updater plus a
// pointer to the applied-staged-paths slice and a route registration func.
func buildUpdater(t *testing.T, current string, pol PolicyView) (u *updater, applied *[]string, addRoute func(sub, body string, code int)) {
	t.Helper()
	t.Setenv(envNoAutoUpdate, "")

	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "promptster-teams")
	if err := os.WriteFile(self, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}

	type resp struct {
		body string
		code int
	}
	routes := map[string]resp{}
	var app []string

	u = &updater{
		currentVersion: current,
		policy:         pol,
		goos:           "darwin",
		goarch:         "arm64",
		apiBaseURL:     "https://api.test",
		releaseBaseURL: "https://rel.test",
		httpGet: func(url string, limit int64) ([]byte, int, error) {
			// Longest matching substring wins so "/SHA256SUMS.minisig" beats
			// "/SHA256SUMS".
			best, bestLen := resp{code: 404}, -1
			for sub, r := range routes {
				if strings.Contains(url, sub) && len(sub) > bestLen {
					best, bestLen = r, len(sub)
				}
			}
			if best.code == 0 {
				return nil, 0, fmt.Errorf("network error: %s", url)
			}
			return []byte(best.body), best.code, nil
		},
		resolveSelf: func() (string, error) { return self, nil },
		apply: func(selfPath, staged string) error {
			app = append(app, staged)
			return nil
		},
		logf: func(string, ...any) {},
		now:  time.Now,
	}
	addRoute = func(sub, body string, code int) { routes[sub] = resp{body, code} }
	return u, &app, addRoute
}

// wireHappyRelease registers a full valid release at tag v9.9.9.
func wireHappyRelease(addRoute func(sub, body string, code int)) {
	addRoute("api.test/repos/pa-arth/promptster-teams-cli/releases/latest", `{"tag_name":"v9.9.9"}`, 200)
	addRoute("releases/download/v9.9.9/SHA256SUMS.minisig", sampleSig, 200)
	addRoute("releases/download/v9.9.9/SHA256SUMS", sampleSums, 200)
	addRoute("releases/download/v9.9.9/promptster-teams-darwin-arm64", "darwin-binary-content-v1\n", 200)
}

func TestGateDevBuildNeverUpdates(t *testing.T) {
	u, applied, _ := buildUpdater(t, "dev", stubPolicy{enabled: true})
	if got := u.checkAndApply(); got != outcomeSkippedDev {
		t.Fatalf("dev build outcome = %v, want skippedDev", got)
	}
	if len(*applied) != 0 {
		t.Fatal("dev build must not apply")
	}
}

func TestGateEmptyVersion(t *testing.T) {
	u, _, _ := buildUpdater(t, "", stubPolicy{enabled: true})
	if got := u.checkAndApply(); got != outcomeSkippedDev {
		t.Fatalf("empty version outcome = %v, want skippedDev", got)
	}
}

func TestGateNoAutoUpdateFlag(t *testing.T) {
	u, applied, _ := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
	u.noAutoUpdate = true
	if got := u.checkAndApply(); got != outcomeSkippedFlag {
		t.Fatalf("flag outcome = %v, want skippedFlag", got)
	}
	if len(*applied) != 0 {
		t.Fatal("flag opt-out must not apply")
	}
}

func TestGateEnvOptOut(t *testing.T) {
	u, _, _ := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
	t.Setenv(envNoAutoUpdate, "1")
	if got := u.checkAndApply(); got != outcomeSkippedFlag {
		t.Fatalf("env opt-out outcome = %v, want skippedFlag", got)
	}
}

func TestGatePolicyDisabled(t *testing.T) {
	u, applied, _ := buildUpdater(t, "0.5.2", stubPolicy{enabled: false})
	if got := u.checkAndApply(); got != outcomeSkippedPolicy {
		t.Fatalf("policy-disabled outcome = %v, want skippedPolicy", got)
	}
	if len(*applied) != 0 {
		t.Fatal("policy-disabled must not apply")
	}
}

func TestUpToDate(t *testing.T) {
	u, applied, addRoute := buildUpdater(t, "9.9.9", stubPolicy{enabled: true})
	wireHappyRelease(addRoute)
	if got := u.checkAndApply(); got != outcomeUpToDate {
		t.Fatalf("same-version outcome = %v, want upToDate", got)
	}
	if len(*applied) != 0 {
		t.Fatal("up-to-date must not apply")
	}
}

func TestHappyPathApplies(t *testing.T) {
	u, applied, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
	wireHappyRelease(addRoute)
	if got := u.checkAndApply(); got != outcomeApplied {
		t.Fatalf("happy path outcome = %v, want applied", got)
	}
	if len(*applied) != 1 {
		t.Fatalf("apply called %d times, want 1", len(*applied))
	}
	// The staged file must exist and carry the verified new binary content.
	staged := (*applied)[0]
	data, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("staged binary unreadable: %v", err)
	}
	if string(data) != "darwin-binary-content-v1\n" {
		t.Fatalf("staged content = %q", string(data))
	}
}

func TestPinnedTargetsThatTag(t *testing.T) {
	// Pinned to v9.9.9; latest endpoint is NOT wired, proving the pin is used
	// instead of "latest".
	u, applied, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true, pinned: "9.9.9"})
	addRoute("releases/download/v9.9.9/SHA256SUMS.minisig", sampleSig, 200)
	addRoute("releases/download/v9.9.9/SHA256SUMS", sampleSums, 200)
	addRoute("releases/download/v9.9.9/promptster-teams-darwin-arm64", "darwin-binary-content-v1\n", 200)
	if got := u.checkAndApply(); got != outcomeApplied {
		t.Fatalf("pinned outcome = %v, want applied", got)
	}
	if len(*applied) != 1 {
		t.Fatal("pinned newer must apply")
	}
}

func TestPinnedOlderDoesNotDowngrade(t *testing.T) {
	// Current newer than the pin → strictly-newer gate blocks a downgrade.
	u, applied, addRoute := buildUpdater(t, "9.9.9", stubPolicy{enabled: true, pinned: "1.0.0"})
	wireHappyRelease(addRoute)
	if got := u.checkAndApply(); got != outcomeUpToDate {
		t.Fatalf("pinned-older outcome = %v, want upToDate (no downgrade)", got)
	}
	if len(*applied) != 0 {
		t.Fatal("must never downgrade to an older pin")
	}
}

func TestNonWritableDirNudges(t *testing.T) {
	u, applied, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
	wireHappyRelease(addRoute)
	// Point self at a nonexistent directory so dirWritable fails.
	u.resolveSelf = func() (string, error) {
		return filepath.Join(t.TempDir(), "no-such-dir", "promptster-teams"), nil
	}
	if got := u.checkAndApply(); got != outcomeBlockedNotWritable {
		t.Fatalf("non-writable outcome = %v, want blockedNotWritable", got)
	}
	if len(*applied) != 0 {
		t.Fatal("non-writable dir must not swap")
	}
}

func TestNudgeMatchesInstallChannel(t *testing.T) {
	cases := []struct {
		name string
		self string
		want string
	}{
		{"npm global unix", "/usr/local/lib/node_modules/@promptster/teams-cli/binaries/promptster-teams-darwin-arm64", nudgeNpm},
		{"npm local unix", "/home/e/proj/node_modules/@promptster/teams-cli/binaries/promptster-teams-linux-x64", nudgeNpm},
		{"pnpm store", "/home/e/proj/node_modules/.pnpm/@promptster+teams-cli@0.5.6/node_modules/@promptster/teams-cli/binaries/promptster-teams-linux-x64", nudgeNpm},
		{"npm global windows", `C:\Users\e\AppData\Roaming\npm\node_modules\@promptster\teams-cli\binaries\promptster-teams-win32-x64.exe`, nudgeNpm},
		{"curl installer", "/usr/local/bin/promptster-teams", nudgeCurl},
		{"homebrew", "/opt/homebrew/bin/promptster-teams", nudgeCurl},
		// A directory merely containing the substring must not read as npm.
		{"lookalike dir", "/home/e/my-node_modules-backup/promptster-teams", nudgeCurl},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nudgeFor(tc.self); got != tc.want {
				t.Fatalf("nudgeFor(%q) = %q, want %q", tc.self, got, tc.want)
			}
		})
	}
}

func TestSha256MismatchRejected(t *testing.T) {
	u, applied, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
	addRoute("api.test/repos/pa-arth/promptster-teams-cli/releases/latest", `{"tag_name":"v9.9.9"}`, 200)
	addRoute("releases/download/v9.9.9/SHA256SUMS.minisig", sampleSig, 200)
	addRoute("releases/download/v9.9.9/SHA256SUMS", sampleSums, 200)
	// Serve a binary whose bytes do NOT match the signed checksum.
	addRoute("releases/download/v9.9.9/promptster-teams-darwin-arm64", "TAMPERED", 200)
	if got := u.checkAndApply(); got != outcomeError {
		t.Fatalf("sha mismatch outcome = %v, want error", got)
	}
	if len(*applied) != 0 {
		t.Fatal("checksum mismatch must not apply")
	}
}

func TestBadSignatureRejected(t *testing.T) {
	u, applied, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
	addRoute("api.test/repos/pa-arth/promptster-teams-cli/releases/latest", `{"tag_name":"v9.9.9"}`, 200)
	// A validly-formatted but wrong signature (SHA256SUMS content changed after
	// signing) must reject the whole update.
	addRoute("releases/download/v9.9.9/SHA256SUMS.minisig", sampleSig, 200)
	addRoute("releases/download/v9.9.9/SHA256SUMS", strings.Replace(sampleSums, "d3d9", "0000", 1), 200)
	addRoute("releases/download/v9.9.9/promptster-teams-darwin-arm64", "darwin-binary-content-v1\n", 200)
	if got := u.checkAndApply(); got != outcomeError {
		t.Fatalf("bad signature outcome = %v, want error", got)
	}
	if len(*applied) != 0 {
		t.Fatal("failed signature must not apply")
	}
}

func TestFetchLatestError(t *testing.T) {
	u, _, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
	addRoute("api.test/repos/pa-arth/promptster-teams-cli/releases/latest", "", 0) // network error
	if got := u.checkAndApply(); got != outcomeError {
		t.Fatalf("latest-fetch error outcome = %v, want error", got)
	}
}
