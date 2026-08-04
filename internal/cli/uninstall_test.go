package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
)

// sandboxHome points every home-relative path (~/.cursor, ~/.claude,
// ~/.promptster-teams) at a throwaway dir.
//
// It does NOT make the autostart or daemon-stop steps safe: launchctl targets
// gui/$UID by LABEL, so a sandboxed HOME would still tear down the developer's
// real ai.promptster.teams job, and StopTeamsDaemon signals whatever live
// watcher it finds. Those two are always faked through uninstallDeps here; the
// file-touching steps run for real against this dir.
func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// StateDir() honours this first; without it the purge test would delete the
	// developer's real ~/.promptster-teams via the second purge target.
	t.Setenv("PROMPTSTER_STATE_DIR", filepath.Join(home, ".promptster-teams"))
	return home
}

// noopDeps is a full set of deps that touch nothing, so each test can override
// exactly the one step it is about.
func noopDeps() uninstallDeps {
	return uninstallDeps{
		stopCapture:       func() error { return nil },
		captureRunning:    func() bool { return false },
		autostartStatus:   func() (service.State, error) { return service.State{Detail: "not enabled"}, nil },
		autostartDisable:  func() error { return nil },
		removeCursorHooks: func() (bool, error) { return false, nil },
		statuslineWrapped: func() bool { return false },
		disableStatusline: func() error { return nil },
		purgeDirs:         func() []string { return nil },
	}
}

