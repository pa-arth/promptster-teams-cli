package selfupdate

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3", "v1.2.3", 0},
		{"1.2.4", "1.2.3", 1},
		{"1.2.3", "1.2.4", -1},
		{"1.3.0", "1.2.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.2", "1.2.0", 0}, // missing patch == 0
		{"1", "1.0.0", 0},   // missing minor+patch == 0
		{"1.2.0", "1.2", 0}, // symmetric
		{"0.5.2", "0.5.10", -1},
		{"0.5.10", "0.5.2", 1},
		{"1.2.3-rc1", "1.2.3", 0}, // pre-release suffix ignored
		{"1.2.3+build5", "1.2.3", 0},
		{"garbage", "1.0.0", -1}, // non-numeric parses as 0.0.0
		{"1.0.0", "garbage", 1},
		{"", "0.0.1", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	if !isNewer("0.5.2", "0.5.3") {
		t.Error("0.5.3 should be newer than 0.5.2")
	}
	if isNewer("0.5.3", "0.5.3") {
		t.Error("equal versions are not newer")
	}
	if isNewer("0.5.3", "0.5.2") {
		t.Error("older target must not be newer (no downgrade)")
	}
	if isNewer("1.0.0", "v1.0.0") {
		t.Error("v-prefixed equal must not be newer")
	}
}
