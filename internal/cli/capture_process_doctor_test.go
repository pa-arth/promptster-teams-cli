package cli

import (
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
)

func joined(lines []doctorLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func worst(lines []doctorLine) (warn, err bool) {
	for _, l := range lines {
		warn = warn || l.Warn
		err = err || l.Err
	}
	return
}

// A daemon on an older build is THE failure this file exists for: capture keeps
// running, every other doctor line stays green, and features that install
// themselves at watch startup were never installed. It must be an error, and it
// must name the fix — a warning the engineer has to interpret is how this went
// unnoticed for months.
func TestCaptureProcessFlagsAnOlderDaemon(t *testing.T) {
	lines := captureProcessLines(capture.CaptureProcess{
		Running: true, PID: 71770, Version: "0.11.3", Exe: "/opt/old/promptster-teams",
	}, "0.12.2")

	_, isErr := worst(lines)
	if !isErr {
		t.Fatalf("a daemon older than this binary must be an error, got %+v", lines)
	}
	text := joined(lines)
	for _, want := range []string{"0.11.3", "0.12.2", "promptster-teams stop", "/opt/old/promptster-teams"} {
		if !strings.Contains(text, want) {
			t.Errorf("stale-daemon line must mention %q; got:\n%s", want, text)
		}
	}
}

// The mirror case, and the one a naive "versions differ" check gets wrong. A
// daemon NEWER than the binary printing the report means this copy is the stale
// one (an old `npx`, a second binary earlier in PATH). Telling that engineer to
// restart capture would replace a current daemon with an older one.
func TestCaptureProcessDoesNotTellYouToRestartANewerDaemon(t *testing.T) {
	lines := captureProcessLines(capture.CaptureProcess{
		Running: true, PID: 42, Version: "0.13.0",
	}, "0.12.2")

	if _, isErr := worst(lines); isErr {
		t.Errorf("a newer daemon is not an error, got %+v", lines)
	}
	if text := joined(lines); strings.Contains(text, "promptster-teams stop") {
		t.Errorf("must not prescribe a restart when the daemon is the newer side; got:\n%s", text)
	}
}

// Every daemon started before this record existed reports no version, and that
// population is exactly the one most likely to be stale. "Unknown" must not
// render as "fine".
func TestCaptureProcessUnknownBuildIsNotSilent(t *testing.T) {
	lines := captureProcessLines(capture.CaptureProcess{Running: true, PID: 7}, "0.12.2")
	if warn, err := worst(lines); !warn && !err {
		t.Fatalf("an unrecorded build must not read as healthy, got %+v", lines)
	}
}

func TestCaptureProcessCleanWhenBuildsMatch(t *testing.T) {
	lines := captureProcessLines(capture.CaptureProcess{
		Running: true, PID: 9, Version: "0.12.2",
	}, "0.12.2")
	if warn, err := worst(lines); warn || err {
		t.Fatalf("matching builds must be clean, got %+v", lines)
	}
}

// A locally-built binary reports "dev", which parses as 0.0.0. Without the
// guard, every release binary calls a developer's own daemon ancient and every
// dev binary calls a release daemon newer.
func TestCaptureProcessIgnoresUnstampedBuilds(t *testing.T) {
	for _, tc := range []struct{ running, ours string }{
		{"dev", "0.12.2"},
		{"0.12.2", "dev"},
		{"", "0.12.2"},
	} {
		lines := captureProcessLines(capture.CaptureProcess{
			Running: true, PID: 1, Version: tc.running,
		}, tc.ours)
		if _, isErr := worst(lines); isErr {
			t.Errorf("running=%q ours=%q must not be an error, got %+v", tc.running, tc.ours, lines)
		}
	}
}

func TestCaptureProcessSaysWhenNothingIsCapturing(t *testing.T) {
	lines := captureProcessLines(capture.CaptureProcess{}, "0.12.2")
	if warn, _ := worst(lines); !warn {
		t.Fatalf("capture not running must warn, got %+v", lines)
	}
	if !strings.Contains(joined(lines), "not running") {
		t.Errorf("must say capture is not running; got:\n%s", joined(lines))
	}
}

// The silent-death detector: the unit names an absolute path baked at enable
// time, and when npm deletes that file the running daemon holds its inode so
// nothing looks wrong until the next login, when capture simply never returns.
func TestAutostartFlagsAProgramPathThatIsGone(t *testing.T) {
	lines := autostartLines(
		service.State{Installed: true, Loaded: true, Detail: "enabled", ProgramPath: "/gone/promptster-teams"},
		true, "/usr/local/bin/promptster-teams", "/home/u/.promptster-teams/bin/promptster-teams",
		func(string) bool { return false },
	)
	if _, isErr := worst(lines); !isErr {
		t.Fatalf("a missing autostart binary must be an error, got %+v", lines)
	}
	if !strings.Contains(joined(lines), "/gone/promptster-teams") {
		t.Errorf("must name the missing path; got:\n%s", joined(lines))
	}
}

// The dangerous direction here is the FALSE ALARM. ProgramPath is empty on
// Windows by design, and a machine that cannot report its baked path must print
// nothing about it rather than the most alarming line doctor has.
func TestAutostartSaysNothingAboutAnUnknownProgramPath(t *testing.T) {
	lines := autostartLines(
		service.State{Installed: true, Loaded: true, Detail: "enabled (Task Scheduler, runs at logon)"},
		true, `C:\Users\u\AppData\Roaming\npm\promptster-teams.exe`,
		`C:\Users\u\.promptster-teams\bin\promptster-teams.exe`,
		func(string) bool { t.Fatal("must not probe the filesystem for an unknown path"); return false },
	)
	if warn, err := worst(lines); warn || err {
		t.Fatalf("an unknown program path must stay silent, got %+v", lines)
	}
	if len(lines) != 1 {
		t.Errorf("expected only the status line, got %+v", lines)
	}
}

// `stop` boots the launchd job out of the live domain and leaves the plist on
// disk, so this state is reachable from our own commands — and it printed a
// green check, which is the shape of a warning nobody reads.
func TestAutostartInstalledButNotLoadedIsNotGreen(t *testing.T) {
	lines := autostartLines(
		service.State{Installed: true, Detail: "installed but not loaded (try re-running enable)"},
		false, "/bin/x", "/bin/x", func(string) bool { return true },
	)
	if warn, _ := worst(lines); !warn {
		t.Fatalf("installed-but-not-loaded must warn, got %+v", lines)
	}
}

// The same unloaded unit is NOT a warning while capture is running, and that
// asymmetry is load-bearing. A live watcher holds the single-instance lock, so
// the supervisor's own spawn exits 0 and the job reads as not-running — the
// documented success case, which every Linux `stop && start` produces. Warning
// there fires on healthy machines and teaches people to skim the line.
func TestAutostartUnloadedIsNotAWarningWhileCaptureRuns(t *testing.T) {
	lines := autostartLines(
		service.State{Installed: true, Detail: "enabled (systemd --user, inactive)"},
		true, "/bin/x", "/bin/x", func(string) bool { return true },
	)
	if warn, err := worst(lines); warn || err {
		t.Fatalf("unloaded + capturing must not alarm, got %+v", lines)
	}
	if !strings.Contains(joined(lines), "next login") {
		t.Errorf("should still say the supervisor takes over at login; got:\n%s", joined(lines))
	}
}

// The managed path and the running binary are BOTH legitimate: an engineer
// running `npx` has a self that differs from the plist's path while everything
// is perfectly healthy. Warning there would train people to ignore the line.
func TestAutostartAcceptsTheManagedAndTheRunningBinary(t *testing.T) {
	const managed = "/home/u/.promptster-teams/bin/promptster-teams"
	for _, baked := range []string{managed, "/tmp/npx-cache/promptster-teams"} {
		lines := autostartLines(
			service.State{Installed: true, Loaded: true, Detail: "enabled", ProgramPath: baked},
			true, "/tmp/npx-cache/promptster-teams", managed,
			func(string) bool { return true },
		)
		if warn, err := worst(lines); warn || err {
			t.Errorf("baked=%q must be clean, got %+v", baked, lines)
		}
	}
}

func TestAutostartFlagsAThirdBinary(t *testing.T) {
	lines := autostartLines(
		service.State{Installed: true, Loaded: true, Detail: "enabled", ProgramPath: "/opt/other/promptster-teams"},
		true, "/tmp/npx/promptster-teams", "/home/u/.promptster-teams/bin/promptster-teams",
		func(string) bool { return true },
	)
	if warn, _ := worst(lines); !warn {
		t.Fatalf("a baked path that is neither self nor managed must warn, got %+v", lines)
	}
}