// The headline case: an engineer removes the CLI, and the Cursor hook entry must
// go with it. A survivor points Cursor at a binary that will not exist and makes
// it exec a missing command inside the agent loop on every event.
func TestUninstallUnenrollsTheCursorHook(t *testing.T) {
	home := sandboxHome(t)
	hooks := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooks), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooks, []byte(
		`{"version":1,"hooks":{"beforeSubmitPrompt":[{"type":"command","command":"/usr/local/bin/theirs"}]}}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}
	if !hooksJSONContains(t, hooks, "cursor-hook") {
		t.Fatal("setup failed: our hook was never enrolled")
	}

	d := noopDeps()
	d.removeCursorHooks = capture.RemoveCursorHooks
	var out bytes.Buffer
	if code := runUninstall(&out, d, false); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}

	if hooksJSONContains(t, hooks, "cursor-hook") {
		body, _ := os.ReadFile(hooks) // #nosec G304 -- test fixture path.
		t.Fatalf("our hook survived uninstall:\n%s", body)
	}
	// Their own hook is not ours to remove.
	if !hooksJSONContains(t, hooks, "/usr/local/bin/theirs") {
		t.Fatal("the engineer's own hook was deleted")
	}
	if !strings.Contains(out.String(), "Cursor hook unenrolled") {
		t.Fatalf("output did not report the unenrollment:\n%s", out.String())
	}
}

// A failing step must not skip the ones after it. Short-circuiting on the first
// error is how the Cursor hook survives an uninstall, which is the whole thing
// this command exists to stop.
func TestUninstallContinuesPastAFailedStep(t *testing.T) {
	sandboxHome(t)
	removed := false
	statuslineRestored := false

	d := noopDeps()
	d.autostartStatus = func() (service.State, error) {
		return service.State{Installed: true, Loaded: true, Detail: "installed"}, nil
	}
	d.autostartDisable = func() error { return errors.New("launchctl said no") }
	d.stopCapture = func() error { return errors.New("could not signal") }
	d.removeCursorHooks = func() (bool, error) { removed = true; return true, nil }
	d.disableStatusline = func() error { statuslineRestored = true; return nil }

	var out bytes.Buffer
	code := runUninstall(&out, d, false)

	if !removed {
		t.Fatal("the Cursor hook step was skipped after an earlier failure")
	}
	if !statuslineRestored {
		t.Fatal("the statusline step was skipped after an earlier failure")
	}
	if code == 0 {
		t.Fatalf("exit code = 0 despite two failed steps\n%s", out.String())
	}
	if !strings.Contains(out.String(), "launchctl said no") {
		t.Fatalf("the autostart failure was not reported:\n%s", out.String())
	}
}

// The one lie that would actually cost something: reporting a clean stop over
// capture that is still alive. An engineer who reads "stopped" walks away, and
// the daemon keeps shipping events from a machine they believe is unenrolled.
func TestUninstallFailsWhenCaptureSurvivesTheStop(t *testing.T) {
	sandboxHome(t)
	d := noopDeps()
	d.captureRunning = func() bool { return true } // alive before AND after
	var out bytes.Buffer
	code := runUninstall(&out, d, false)

	if code == 0 {
		t.Fatalf("exit code = 0 while capture is still running\n%s", out.String())
	}
	if strings.Contains(out.String(), "background capture stopped") {
		t.Fatalf("claimed capture was stopped while it is still running:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "STILL running") {
		t.Fatalf("did not report the surviving daemon:\n%s", out.String())
	}
}

// A status probe that fails must not stop the removal. Disable is idempotent on
// every platform, so calling it blind costs nothing — while skipping it leaves a
// registered unit that brings capture back at the next login, the worst residue
// an uninstall can leave. Raised by review on PR #124.
func TestUninstallStillDeregistersWhenTheStatusProbeFails(t *testing.T) {
	sandboxHome(t)
	disabled := false
	d := noopDeps()
	d.autostartStatus = func() (service.State, error) { return service.State{}, errors.New("cannot stat the plist") }
	d.autostartDisable = func() error { disabled = true; return nil }

	var out bytes.Buffer
	code := runUninstall(&out, d, false)

	if !disabled {
		t.Fatalf("autostart was left registered because its status could not be read:\n%s", out.String())
	}
	// The probe failing is not itself a failure once the removal succeeded.
	if code != 0 {
		t.Fatalf("exit code = %d after a successful blind removal\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "status could not be read") {
		t.Fatalf("the output hid the failed probe:\n%s", out.String())
	}
}

// Without --purge the key and the unsent queue survive: an accidental uninstall
// should cost an enrollment, not a backlog of captured events.
func TestUninstallWithoutPurgeKeepsCredentialsAndQueue(t *testing.T) {
	home := sandboxHome(t)
	stateDir := filepath.Join(home, ".promptster-teams")
	writeFixture(t, filepath.Join(stateDir, "credentials"), `{"token":"PSE-XXXX"}`)
	writeFixture(t, filepath.Join(stateDir, "outbox.jsonl"), "{}\n")

	d := noopDeps()
	d.purgeDirs = func() []string { return []string{stateDir} }

	var out bytes.Buffer
	if code := runUninstall(&out, d, false); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "credentials")); err != nil {
		t.Fatalf("credentials were deleted without --purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "outbox.jsonl")); err != nil {
		t.Fatalf("the event queue was deleted without --purge: %v", err)
	}
}

func TestUninstallPurgeDeletesTheStateDir(t *testing.T) {
	home := sandboxHome(t)
	stateDir := filepath.Join(home, ".promptster-teams")
	writeFixture(t, filepath.Join(stateDir, "credentials"), `{"token":"PSE-XXXX"}`)
	writeFixture(t, filepath.Join(stateDir, "bin", "promptster-teams"), "binary")

	d := noopDeps()
	d.purgeDirs = func() []string { return []string{stateDir} }

	var out bytes.Buffer
	if code := runUninstall(&out, d, true); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("%s survived --purge (err=%v)", stateDir, err)
	}
}

// `npm rm -g` runs no uninstall script and leaves the managed binary behind
// (both measured against npm 11.5.2), so removing the package alone stops
// nothing. Every run has to say so — an engineer who only reads this output must
// not walk away believing the package manager finished the job.
func TestUninstallAlwaysNamesTheNpmGap(t *testing.T) {
	sandboxHome(t)
	for _, purge := range []bool{false, true} {
		var out bytes.Buffer
		if code := runUninstall(&out, noopDeps(), purge); code != 0 {
			t.Fatalf("purge=%v: exit code = %d\n%s", purge, code, out.String())
		}
		if !strings.Contains(out.String(), "npm rm -g @promptster/teams-cli") {
			t.Fatalf("purge=%v: output never mentions the npm gap:\n%s", purge, out.String())
		}
	}
}

// Reporting what was OBSERVED, not what was attempted: a machine that never
// enabled autostart or the statusline must not be told they were removed.
func TestUninstallDoesNotClaimToRemoveWhatWasNeverThere(t *testing.T) {
	sandboxHome(t)
	var out bytes.Buffer
	if code := runUninstall(&out, noopDeps(), false); code != 0 {
		t.Fatalf("exit code = %d\n%s", code, out.String())
	}
	s := out.String()
	for _, claim := range []string{"autostart unit removed", "Claude statusline restored", "Cursor hook unenrolled"} {
		if strings.Contains(s, claim) {
			t.Fatalf("claimed %q on a machine where nothing was installed:\n%s", claim, s)
		}
	}
	for _, want := range []string{"autostart was not enabled", "no Cursor hook to unenroll", "statusline was not wrapped", "no background capture was running"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

// Running it twice is a legitimate thing to do after a partial failure.
func TestUninstallIsIdempotent(t *testing.T) {
	home := sandboxHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(home, ".promptster-teams")
	writeFixture(t, filepath.Join(stateDir, "credentials"), "x")

	d := noopDeps()
	d.removeCursorHooks = capture.RemoveCursorHooks
	d.purgeDirs = func() []string { return []string{stateDir} }

	for i := 0; i < 2; i++ {
		var out bytes.Buffer
		if code := runUninstall(&out, d, true); code != 0 {
			t.Fatalf("run %d: exit code = %d\n%s", i+1, code, out.String())
		}
		// The second run has nothing left to delete and must SAY so. A silent
		// purge section reads as "the purge did not run", which is the reading
		// that sends someone hunting for a directory that is already gone.
		if i == 1 && !strings.Contains(out.String(), "nothing to delete") {
			t.Fatalf("the second run said nothing about the already-deleted state dir:\n%s", out.String())
		}
	}
}

// pathUnder must not read a sibling directory as "inside" the state dir — the
// check that keeps --purge's Windows carve-out from skipping the wrong file.
func TestPathUnderRequiresASeparatorBoundary(t *testing.T) {
	base := filepath.Join("/home", "x", ".promptster-teams")
	if pathUnder(base+"-old", base) {
		t.Fatal("a sibling directory read as inside the state dir")
	}
	if !pathUnder(filepath.Join(base, "bin", "promptster-teams"), base) {
		t.Fatal("a real child did not read as inside the state dir")
	}
	if !pathUnder(base, base) {
		t.Fatal("the directory itself did not read as inside itself")
	}
}

func TestRemoveAllExceptKeepsOnlyTheRunningBinary(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "bin", "promptster-teams.exe")
	writeFixture(t, keep, "binary")
	writeFixture(t, filepath.Join(dir, "credentials"), "secret")
	writeFixture(t, filepath.Join(dir, "bin", "stale"), "junk")
	writeFixture(t, filepath.Join(dir, "outbox", "q.jsonl"), "{}")

	if err := removeAllExcept(dir, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the running binary was deleted: %v", err)
	}
	for _, gone := range []string{
		filepath.Join(dir, "credentials"),
		filepath.Join(dir, "bin", "stale"),
		filepath.Join(dir, "outbox"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s survived (err=%v)", gone, err)
		}
	}
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hooksJSONContains reports whether any command string in hooks.json contains
// needle. Parsed rather than grepped so a match cannot come from an unrelated
// key.
func hooksJSONContains(t *testing.T, path, needle string) bool {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test fixture path.
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Hooks map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("hooks.json is not valid JSON after uninstall: %v\n%s", err, data)
	}
	for _, entries := range parsed.Hooks {
		for _, e := range entries {
			if strings.Contains(e.Command, needle) {
				return true
			}
		}
	}
	return false
}
