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
