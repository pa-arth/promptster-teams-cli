// Package selfupdate lets the long-running `watch` daemon silently update
// itself from GitHub Releases on the same 24h cadence as the config census, so
// a fleet installed on an old CLI stops missing new capture features (the bug
// that motivated this: config-census never emitted because engineers ran a
// pre-census binary forever).
//
// Trust model: the release pipeline signs SHA256SUMS with a minisign key whose
// PUBLIC half is EMBEDDED in this binary (verify.go). An update is applied only
// after a TWO-step check — (a) the SHA256SUMS.minisig signature verifies against
// the embedded key, then (b) the downloaded binary's sha256 matches its trusted
// line in SHA256SUMS. A bad or missing signature rejects the whole update. The
// swap is atomic (rename over self) and the daemon re-execs in place so capture
// never drops.
//
// Fail posture is deliberately the OPPOSITE of the prose policy: a network blip
// or a rejecting backend must never STRAND the fleet, so anything uncertain is
// best-effort and simply retried next cycle. Org control is opt-OUT (auto-update
// on unless the org disables it) rather than opt-in.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

const (
	// updateCheckInterval is how often a long-running watch re-checks for a
	// newer release — the same 24h cadence as the config census.
	updateCheckInterval = 24 * time.Hour
	// updateCheckPoll is how often the ticker CHECKS whether the interval has
	// elapsed (against the persisted cursor), so the cadence survives laptop
	// sleeps and watch restarts. A poll is a file read and a compare — no
	// network unless an interval actually elapsed — so it is cheap enough to run
	// well below updateCheckInterval. It bounds how fast an org-set
	// minCliVersion can escalate, which is the reason it is minutes not hours.
	updateCheckPoll = 5 * time.Minute
	// belowMinCheckInterval replaces updateCheckInterval while the running
	// version is below the org's minCliVersion floor. It is the RETRY FLOOR for
	// an escalated rollout, not a target: without it a fleet that is below the
	// floor and failing to update (GitHub down, release yanked mid-rollout)
	// would re-hit the releases API every poll and exhaust the 60/hr
	// unauthenticated per-IP limit — starving the very update it is chasing.
	belowMinCheckInterval = 15 * time.Minute

	// repoSlug is the public releases repo.
	repoSlug = "pa-arth/promptster-teams-cli"

	// envNoAutoUpdate opts a single machine out of self-update regardless of org
	// policy. Any truthy value disables it.
	envNoAutoUpdate = "PROMPTSTER_TEAMS_NO_AUTO_UPDATE"

	// maxSumsBytes / maxSigBytes cap the trusted-metadata downloads. Both files
	// are tiny; the caps bound memory against a hostile response.
	maxSumsBytes = 1 << 20  // 1 MiB (SHA256SUMS is a handful of lines)
	maxSigBytes  = 64 << 10 // 64 KiB (.minisig is ~200 bytes)
	// maxBinaryBytes caps the binary download (well above any real CLI size).
	maxBinaryBytes = 256 << 20 // 256 MiB
)

// PolicyView is the slice of the org policy resolver the updater reads. Kept as
// an interface so selfupdate does not import capture and tests can substitute a
// stub without a live resolver.
type PolicyView interface {
	// AutoUpdateEnabled reports whether the org allows self-update. Defaults to
	// true when unknown (fail-OPEN — a network blip must not strand the fleet).
	AutoUpdateEnabled() bool
	// PinnedCliVersion returns an exact tag the org pins the fleet to, or "".
	PinnedCliVersion() string
	// MinCliVersion returns the version floor the org wants the fleet on, or "".
	// It is an escalation lever for the CHECK CADENCE only — it never overrides
	// AutoUpdateEnabled or a pin, and it never changes which tag is installed.
	MinCliVersion() string
}

// npmPackage is the published npm package name.
const npmPackage = "@promptster/teams-cli"

