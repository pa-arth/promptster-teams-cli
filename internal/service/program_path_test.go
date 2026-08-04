package service

import "testing"

// The unit is written by ONE version of this CLI and read back by another —
// that gap is the entire point of reading it — so the parser is pinned against
// the renderer rather than against a hand-written fixture that could drift with
// neither side noticing.
func TestProgramPathRoundTripsThroughTheRenderers(t *testing.T) {
	for _, bin := range []string{
		"/home/u/.promptster-teams/bin/promptster-teams",
		"/Users/a b/Library/Application Support/promptster-teams", // spaces
		"/home/r&d/tools/promptster-teams",                        // XML-escaped in the plist
	} {
		if got := programPathFromPlist(renderPlist(bin, "/tmp/log", "/home/u")); got != bin {
			t.Errorf("plist round trip: got %q, want %q", got, bin)
		}
		if got := programPathFromUnit(renderUnit(bin)); got != bin {
			t.Errorf("unit round trip: got %q, want %q", got, bin)
		}
	}
}

// Unknown must be reported as unknown. Every caller treats "" as "say nothing",
// so a parser that guesses on unfamiliar input turns a healthy machine into an
// alarming diagnostic — the failure mode that matters more than the miss.
func TestProgramPathIsEmptyRatherThanAGuess(t *testing.T) {
	for name, in := range map[string]string{
		"empty":                  "",
		"plist without the key":  "<plist><dict><key>Label</key><string>x</string></dict></plist>",
		"plist truncated":        "<key>ProgramArguments</key><array><string>/opt/pt",
		"unit without ExecStart": "[Service]\nRestart=on-failure\n",
	} {
		if got := programPathFromPlist(in); got != "" {
			t.Errorf("%s: plist parser returned %q, want \"\"", name, got)
		}
		if got := programPathFromUnit(in); got != "" {
			t.Errorf("%s: unit parser returned %q, want \"\"", name, got)
		}
	}
	// An unquoted ExecStart is not one of ours. Taking the first whitespace token
	// would silently truncate a path with a space in it and report a file that
	// does not exist — a false alarm dressed as a precise one.
	if got := programPathFromUnit(`ExecStart=/opt/my tools/promptster-teams watch`); got != "" {
		t.Errorf("unquoted ExecStart returned %q, want \"\"", got)
	}
}
