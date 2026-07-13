package selfupdate

import (
	"strconv"
	"strings"
)

// compareVersions compares two dotted semver-ish strings (major.minor.patch),
// tolerating a leading "v" and missing components (treated as 0). It returns
// -1 if a < b, 0 if equal, +1 if a > b. Only the numeric major/minor/patch
// triple is considered; pre-release/build suffixes are ignored — a release
// train the CLI actually ships never uses them. Non-numeric components parse as
// 0 so a garbage tag can never be judged "newer" and trigger a bogus swap.
func compareVersions(a, b string) int {
	pa := parseVersion(a)
	pb := parseVersion(b)
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

// isNewer reports whether target is strictly newer than current. This is the
// only predicate that authorizes a swap: equal or older targets are no-ops, so
// a stale "latest" or a backwards org pin can never downgrade the fleet.
func isNewer(current, target string) bool {
	return compareVersions(current, target) < 0
}

// parseVersion extracts the leading major.minor.patch integers from a version
// string, stripping a leading "v" and stopping each component at the first
// non-digit run. Missing components are 0.
func parseVersion(v string) [3]int {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	// Drop any pre-release/build suffix (e.g. "1.2.3-rc1" -> "1.2.3").
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.SplitN(v, ".", 4)
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		out[i] = leadingInt(parts[i])
	}
	return out
}

// leadingInt parses the leading run of ASCII digits in s as an int, returning 0
// when s has no leading digit.
func leadingInt(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}
