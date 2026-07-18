package state

import (
	"path/filepath"
	"testing"
)

// TestHomeRelative pins the home-collapse contract: empty → empty, under-home →
// "~/…", exact home → "~", and an absolute path outside home stays unchanged
// (it carries no username). os.UserHomeDir reads $HOME on unix, so a fake HOME
// makes the boundary deterministic without touching the real home dir.
func TestHomeRelative(t *testing.T) {
	home := filepath.Join(t.TempDir(), "user")
	t.Setenv("HOME", home)

	cases := []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{"exact home collapses to tilde", home, "~"},
		{"under home collapses", filepath.Join(home, "repos", "foo", "bar"), "~/repos/foo/bar"},
		{"outside-home absolute unchanged", "/tmp/ws", "/tmp/ws"},
		{"sibling of home is not mangled", home + "-sibling/x", home + "-sibling/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HomeRelative(tc.in); got != tc.want {
				t.Errorf("HomeRelative(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHomeRelativeStrict pins the TELEMETRY contract: the result is "~"-prefixed
// or EMPTY, never a raw absolute path. Unlike HomeRelative (which returns an
// outside-home path unchanged for user-facing display), an outside-home path or
// a home-lookup failure must collapse to "" so the workdir field is omitted
// rather than leaking an absolute path that can carry the OS username.
func TestHomeRelativeStrict(t *testing.T) {
	home := filepath.Join(t.TempDir(), "user")
	t.Setenv("HOME", home)

	cases := []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{"exact home collapses to tilde", home, "~"},
		{"under home collapses", filepath.Join(home, "repos", "foo", "bar"), "~/repos/foo/bar"},
		{"outside-home absolute dropped", "/tmp/ws", ""},
		{"outside-home with username dropped", "/mnt/users/alice/repo", ""},
		{"sibling of home dropped (not mangled, not leaked)", home + "-sibling/x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HomeRelativeStrict(tc.in); got != tc.want {
				t.Errorf("HomeRelativeStrict(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHomeRelativeStrictHomeLookupFailure simulates os.UserHomeDir failing (empty
// HOME on unix) — HomeRelative returns the path unchanged, so HomeRelativeStrict
// must return "" to avoid emitting an absolute, username-bearing path as workdir.
func TestHomeRelativeStrictHomeLookupFailure(t *testing.T) {
	t.Setenv("HOME", "")
	if got := HomeRelativeStrict("/mnt/users/alice/repo"); got != "" {
		t.Errorf("HomeRelativeStrict on home-lookup failure = %q, want \"\"", got)
	}
}
