// Package service registers promptster-teams capture as a per-user OS service
// that launches at login and is kept alive by the platform supervisor, so
// capture survives reboots with zero user action. Without this, a manual
// `start` dies on reboot/logout and every gap silently under-counts the
// seat-utilization metric.
//
// The service runs `promptster-teams watch` (the long-lived foreground
// supervisor), NOT `start` — `start` detaches and exits immediately, which a
// supervisor reads as an instant crash and restart-loops. The OS supervisor
// replaces the manual detach + PID-file dance and is strictly more robust.
//
// Each platform's Enable/Disable/Status lives in a build-tagged file
// (service_darwin.go / service_linux.go / service_windows.go), mirroring the
// detach_*.go and signing_lock_*.go convention. The pure unit/plist/task
// renderers stay here, untagged, so they compile and golden-test on any host.
package service

import (
	"strings"
)

// label is the launchd/service identifier; taskName is the Windows Task
// Scheduler task name (spaces allowed there, not in the reverse-DNS label).
const (
	label    = "ai.promptster.teams"
	taskName = "Promptster Teams"
)

// State is what autostart is doing right now. It is a struct rather than the
// (bool, string) it used to be because "installed" and "armed right now" are
// genuinely different facts, and a caller that can only see the first has to
// recover the second by pattern-matching the prose in Detail — which is how
// `doctor` ended up printing a green check over "installed but not loaded".
type State struct {
	// Installed means the unit/plist/task exists, so capture returns at login.
	Installed bool
	// Loaded means the platform supervisor is watching the job RIGHT NOW.
	//
	// It is not the same question as Installed. `stop` boots the launchd job out
	// of the live GUI domain and deliberately leaves the plist on disk, so a
	// stopped-then-started machine is Installed && !Loaded: capture is running
	// but unsupervised, and nothing revives it until the next login. On Linux the
	// analogous fact is the enable symlink (a `systemctl --user stop` leaves the
	// unit enabled), and on Windows an ONLOGON task has no separate loaded state,
	// so Loaded == Installed there.
	Loaded bool
	// Detail is the human-readable one-liner for status output.
	Detail string
	// ProgramPath is the ABSOLUTE binary path baked into the unit at enable time
	// — the binary that will actually run at the next login, which is not
	// necessarily the one running now (see autostart repair).
	//
	// Empty means UNKNOWN, and callers must stay silent on unknown rather than
	// guess. A diagnostic that cries wolf about a healthy machine is worse than
	// one that says nothing: the Windows path is exactly where a naive parse
	// produces a confident wrong answer, so it reports "" instead of parsing
	// schtasks XML.
	ProgramPath string
}

// Manager installs, removes, and reports the per-user autostart service for the
// current platform. New() returns the platform implementation.
type Manager interface {
	// Enable registers the service and starts it now (and at every login).
	Enable() error
	// Stop halts the service now but leaves it registered, so it still returns at
	// the next login. Idempotent; a no-op when autostart isn't installed.
	//
	// This exists so `stop` can disarm the supervisor's restart policy before
	// signaling the watcher. Without it, `stop`'s SIGKILL escalation reads as a
	// crash to launchd's KeepAlive / systemd's Restart=on-failure and capture is
	// resurrected seconds later — while `stop` reports success.
	Stop() error
	// Disable stops and deregisters the service. Idempotent.
	Disable() error
	// Status reports what autostart is doing right now.
	Status() (State, error)
}

// xmlEscape escapes the five XML special chars for safe interpolation into the
// plist (a home/bin path could in principle contain & or <).
var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func xmlEscape(s string) string { return xmlEscaper.Replace(s) }

