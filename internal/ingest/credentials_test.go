package ingest

import "testing"

// TestIsEngineerKey pins the developer-key format the CLI accepts. The bug this
// guards: the backend upgraded to six-group (120-bit) keys, but the CLI still
// required exactly two groups and rejected every real key at login. The regex is
// intentionally count-agnostic (2+ groups) — this asserts both the real six-group
// shape and legacy two-group keys validate, while genuinely malformed input does not.
func TestIsEngineerKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"six groups (backend mints today)", "PSE-VJA3-3W49-6RX8-D2QC-S7CN-CE8N", true}, // gitleaks:allow
		{"two groups (legacy)", "PSE-AB2C-9XYZ", true},                                  // gitleaks:allow
		{"surrounding whitespace trimmed", "  PSE-AB2C-9XYZ\n", true},                   // gitleaks:allow
		{"empty", "", false},
		{"prefix only", "PSE-", false},
		{"single group", "PSE-AB2C", false},
		{"wrong prefix", "PST-AB2C-9XYZ", false}, // gitleaks:allow — candidate key, not engineer
		{"short group", "PSE-AB2-9XYZ", false},
		{"long group", "PSE-AB2CD-9XYZ", false},
		{"lowercase", "PSE-ab2c-9xyz", false},
		{"ambiguous charset I", "PSE-ABIC-9XYZ", false}, // I excluded
		{"ambiguous charset O", "PSE-ABOC-9XYZ", false}, // O excluded
		{"ambiguous charset 0", "PSE-AB0C-9XYZ", false}, // 0 excluded
		{"ambiguous charset 1", "PSE-AB1C-9XYZ", false}, // 1 excluded
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsEngineerKey(c.key); got != c.want {
				t.Errorf("IsEngineerKey(%q) = %v, want %v", c.key, got, c.want)
			}
		})
	}
}
