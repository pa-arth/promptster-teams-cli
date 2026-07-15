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
	min     string
}

func (s stubPolicy) AutoUpdateEnabled() bool  { return s.enabled }
func (s stubPolicy) PinnedCliVersion() string { return s.pinned }
func (s stubPolicy) MinCliVersion() string    { return s.min }

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

	// Longest matching substring wins so "/SHA256SUMS.minisig" beats
	// "/SHA256SUMS". A registered code of 0 means "network error".
	match := func(url string) (resp, bool) {
		best, bestLen := resp{code: 404}, -1
		for sub, r := range routes {
			if strings.Contains(url, sub) && len(sub) > bestLen {
				best, bestLen = r, len(sub)
			}
		}
		return best, best.code != 0
	}

	u = &updater{
		currentVersion: current,
		policy:         pol,
		goos:           "darwin",
		goarch:         "arm64",
		releaseBaseURL: "https://rel.test",
		httpGet: func(url string, limit int64) ([]byte, int, error) {
			best, ok := match(url)
			if !ok {
				return nil, 0, fmt.Errorf("network error: %s", url)
			}
			return []byte(best.body), best.code, nil
		},
		// Redirect routes reuse the same table: body IS the Location header.
		httpRedirect: func(url string) (string, int, error) {
			best, ok := match(url)
			if !ok {
				return "", 0, fmt.Errorf("network error: %s", url)
			}
			return best.body, best.code, nil
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

// latestRoute is the releases/latest redirect route: the substring the stub
// matches, and the Location it answers with.
const latestRoute = "rel.test/pa-arth/promptster-teams-cli/releases/latest"

func locationForTag(tag string) string {
	return "https://rel.test/pa-arth/promptster-teams-cli/releases/tag/" + tag
}

// wireHappyRelease registers a full valid release at tag v9.9.9.
func wireHappyRelease(addRoute func(sub, body string, code int)) {
	addRoute(latestRoute, locationForTag("v9.9.9"), 302)
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
		{"npm global unix", "/usr/local/lib/node_modules/@promptster/teams-cli/binaries/promptster-teams-darwin-arm64", nudgeNpmGlobal},
		{"npm global windows", `C:\Users\e\AppData\Roaming\npm\node_modules\@promptster\teams-cli\binaries\promptster-teams-win32-x64.exe`, nudgeNpmGlobal},
		{"pnpm global", "/home/e/Library/pnpm/global/5/.pnpm/@promptster+teams-cli@0.5.6/node_modules/@promptster/teams-cli/binaries/promptster-teams-linux-x64", nudgePnpmGlobal},
		{"curl installer", "/usr/local/bin/promptster-teams", nudgeCurl},
		{"homebrew", "/opt/homebrew/bin/promptster-teams", nudgeCurl},
		// A directory merely containing the substring must not read as npm.
		{"lookalike dir", "/home/e/my-node_modules-backup/promptster-teams", nudgeCurl},

		// The cases the review caught: a global command against a project-local
		// install updates the global prefix and leaves this copy stale, so these
		// must name the project instead of prescribing `npm i -g`.
		{
			"npm project-local unix",
			"/home/e/proj/node_modules/@promptster/teams-cli/binaries/promptster-teams-linux-x64",
			"promptster-teams: update available — update " + npmPackage + " in /home/e/proj",
		},
		{
			"pnpm project-local store",
			"/home/e/proj/node_modules/.pnpm/@promptster+teams-cli@0.5.6/node_modules/@promptster/teams-cli/binaries/promptster-teams-linux-x64",
			"promptster-teams: update available — update " + npmPackage + " in /home/e/proj",
		},
		{
			"npm project-local windows",
			`C:\Users\e\proj\node_modules\@promptster\teams-cli\binaries\promptster-teams-win32-x64.exe`,
			"promptster-teams: update available — update " + npmPackage + ` in C:\Users\e\proj`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nudgeFor(tc.self); got != tc.want {
				t.Fatalf("nudgeFor(%q) = %q, want %q", tc.self, got, tc.want)
			}
		})
	}
}

// nudgeCurl once pointed at https://get.promptster.ai — the HIRING CLI's
// installer, which drops `promptster` from pa-arth/promptster-cli-releases into
// ~/.promptster/bin and leaves promptster-teams exactly as stale as it was.
//
// Every other nudge test compares against the nudgeCurl CONSTANT, so all of
// them stayed green while it named the wrong product. This one pins the
// CONTENT, which is the only way that class of bug gets caught.
func TestNudgeCurlInstallsThisProduct(t *testing.T) {
	if !strings.Contains(nudgeCurl, repoSlug) {
		t.Fatalf("nudgeCurl does not name %s: %q", repoSlug, nudgeCurl)
	}
	if !strings.Contains(nudgeCurl, "install.sh") {
		t.Fatalf("nudgeCurl does not run this repo's install.sh: %q", nudgeCurl)
	}
	// The hiring CLI's installer, in any form.
	if strings.Contains(nudgeCurl, "get.promptster.ai") ||
		strings.Contains(nudgeCurl, "promptster-cli-releases") {
		t.Fatalf("nudgeCurl points at the hiring CLI installer: %q", nudgeCurl)
	}
}

