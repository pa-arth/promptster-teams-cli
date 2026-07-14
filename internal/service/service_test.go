package service

import (
	"reflect"
	"strings"
	"testing"
)

// TestStopIsNoOpWhenNotInstalled pins the contract `stop` depends on: calling
// Stop when autostart was never enabled must succeed silently rather than error,
// so StopTeamsDaemon can call it unconditionally on every teardown.
//
// It skips when autostart IS installed — the assertion isn't worth booting out
// the developer's own running capture to make.
func TestStopIsNoOpWhenNotInstalled(t *testing.T) {
	mgr := New()
	installed, _, err := mgr.Status()
	if err != nil {
		t.Skipf("cannot read autostart status on this host: %v", err)
	}
	if installed {
		t.Skip("autostart is enabled on this host; refusing to stop the real service in a test")
	}
	if err := mgr.Stop(); err != nil {
		t.Errorf("Stop() on a host without autostart installed = %v, want nil (must be a no-op)", err)
	}
}

func TestRenderPlist(t *testing.T) {
	out := renderPlist("/opt/pt/promptster-teams", "/home/u/.promptster-teams/daemon.log", "/home/u")

	wants := []string{
		"<string>ai.promptster.teams</string>",
		"<string>/opt/pt/promptster-teams</string>", // the binary
		"<string>watch</string>",                    // the subcommand, never `start`
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>",
		"<false/>", // only restart on crash, not clean exit
		"<string>/home/u</string>",
		"<string>/home/u/.promptster-teams/daemon.log</string>",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("plist missing %q\n---\n%s", w, out)
		}
	}
	if strings.Contains(out, "<string>start</string>") {
		t.Error("plist must run `watch`, not `start` (start detaches and restart-loops the supervisor)")
	}
}

func TestRenderPlistEscapesXML(t *testing.T) {
	out := renderPlist("/home/a&b/promptster-teams", "/log", "/home/a&b")
	if strings.Contains(out, "a&b") {
		t.Errorf("bare & not escaped in plist:\n%s", out)
	}
	if !strings.Contains(out, "a&amp;b") {
		t.Errorf("expected escaped &amp; in plist:\n%s", out)
	}
}

func TestRenderUnit(t *testing.T) {
	out := renderUnit("/opt/pt/promptster-teams")

	wants := []string{
		`ExecStart="/opt/pt/promptster-teams" watch`, // quoted bin, `watch` subcommand
		"Restart=on-failure",                         // NOT always — a clean lock bow-out must not loop
		"RestartSec=10",
		"WantedBy=default.target",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("unit missing %q\n---\n%s", w, out)
		}
	}
	if strings.Contains(out, "Restart=always") {
		t.Error("Restart=always busy-loops the single-instance clean exit(0); want Restart=on-failure")
	}
	if strings.Contains(out, "start\n") || strings.Contains(out, "\" start") {
		t.Error("unit must run `watch`, not `start`")
	}
}

func TestRenderTaskArgs(t *testing.T) {
	got := renderTaskArgs(`C:\Program Files\pt\promptster-teams.exe`)
	want := []string{
		"/Create",
		"/TN", "Promptster Teams",
		"/TR", `"C:\Program Files\pt\promptster-teams.exe" watch`,
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/F",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("task args\n got %#v\nwant %#v", got, want)
	}
}
