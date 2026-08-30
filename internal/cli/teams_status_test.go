package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/selfupdate"
)

func TestHumanInterval(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Minute, "30m"},
		{5 * time.Minute, "5m"},
		{24 * time.Hour, "24h"},
		{time.Hour, "1h"},
		{90 * time.Minute, "90m"},
		// No whole-unit rendering exists for these, so Duration's own form wins
		// rather than a rounding rule that would misreport the real cadence.
		{90 * time.Second, "1m30s"},
		{45 * time.Second, "45s"},
	}
	for _, c := range cases {
		if got := humanInterval(c.in); got != c.want {
			t.Errorf("humanInterval(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Doctor's auto-update line is the one place an engineer looks when they already
// suspect auto-update is broken, so a wrong branch here actively misleads the
// person debugging. Each case below pins a claim the previous implementation got
// wrong by restating a fact selfupdate owns.

func TestAutoUpdateStatusLineReportsNewerRelease(t *testing.T) {
	got := autoUpdateStatusLine("", "0.11.0", "0.12.1", true)

	if !strings.Contains(got, "newer release available (0.12.1)") {
		t.Fatalf("expected the newer release to be named, got %q", got)
	}
	// The cadence must be derived from the constant, not restated. This is the
	// assertion that fails if someone types a number into the copy again.
	if !strings.Contains(got, humanInterval(selfupdate.CheckInterval)) {
		t.Fatalf("expected the real cadence %s in %q", humanInterval(selfupdate.CheckInterval), got)
	}
	if strings.Contains(got, "24h") {
		t.Fatalf("line still claims the retired 24h cadence: %q", got)
	}
}

// The bug: a machine AHEAD of the published release (a local build, or a release
// yanked after it shipped) was told a "newer release" was available, because the
// check was a string comparison rather than a version comparison. The updater
// itself would never act on that — IsNewer is strict — so doctor was
// contradicting the daemon it reports on.
func TestAutoUpdateStatusLineAheadOfLatestIsUpToDate(t *testing.T) {
	got := autoUpdateStatusLine("", "0.13.0", "0.12.1", true)

	if strings.Contains(got, "newer release available") {
		t.Fatalf("running ahead of latest must not claim an update: %q", got)
	}
	// And it must name the version actually running, not the older published
	// one — "up to date (0.12.1)" while running 0.13.0 is the same lie inverted.
	if !strings.Contains(got, "up to date (0.13.0)") {
		t.Fatalf("expected the running version reported as up to date, got %q", got)
	}
}

// A tag differing from the running version only by a "v" prefix is the SAME
// version. String inequality called it an update.
func TestAutoUpdateStatusLineIgnoresVPrefix(t *testing.T) {
	got := autoUpdateStatusLine("", "1.0.0", "v1.0.0", true)

	if strings.Contains(got, "newer release available") {
		t.Fatalf("v-prefixed same version must not read as an update: %q", got)
	}
}

func TestAutoUpdateStatusLineEqualVersionIsUpToDate(t *testing.T) {
	got := autoUpdateStatusLine("", "0.12.1", "0.12.1", true)

	if !strings.Contains(got, "up to date (0.12.1)") {
		t.Fatalf("expected up-to-date line, got %q", got)
	}
}

// The opt-out must be read exactly as the updater reads it (trimmed,
// case-folded). "TRUE" silenced the daemon while doctor reported auto-update on.
func TestAutoUpdateStatusLineHonorsEnvOptOutAsUpdaterDoes(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " 1 "} {
		got := autoUpdateStatusLine(v, "0.11.0", "0.12.1", true)
		if !strings.Contains(got, "auto-update disabled") {
			t.Fatalf("%q should read as disabled, got %q", v, got)
		}
		if !selfupdate.EnvDisablesAutoUpdate(v) {
			t.Fatalf("test premise broken: updater does not treat %q as an opt-out", v)
		}
	}
}

// A set-but-falsy value leaves auto-update ON, and doctor must say so rather
// than fall through to a vaguer line.
func TestAutoUpdateStatusLineFalsyEnvIsNotAnOptOut(t *testing.T) {
	for _, v := range []string{"0", "false", "off", ""} {
		got := autoUpdateStatusLine(v, "0.11.0", "0.12.1", true)
		if strings.Contains(got, "auto-update disabled") {
			t.Fatalf("%q must not read as an opt-out, got %q", v, got)
		}
		if selfupdate.EnvDisablesAutoUpdate(v) {
			t.Fatalf("test premise broken: updater treats %q as an opt-out", v)
		}
	}
}

func TestAutoUpdateStatusLineDevBuild(t *testing.T) {
	for _, v := range []string{"dev", ""} {
		got := autoUpdateStatusLine("", v, "0.12.1", true)
		if !strings.Contains(got, "inactive for dev build") {
			t.Fatalf("version %q should report dev build, got %q", v, got)
		}
	}
}

// The probe is best-effort and degrades silently; doctor still has to say what
// update checks will do rather than say nothing.
func TestAutoUpdateStatusLineProbeFailureStillReportsOn(t *testing.T) {
	got := autoUpdateStatusLine("", "0.12.1", "", false)

	if !strings.Contains(got, "update checks on") {
		t.Fatalf("expected an on line when the probe fails, got %q", got)
	}
	if strings.Contains(got, "up to date") || strings.Contains(got, "newer release") {
		t.Fatalf("a failed probe must not assert a version verdict: %q", got)
	}
}
