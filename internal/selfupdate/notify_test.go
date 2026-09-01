package selfupdate

import "testing"

// The tag reaches promptGUI from a GitHub redirect and is interpolated into an
// AppleScript string literal. tagFromReleaseLocation rejects path separators —
// it was written to protect a URL path — but NOT quotes, which is what ends a
// script literal. These pin the sanitizer that closes that gap.
func TestSanitizeForDialogStripsScriptBreakingCharacters(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0.25.0", "0.25.0"},
		{"v1.2.3-rc.1+build_7", "v1.2.3-rc.1+build_7"},
		// The injection shapes: a quote closes the literal, the rest would run.
		{`1.0" & (do shell script "id") & "`, `1.0doshellscriptid`},
		{"1.0\\\"", "1.0"},
		{"1.0\nnew line", "1.0newline"},
		{"1.0; rm -rf /", "1.0rm-rf"},
		{"1.0$(whoami)", "1.0whoami"},
		{"1.0`id`", "1.0id"},
	} {
		if got := sanitizeForDialog(tc.in); got != tc.want {
			t.Errorf("sanitizeForDialog(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The release-notes link is shown to a human as proof of what they are
// approving, so anything not our own https GitHub URL is dropped rather than
// rendered — a malformed link in that dialog is a signal, not decoration.
func TestIsSafeURLAcceptsOnlyOurReleaseLinks(t *testing.T) {
	good := []string{
		"https://github.com/pa-arth/promptster-teams-cli/releases/tag/v0.25.0",
		"https://github.com/pa-arth/promptster-teams-cli/releases/tag/v1.2.3-rc.1",
	}
	for _, u := range good {
		if !isSafeURL(u) {
			t.Errorf("isSafeURL(%q) = false, want true", u)
		}
	}
	bad := []string{
		"http://github.com/x",                      // not https
		"https://evil.test/pa-arth/x",              // not github
		`https://github.com/x" & (do shell script`, // quote + space
		"https://github.com/x\nrm -rf /",           // newline
		"",
	}
	for _, u := range bad {
		if isSafeURL(u) {
			t.Errorf("isSafeURL(%q) = true, want false", u)
		}
	}
}

// A sanitized value must survive escaping unchanged; the two layers exist so
// neither has to be perfect alone.
func TestEscapeAppleScriptClosesTheLiteral(t *testing.T) {
	if got := escapeAppleScript(`a"b\c`); got != `a\"b\\c` {
		t.Fatalf("escapeAppleScript = %q", got)
	}
}