// A global hint must never be printed for a project-local install: it updates a
// different copy and leaves this one stale, which is the bug the whole
// channel-matching exercise exists to prevent.
func TestLocalInstallNeverGetsAGlobalHint(t *testing.T) {
	locals := []string{
		"/home/e/proj/node_modules/@promptster/teams-cli/binaries/promptster-teams-linux-x64",
		"/home/e/proj/node_modules/.pnpm/@promptster+teams-cli@0.5.6/node_modules/@promptster/teams-cli/binaries/promptster-teams-linux-x64",
		`C:\Users\e\proj\node_modules\@promptster\teams-cli\binaries\promptster-teams-win32-x64.exe`,
	}
	for _, self := range locals {
		got := nudgeFor(self)
		for _, bad := range []string{nudgeNpmGlobal, nudgePnpmGlobal, nudgeCurl} {
			if got == bad {
				t.Fatalf("local install %q got global/curl hint %q", self, got)
			}
		}
		if !strings.Contains(got, "-g ") {
			continue
		}
		t.Fatalf("local install %q got a -g hint: %q", self, got)
	}
}

func TestCheckIntervalEscalatesBelowMinCliVersion(t *testing.T) {
	cases := []struct {
		name    string
		current string
		pol     PolicyView
		want    time.Duration
	}{
		{"no policy", "0.5.2", nil, updateCheckInterval},
		{"no floor set", "0.5.2", stubPolicy{enabled: true}, updateCheckInterval},
		{"below floor escalates", "0.5.2", stubPolicy{enabled: true, min: "0.6.0"}, belowMinCheckInterval},
		{"below floor, v-prefixed", "0.5.2", stubPolicy{enabled: true, min: "v0.6.0"}, belowMinCheckInterval},
		{"at floor", "0.6.0", stubPolicy{enabled: true, min: "0.6.0"}, updateCheckInterval},
		{"above floor", "0.7.0", stubPolicy{enabled: true, min: "0.6.0"}, updateCheckInterval},
		// The floor escalates the CADENCE only. checkAndApply still enforces the
		// org switch and the pin, so a disabled org polls a no-op cheaply rather
		// than being dragged forward against its policy.
		{"below floor but org disabled", "0.5.2", stubPolicy{enabled: false, min: "0.6.0"}, belowMinCheckInterval},
		{"below floor but pinned", "0.5.2", stubPolicy{enabled: true, pinned: "0.5.4", min: "0.6.0"}, belowMinCheckInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &updater{currentVersion: tc.current, policy: tc.pol}
			if got := u.checkInterval(); got != tc.want {
				t.Fatalf("checkInterval() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A floor must not become a licence to install something the normal gates would
// refuse: an org that disabled auto-update stays disabled even while below it.
func TestMinCliVersionDoesNotOverrideDisabledOrg(t *testing.T) {
	u, applied, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: false, min: "9.9.9"})
	wireHappyRelease(addRoute)
	if got := u.checkAndApply(); got != outcomeSkippedPolicy {
		t.Fatalf("disabled org below floor = %v, want skippedPolicy", got)
	}
	if len(*applied) != 0 {
		t.Fatal("a min-version floor must not override the org auto-update switch")
	}
}

// A floor must not drag a pinned fleet past its pin.
func TestMinCliVersionDoesNotOverridePin(t *testing.T) {
	u, applied, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true, pinned: "0.5.4", min: "9.9.9"})
	wireHappyRelease(addRoute)
	addRoute("github.test/pa-arth/promptster-teams-cli/releases/download/v0.5.4/SHA256SUMS", "", 404)
	if got := u.checkAndApply(); got == outcomeApplied {
		t.Fatal("a min-version floor must not override an org pin")
	}
	for _, p := range *applied {
		if strings.Contains(p, "9.9.9") {
			t.Fatalf("installed the floor %q instead of the pin", p)
		}
	}
}

func TestSha256MismatchRejected(t *testing.T) {
	u, applied, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
	addRoute(latestRoute, locationForTag("v9.9.9"), 302)
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
	addRoute(latestRoute, locationForTag("v9.9.9"), 302)
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
	addRoute(latestRoute, "", 0) // network error
	if got := u.checkAndApply(); got != outcomeError {
		t.Fatalf("latest-fetch error outcome = %v, want error", got)
	}
}

// The tag now comes from a redirect target rather than a JSON field, so the
// parse is the new failure surface.
func TestTagFromReleaseLocation(t *testing.T) {
	ok := []struct{ name, loc, want string }{
		{"absolute", "https://github.com/pa-arth/promptster-teams-cli/releases/tag/v0.6.1", "v0.6.1"},
		{"relative", "/pa-arth/promptster-teams-cli/releases/tag/v0.6.1", "v0.6.1"},
		{"trailing slash", "https://github.com/x/y/releases/tag/v1.2.3/", "v1.2.3"},
		{"percent-escaped", "https://github.com/x/y/releases/tag/v1.2.3%2Brc1", "v1.2.3+rc1"},
		{"no v prefix", "https://github.com/x/y/releases/tag/1.2.3", "1.2.3"},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tagFromReleaseLocation(tc.loc)
			if err != nil {
				t.Fatalf("tagFromReleaseLocation(%q) errored: %v", tc.loc, err)
			}
			if got != tc.want {
				t.Fatalf("tagFromReleaseLocation(%q) = %q, want %q", tc.loc, got, tc.want)
			}
		})
	}

	bad := []struct{ name, loc string }{
		{"empty", ""},
		{"no marker", "https://github.com/pa-arth/promptster-teams-cli/releases"},
		{"empty tag", "https://github.com/x/y/releases/tag/"},
		{"login redirect", "https://github.com/login?return_to=%2Fx%2Fy"},
		// The tag is interpolated into the download URL, so a separator that
		// could climb out of /releases/download/<tag>/ must be refused.
		{"traversal escaped", "https://github.com/x/y/releases/tag/..%2F..%2Fevil"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tagFromReleaseLocation(tc.loc); err == nil {
				t.Fatalf("tagFromReleaseLocation(%q) = %q, want error", tc.loc, got)
			}
		})
	}
}