// The one-line hints printed when an update exists but the install dir is not
// writable (e.g. a root-owned install run as a normal user).
//
// The hint MUST name an action that updates THE COPY THAT PRINTED IT. Any hint
// that installs somewhere else drops a SECOND binary in a different PATH entry,
// leaves a coin flip over which one runs, and leaves the stale copy — the exact
// failure it was supposed to fix. That rules out one global message: telling a
// curl-installed engineer to use npm is wrong, and so is telling a
// project-local or pnpm install to `npm i -g`.
const (
	nudgeCurl       = "promptster-teams: update available — run: curl -fsSL https://get.promptster.ai | sh"
	nudgeNpmGlobal  = "promptster-teams: update available — run: npm i -g " + npmPackage + "@latest"
	nudgePnpmGlobal = "promptster-teams: update available — run: pnpm add -g " + npmPackage + "@latest"
)

// pathSegments splits a path on BOTH separators rather than using
// filepath.ToSlash, which rewrites "\" only when GOOS=windows — that would make
// every check here host-dependent and leave the Windows layouts untestable from
// a unix CI runner. A unix directory whose name literally contains a backslash
// could false-positive; that costs one wrong hint line and nothing else, which
// is well worth host-independent behavior.
func pathSegments(p string) []string {
	return strings.FieldsFunc(p, func(c rune) bool { return c == '/' || c == '\\' })
}

func hasSegment(segs []string, want string) bool {
	for _, s := range segs {
		if s == want {
			return true
		}
	}
	return false
}

// hasAdjacent reports whether segs contains a immediately followed by b.
func hasAdjacent(segs []string, a, b string) bool {
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == a && segs[i+1] == b {
			return true
		}
	}
	return false
}

// nodeProjectRoot returns the directory containing the OUTERMOST node_modules
// segment of self — the project (or global prefix) the package was installed
// into — or "" when self is not under a node_modules tree.
//
// Scans the raw string rather than filepath.Dir-walking for the same
// host-independence reason as pathSegments.
func nodeProjectRoot(self string) string {
	for i := 0; i < len(self); i++ {
		if self[i] != '/' && self[i] != '\\' {
			continue
		}
		rest := self[i+1:]
		if !strings.HasPrefix(rest, "node_modules") {
			continue
		}
		after := rest[len("node_modules"):]
		if after == "" || after[0] == '/' || after[0] == '\\' {
			return self[:i]
		}
	}
	return ""
}

// nudgeFor picks the hint that updates the running binary in place.
//
// Global-vs-local matters more than npm-vs-pnpm: `npm i -g` against a
// project-local install updates the global prefix and leaves the local copy
// exactly as stale as it was. Only the documented global layouts get a copyable
// command; anything else names the package and the directory and lets the
// engineer use whatever package manager owns it, because the path alone cannot
// tell npm from yarn and a guess there is the same second-install bug again.
func nudgeFor(self string) string {
	segs := pathSegments(self)
	if !hasSegment(segs, "node_modules") {
		return nudgeCurl
	}
	pnpm := hasSegment(segs, ".pnpm") || hasSegment(segs, "pnpm")
	switch {
	// pnpm's global prefix, e.g. ~/Library/pnpm/global/5/node_modules/...
	case pnpm && hasSegment(segs, "global"):
		return nudgePnpmGlobal
	// npm's global prefix: <prefix>/lib/node_modules (unix) or
	// <AppData>\npm\node_modules (windows).
	case !pnpm && (hasAdjacent(segs, "lib", "node_modules") || hasAdjacent(segs, "npm", "node_modules")):
		return nudgeNpmGlobal
	}
	root := nodeProjectRoot(self)
	if root == "" {
		// Under node_modules but with no resolvable root (relative path). Say
		// nothing prescriptive rather than risk sending them to the wrong copy.
		return "promptster-teams: update available — update " + npmPackage + " in this project"
	}
	return "promptster-teams: update available — update " + npmPackage + " in " + root
}

// outcome is the result of one checkAndApply, used to drive the startup banner
// and to make the gate logic assertable in tests.
type outcome int

const (
	outcomeSkippedDev         outcome = iota // dev/empty build — never self-updates
	outcomeSkippedFlag                       // --no-auto-update or env opt-out
	outcomeSkippedPolicy                     // org policy disabled auto-update
	outcomeUpToDate                          // no newer (or pinned-not-newer) release
	outcomeBlockedNotWritable                // newer release found, install dir read-only
	outcomeError                             // best-effort failure (network/verify/io)
	outcomeApplied                           // swapped + re-exec'd (does not return in prod)
)

