package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

// ManualStatus is what a foreground `update` run found and did.
type ManualStatus struct {
	Current string
	// Target is the version that would be (or was) installed. Empty when the
	// check could not resolve one.
	Target string
	// UpToDate is true when no newer release exists. Target is then the current
	// version, not a newer one.
	UpToDate bool
	// Applied is true when a new binary is on disk. The caller is responsible
	// for restarting capture — this function deliberately does not re-exec.
	Applied bool
	// Blocked, when non-empty, explains why an available update was not
	// installed (org policy, a lockfile-pinned copy, an unwritable directory).
	// It is a sentence for a human, not a code to branch on.
	Blocked string
	// Pinned is true when the target came from the org's pin rather than from
	// the newest release, so the caller can say so rather than implying the
	// engineer is now on the latest build.
	Pinned bool
}

// CheckManual resolves what an update would do, WITHOUT downloading or
// installing anything. It is the read-only half of the `update` command, so the
// command can show an engineer the version and the release notes before asking
// them to agree to it.
//
// Org policy is enforced here, not just in the daemon. A manual `update` is an
// explicit instruction from the engineer, which is why it bypasses the local
// CONSENT record — but consent and policy are different things. Consent asks
// whether this engineer agreed to background updates on their own machine; the
// org's switch and pin decide which builds are allowed on the company's
// machines at all, and an engineer typing a command is not an authority that
// overrides their security team.
func CheckManual(pol PolicyView) (ManualStatus, error) {
	st := ManualStatus{Current: version.Version}
	u := newDefaultUpdater(version.Version, false, pol)

	if u.currentVersion == "" || u.currentVersion == "dev" {
		return st, fmt.Errorf("this is a %q build with no release to compare against", u.currentVersion)
	}
	if pol != nil && !pol.AutoUpdateEnabled() && pol.PinnedCliVersion() == "" {
		st.Blocked = "your organization has disabled CLI updates"
		return st, nil
	}

	var tag string
	if pol != nil && pol.PinnedCliVersion() != "" {
		tag = ensureVPrefix(pol.PinnedCliVersion())
		st.Pinned = true
	} else {
		latest, err := u.fetchLatestTag()
		if err != nil {
			return st, fmt.Errorf("could not reach GitHub releases: %w", err)
		}
		tag = latest
	}
	st.Target = strings.TrimPrefix(tag, "v")

	if !IsNewer(u.currentVersion, st.Target) {
		st.UpToDate = true
		return st, nil
	}

	self, err := u.resolveSelf()
	if err != nil {
		return st, fmt.Errorf("could not resolve this binary's path: %w", err)
	}
	if isProjectLocalInstall(self) {
		st.Blocked = "this copy is pinned by a project lockfile — " + nudgeFor(self, state.CanonicalInstallBin())
		return st, nil
	}
	if !dirWritable(filepath.Dir(self)) {
		st.Blocked = nudgeFor(self, state.CanonicalInstallBin())
		return st, nil
	}
	return st, nil
}

// ApplyManual downloads, verifies and installs the target resolved by
// CheckManual. It does NOT re-exec and does NOT restart capture: the caller
// decides when to bounce the daemon, because the daemon is a different process
// from the one running this code and stopping it is a visible action that
// belongs in the command, not buried in the updater.
//
// Verification is identical to the daemon's path — minisign over SHA256SUMS,
// then sha256 over the asset — because "the engineer asked for it" is not a
// reason to install unverified bytes. A manual update relaxes consent, never
// trust.
func ApplyManual(pol PolicyView, target string) error {
	u := newDefaultUpdater(version.Version, false, pol)

	self, err := u.resolveSelf()
	if err != nil {
		return fmt.Errorf("could not resolve this binary's path: %w", err)
	}
	asset, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	dir := filepath.Dir(self)
	staged, err := u.downloadAndVerify(ensureVPrefix(target), asset, dir)
	if err != nil {
		return err
	}
	if err := swapInPlace(self, staged); err != nil {
		_ = os.Remove(staged)
		return err
	}
	return nil
}