// A repo with no releases answers 200 (the releases index) instead of
// redirecting; a 200 must never be mistaken for a usable tag.
func TestFetchLatestTagRequiresRedirect(t *testing.T) {
	cases := []struct {
		name, body string
		code       int
	}{
		{"200 index page", "<html>no releases</html>", 200},
		{"404", "", 404},
		{"3xx with no Location", "", 302},
		// Isolates the status check from the Location parse: a perfectly
		// well-formed tag target that arrives on a NON-redirect status (a proxy
		// or captive portal answering 200 with a Location header) must still be
		// refused. Without this case the status guard can be deleted and every
		// other case here stays green, because their Locations fail the parse
		// anyway.
		{"200 carrying a valid tag Location", locationForTag("v9.9.9"), 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, applied, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
			// Wire a COMPLETE, valid release at v9.9.9 first, then override only
			// the latest-redirect. Without this the assets would 404 and every
			// case would reach outcomeError via a failed download no matter what
			// fetchLatestTag decided — the failure would mask the thing under
			// test. With it, a tag that wrongly resolves goes on to APPLY, so
			// outcomeError is evidence the resolve itself refused.
			wireHappyRelease(addRoute)
			addRoute(latestRoute, tc.body, tc.code)
			if got := u.checkAndApply(); got != outcomeError {
				t.Fatalf("outcome = %v, want error", got)
			}
			if len(*applied) != 0 {
				t.Fatal("must not apply without a resolved tag")
			}
		})
	}
}

// The whole point of the change: no check may touch api.github.com, whose
// 60/hr per-IP limit is what forced the old 24h cadence.
func TestChecksNeverHitTheGitHubAPI(t *testing.T) {
	u, _, addRoute := buildUpdater(t, "0.5.2", stubPolicy{enabled: true})
	wireHappyRelease(addRoute)

	var seen []string
	wrappedGet, wrappedRedirect := u.httpGet, u.httpRedirect
	u.httpGet = func(url string, limit int64) ([]byte, int, error) {
		seen = append(seen, url)
		return wrappedGet(url, limit)
	}
	u.httpRedirect = func(url string) (string, int, error) {
		seen = append(seen, url)
		return wrappedRedirect(url)
	}
	if got := u.checkAndApply(); got != outcomeApplied {
		t.Fatalf("outcome = %v, want applied", got)
	}
	if len(seen) == 0 {
		t.Fatal("no requests recorded — the test is not exercising the fetch path")
	}
	for _, url := range seen {
		if strings.Contains(url, "api.github.com") || strings.Contains(url, "/repos/") {
			t.Fatalf("check hit the rate-limited JSON API: %q", url)
		}
	}
}