// updater carries everything one check needs. Impure edges (HTTP, self-path,
// swap+reexec, clock) are fields so tests exercise the gate/verify logic with
// no network and no process replacement.
type updater struct {
	currentVersion string
	noAutoUpdate   bool
	policy         PolicyView

	goos, goarch string

	// apiBaseURL is the GitHub API base (default https://api.github.com),
	// overridable in tests. releaseBaseURL is the release-download base
	// (default https://github.com), overridable in tests.
	apiBaseURL     string
	releaseBaseURL string

	// httpGet fetches a URL and returns (body, statusCode, err), bounded to
	// limit bytes. Injected so tests serve fixtures.
	httpGet func(url string, limit int64) ([]byte, int, error)
	// resolveSelf returns the absolute, symlink-resolved path of the running
	// binary. Injected so tests point at a temp file.
	resolveSelf func() (string, error)
	// apply performs the swap + re-exec. Injected so tests record the call
	// instead of replacing the process.
	apply func(self, staged string) error

	logf func(format string, args ...any)
	now  func() time.Time
}

// newDefaultUpdater wires the production edges.
func newDefaultUpdater(currentVersion string, noAutoUpdate bool, pol PolicyView) *updater {
	return &updater{
		currentVersion: currentVersion,
		noAutoUpdate:   noAutoUpdate,
		policy:         pol,
		goos:           runtime.GOOS,
		goarch:         runtime.GOARCH,
		apiBaseURL:     "https://api.github.com",
		releaseBaseURL: "https://github.com",
		httpGet:        httpGetLimited,
		resolveSelf:    resolveSelfPath,
		apply:          applySwapAndReexec,
		logf:           state.HookDebugf,
		now:            time.Now,
	}
}

// checkAndApply runs one full gate → fetch → verify → swap cycle. It is
// best-effort throughout: it never panics and returns an outcome the caller
// logs; the cursor is advanced by the caller regardless of outcome so a broken
// release is retried at most once per interval.
func (u *updater) checkAndApply() outcome {
	// 1. Gate. A dev/empty build has no release to compare against.
	if u.currentVersion == "" || u.currentVersion == "dev" {
		return outcomeSkippedDev
	}
	if u.noAutoUpdate || envTruthy(os.Getenv(envNoAutoUpdate)) {
		return outcomeSkippedFlag
	}
	if u.policy != nil && !u.policy.AutoUpdateEnabled() {
		return outcomeSkippedPolicy
	}

	// 2. Target version: an org pin (exact tag) wins over "latest". The pin
	// still passes through the strictly-newer gate below, so it can only move
	// the fleet FORWARD — never downgrade.
	var tag string
	if u.policy != nil && u.policy.PinnedCliVersion() != "" {
		tag = ensureVPrefix(u.policy.PinnedCliVersion())
	} else {
		latest, err := u.fetchLatestTag()
		if err != nil {
			u.logf("selfupdate: could not fetch latest release: %v", err)
			return outcomeError
		}
		tag = latest
	}
	target := strings.TrimPrefix(tag, "v")

	// 3. Only strictly-newer targets proceed.
	if !isNewer(u.currentVersion, target) {
		return outcomeUpToDate
	}

	// 4. Locate self and confirm the install dir is writable.
	self, err := u.resolveSelf()
	if err != nil {
		u.logf("selfupdate: could not resolve own path: %v", err)
		return outcomeError
	}
	dir := filepath.Dir(self)
	if !dirWritable(dir) {
		u.logf("selfupdate: %s not writable — skipping swap to %s", dir, target)
		fmt.Fprintln(os.Stderr, nudgeFor(self))
		return outcomeBlockedNotWritable
	}

	// 5. Download + verify (minisign THEN sha256) from the SAME tag.
	asset, err := assetName(u.goos, u.goarch)
	if err != nil {
		u.logf("selfupdate: %v", err)
		return outcomeError
	}
	staged, err := u.downloadAndVerify(tag, asset, dir)
	if err != nil {
		u.logf("selfupdate: rejected update to %s: %v", target, err)
		return outcomeError
	}

	// 6. Swap + re-exec. On success this does not return.
	if err := u.apply(self, staged); err != nil {
		_ = os.Remove(staged)
		u.logf("selfupdate: apply failed: %v", err)
		return outcomeError
	}
	return outcomeApplied
}