// renderPlist builds the launchd LaunchAgent for macOS. RunAtLoad starts it at
// login; KeepAlive{SuccessfulExit:false} restarts it only on a crash (so a
// clean exit — e.g. the single-instance guard bowing out — doesn't respawn a
// duplicate); ThrottleInterval throttles crash-restart storms.
func renderPlist(bin, log, home string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(bin) + `</string>
		<string>watch</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>WorkingDirectory</key>
	<string>` + xmlEscape(home) + `</string>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(log) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(log) + `</string>
</dict>
</plist>
`
}

// renderUnit builds the systemd --user unit for Linux. WantedBy=default.target
// starts it at graphical/user login. Restart=on-failure (NOT always) is the
// systemd analog of the mac plist's KeepAlive{SuccessfulExit:false}: it revives
// a crashed watcher but does NOT restart a clean exit(0) — critical because the
// single-instance guard exits 0 when the lock is already held, and Restart=always
// would busy-loop that bow-out every RestartSec forever. Logs go to journald
// (journalctl --user -u promptster-teams). The binary is quoted so a home dir
// with spaces still parses.
func renderUnit(bin string) string {
	return `[Unit]
Description=Promptster Teams — on-device AI coding capture
After=default.target

[Service]
Type=simple
ExecStart="` + bin + `" watch
Restart=on-failure
RestartSec=10
# Let the update notification reach the engineer's screen when the desktop has
# imported these (GNOME/KDE run: systemctl --user import-environment). systemd
# silently ignores names it does not have, so this is a no-op on a headless box
# rather than a startup failure — and selfupdate falls back to a display guess
# and then to its printed surfaces if they never arrive.
PassEnvironment=DISPLAY WAYLAND_DISPLAY XAUTHORITY

[Install]
WantedBy=default.target
`
}

// programPathFromPlist reads back the binary renderPlist baked into
// ProgramArguments. Parsing our OWN rendered XML is the whole reason this is
// safe to do here while `autostart repair` still re-renders blind: repair must
// work on any unit, this only has to describe one we wrote.
//
// Anything it cannot recognize returns "" (unknown), never a guess — the caller
// prints nothing on unknown, so a parse that drifts degrades to silence instead
// of to a false alarm about a healthy machine.
func programPathFromPlist(xml string) string {
	i := strings.Index(xml, "<key>ProgramArguments</key>")
	if i < 0 {
		return ""
	}
	rest := xml[i:]
	j := strings.Index(rest, "<string>")
	if j < 0 {
		return ""
	}
	rest = rest[j+len("<string>"):]
	k := strings.Index(rest, "</string>")
	if k < 0 {
		return ""
	}
	return xmlUnescape(rest[:k])
}

// xmlUnescape reverses xmlEscape. &amp; is applied LAST so a literal "&amp;lt;"
// in a path survives the round trip instead of being decoded twice.
func xmlUnescape(s string) string {
	for _, r := range [][2]string{
		{"&lt;", "<"}, {"&gt;", ">"}, {"&quot;", `"`}, {"&apos;", "'"}, {"&amp;", "&"},
	} {
		s = strings.ReplaceAll(s, r[0], r[1])
	}
	return s
}

// programPathFromUnit reads back the binary renderUnit baked into ExecStart.
// renderUnit always quotes it (a home dir may contain spaces), so the quoted
// span is the path; an unquoted ExecStart is not ours and reports "" rather
// than a first-token guess that would truncate at the first space.
func programPathFromUnit(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		v := strings.TrimPrefix(line, "ExecStart=")
		if !strings.HasPrefix(v, `"`) {
			return ""
		}
		if end := strings.Index(v[1:], `"`); end >= 0 {
			return v[1 : 1+end]
		}
		return ""
	}
	return ""
}

// renderTaskArgs builds the schtasks argv (no shell) that creates the ONLOGON
// task on Windows. The binary is quoted inside /TR because the install path can
// contain spaces. /F overwrites an existing task so enable is idempotent.
func renderTaskArgs(bin string) []string {
	return []string{
		"/Create",
		"/TN", taskName,
		"/TR", `"` + bin + `" watch`,
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/F",
	}
}
