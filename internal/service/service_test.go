package service

import (
	"reflect"
	"strings"
	"testing"
)

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
		"Restart=always",
		"RestartSec=10",
		"WantedBy=default.target",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("unit missing %q\n---\n%s", w, out)
		}
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