// downloadAndVerify fetches SHA256SUMS, its .minisig, and the platform asset for
// the given tag into temp files in dir, verifies the signature over SHA256SUMS
// against the embedded key, then verifies the asset's sha256 against the trusted
// SHA256SUMS line. It returns the staged binary path on success; on ANY failure
// it removes partial temp files and returns an error (the whole update is
// rejected). Order matters: the signature is checked BEFORE the checksums are
// trusted.
func (u *updater) downloadAndVerify(tag, asset, dir string) (string, error) {
	base := strings.TrimRight(u.releaseBaseURL, "/") + "/" + repoSlug + "/releases/download/" + tag

	sums, code, err := u.httpGet(base+"/SHA256SUMS", maxSumsBytes)
	if err != nil || code != http.StatusOK {
		return "", httpErr("SHA256SUMS", code, err)
	}
	sig, code, err := u.httpGet(base+"/SHA256SUMS.minisig", maxSigBytes)
	if err != nil || code != http.StatusOK {
		return "", httpErr("SHA256SUMS.minisig", code, err)
	}

	// (a) minisign signature over SHA256SUMS — the trust gate. Reject if unsigned.
	if len(sig) == 0 {
		return "", fmt.Errorf("SHA256SUMS.minisig is empty (unsigned release)")
	}
	if err := verifyMinisign(sums, sig); err != nil {
		return "", fmt.Errorf("minisign verify: %w", err)
	}

	// (b) fetch the binary and check its sha256 against the now-trusted sums.
	wantHex, err := expectedSum(sums, asset)
	if err != nil {
		return "", err
	}
	bin, code, err := u.httpGet(base+"/"+asset, maxBinaryBytes)
	if err != nil || code != http.StatusOK {
		return "", httpErr(asset, code, err)
	}

	staged, err := os.CreateTemp(dir, ".promptster-teams-update-*")
	if err != nil {
		return "", fmt.Errorf("stage temp file: %w", err)
	}
	stagedName := staged.Name()
	if _, err := staged.Write(bin); err != nil {
		_ = staged.Close()
		_ = os.Remove(stagedName)
		return "", fmt.Errorf("write staged binary: %w", err)
	}
	if err := staged.Close(); err != nil {
		_ = os.Remove(stagedName)
		return "", fmt.Errorf("close staged binary: %w", err)
	}
	if err := verifyFileSum(stagedName, wantHex); err != nil {
		_ = os.Remove(stagedName)
		return "", err
	}
	return stagedName, nil
}

// httpErr formats a download failure without misusing %w on a nil error (a
// non-200 with err==nil would otherwise print "%!w(<nil>)").
func httpErr(what string, code int, err error) error {
	if err != nil {
		return fmt.Errorf("download %s: %w", what, err)
	}
	return fmt.Errorf("download %s: status %d", what, code)
}

// fetchLatestTag reads the latest release's tag_name from the GitHub API.
func (u *updater) fetchLatestTag() (string, error) {
	url := strings.TrimRight(u.apiBaseURL, "/") + "/repos/" + repoSlug + "/releases/latest"
	body, code, err := u.httpGet(url, maxSumsBytes)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("GitHub API status %d", code)
	}
	var parsed struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if strings.TrimSpace(parsed.TagName) == "" {
		return "", fmt.Errorf("latest release has no tag_name")
	}
	return parsed.TagName, nil
}

// --- production edges --------------------------------------------------------

// httpGetLimited GETs url with the shared CLI client (carries the version
// header) and returns up to limit bytes of the body plus the status code.
func httpGetLimited(url string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	// `*/*` is the only Accept that works for BOTH endpoints this helper hits:
	// the GitHub JSON API (`/repos/.../releases/latest`) 415s on
	// `application/octet-stream`, while the raw release-download URLs
	// (`/releases/download/...`) serve the file for any Accept. Don't narrow this.
	req.Header.Set("Accept", "*/*")
	resp, err := ingest.HTTPClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// resolveSelfPath returns the running binary's absolute, symlink-resolved path.
func resolveSelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// dirWritable reports whether dir accepts a new file, by creating and removing a
// probe temp file — the only reliable cross-platform writability test (a stat of
// mode bits lies under root, ACLs, and read-only mounts).
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".promptster-teams-wtest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// envTruthy reports whether an env var value means "on".
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ensureVPrefix normalizes a tag to the "vX.Y.Z" form GitHub uses for release
// download paths.
func ensureVPrefix(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return tag
	}
	if strings.HasPrefix(tag, "v") {
		return tag
	}
	return "v" + tag
}

