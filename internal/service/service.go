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
	"path/filepath"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
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
	// Disable stops and deregisters the service. Idempotent.
	Disable() error
	// Status reports whether autostart is installed plus a human-readable detail.
	Status() (installed bool, detail string, err error)
}

// binPath is the binary the service invokes — the canonical install path,
// already .exe-aware on Windows (internal/state/hooks.go).
func binPath() string { return state.PromptsterBin() }

// logPath is where launchd/Task Scheduler tee the watcher's stdout/stderr. It
// sits alongside the manual daemon's log under ~/.promptster-teams. (systemd
// logs to journald instead — see renderUnit.)
func logPath() string { return filepath.Join(state.GlobalPromptsterDir(), "daemon.log") }

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
// starts it at graphical/user login; Restart=always revives it on crash. Logs
// go to journald (journalctl --user -u promptster-teams). The binary is quoted
// so a home dir with spaces still parses.
func renderUnit(bin string) string {
	return `[Unit]
Description=Promptster Teams — on-device AI coding capture
After=default.target

[Service]
Type=simple
ExecStart="` + bin + `" watch
Restart=always
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
