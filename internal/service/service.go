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
	// Status reports whether autostart is installed plus a human-readable detail.
	Status() (installed bool, detail string, err error)
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

[Install]
WantedBy=default.target
`
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