// --- cursor + runner ---------------------------------------------------------

// lastUpdateCheckPath persists when the last update check ran so restarts and
// hourly ticks don't re-check inside the 24h window (startup always checks; the
// cursor only paces the ticker). Mirrors census's last-census-at cursor.
func lastUpdateCheckPath() string {
	return filepath.Join(state.GlobalPromptsterDir(), "last-update-check")
}

func loadLastUpdateCheck() time.Time {
	raw, err := os.ReadFile(lastUpdateCheckPath())
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}
	}
	return t
}

func saveLastUpdateCheck(t time.Time) {
	p := lastUpdateCheckPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(t.UTC().Format(time.RFC3339)), 0o600)
}

// checkInterval is how long this watcher waits between checks: the normal 24h
// cadence, or the shorter escalated floor while the running version is below the
// org's minCliVersion.
//
// The floor only moves the CADENCE. Whether an update is allowed at all, and
// which tag it targets, stay entirely with checkAndApply — so an org that
// disabled auto-update or pinned a tag is unaffected by a floor, and a floor can
// never drag a fleet to a version the newer-only gate would reject.
func (u *updater) checkInterval() time.Duration {
	if u.policy == nil {
		return updateCheckInterval
	}
	min := strings.TrimPrefix(u.policy.MinCliVersion(), "v")
	if min == "" || !isNewer(u.currentVersion, min) {
		return updateCheckInterval
	}
	return belowMinCheckInterval
}

// runAutoUpdate checks once immediately, prints a one-line banner if a newer
// release was found but not applied, then re-checks whenever the current
// interval has elapsed since the persisted cursor, until stop is closed. The
// cursor advances after every check (applied never returns) so a broken release
// retries at most once per interval.
//
// The interval is re-read every poll rather than captured once, so an org
// raising minCliVersion mid-run escalates a watcher that is already up — which
// is the entire point of the lever.
func runAutoUpdate(u *updater, stop <-chan struct{}) {
	res := u.checkAndApply()
	saveLastUpdateCheck(u.now())
	startupBanner(res)

	ticker := time.NewTicker(updateCheckPoll)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if u.now().Sub(loadLastUpdateCheck()) >= u.checkInterval() {
				_ = u.checkAndApply()
				saveLastUpdateCheck(u.now())
			}
		}
	}
}

// startupBanner prints ONE concise line at watch start when an update exists but
// could not be applied, so an operator sees why the fleet is stuck on an old
// version. The not-writable nudge is already printed inside checkAndApply.
func startupBanner(res outcome) {
	switch res {
	case outcomeSkippedPolicy:
		fmt.Fprintln(os.Stderr, "promptster-teams: auto-update disabled by org policy")
	case outcomeBlockedNotWritable:
		// nudge already printed in checkAndApply — no second line.
	}
}

// StartAutoUpdate launches the auto-update goroutine and returns a stop function
// the caller defers. Mirrors capture.StartConfigCensus. noAutoUpdate comes from
// the `--no-auto-update` watch flag; pol is the org policy resolver (may be nil,
// in which case auto-update defaults on).
func StartAutoUpdate(noAutoUpdate bool, pol PolicyView) (stop func()) {
	u := newDefaultUpdater(version.Version, noAutoUpdate, pol)
	done := make(chan struct{})
	go runAutoUpdate(u, done)
	return func() { close(done) }
}

// LatestVersionBestEffort fetches the latest release tag (stripped of "v") with
// a short timeout, for read-only display in `doctor`. It degrades silently:
// ok=false on any error, never blocking the command meaningfully.
func LatestVersionBestEffort(timeout time.Duration) (string, bool) {
	url := "https://api.github.com/repos/" + repoSlug + "/releases/latest"
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var parsed struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSumsBytes)).Decode(&parsed); err != nil {
		return "", false
	}
	tag := strings.TrimSpace(parsed.TagName)
	if tag == "" {
		return "", false
	}
	return strings.TrimPrefix(tag, "v"), true
}
