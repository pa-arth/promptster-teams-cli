package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCursorHooks writes a hooks.json registering cmd for every step we
// enroll, matching what EnsureCursorHooks produces.
func writeCursorHooks(t *testing.T, path, cmd string, steps []string) {
	t.Helper()
	hooks := map[string][]cursorHookCmd{}
	for _, s := range steps {
		hooks[s] = []cursorHookCmd{{Type: "command", Command: cmd}}
	}
	data, err := json.Marshal(map[string]any{"version": 1, "hooks": hooks})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func onlyLine(t *testing.T, lines []CursorHookDoctorLine) CursorHookDoctorLine {
	t.Helper()
	if len(lines) != 1 {
		t.Fatalf("want exactly one diagnostic, got %d: %+v", len(lines), lines)
	}
	return lines[0]
}

// THE case this exists for: the binary was deleted without running `uninstall`,
// so Cursor execs a missing command inside the agent loop on every event. Once
// the binary is gone none of our code runs, so doctor — run from a working
// binary — is the only place this can ever be reported.
func TestCursorHooksDoctorFlagsADanglingCommand(t *testing.T) {
	home := sandboxHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	gone := filepath.Join(home, ".promptster-teams", "bin", "promptster-teams")
	writeCursorHooks(t, path, fmt.Sprintf("%q cursor-hook", gone), cursorHookSteps)

	l := onlyLine(t, CursorHooksDoctor())
	if !l.Err {
		t.Fatalf("a dangling hook command was not reported as an error: %+v", l)
	}
	if l.OK {
		t.Fatalf("a dangling hook command reported OK: %+v", l)
	}
	if !strings.Contains(l.Text, "uninstall") {
		t.Fatalf("the diagnostic does not say how to fix it: %q", l.Text)
	}
}

// The mirror-image failure, and the more dangerous one to ship: a HEALTHY
// enrollment must never be reported as dangling. On Windows the command arrives
// backslash-escaped through %q, so a naive scan to the next quote yields a path
// with doubled separators that os.Stat calls missing — turning every healthy
// Windows machine into the most alarming line doctor can print.
func TestCursorHooksDoctorDoesNotFalseAlarmOnAnEscapedPath(t *testing.T) {
	home := sandboxHome(t)
	bin := filepath.Join(home, "bin", "promptster-teams")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o700); err != nil { // #nosec G306 -- test fixture that must be executable-shaped.
		t.Fatal(err)
	}
	writeCursorHooks(t, filepath.Join(home, ".cursor", "hooks.json"), fmt.Sprintf("%q cursor-hook", bin), cursorHookSteps)

	if l := onlyLine(t, CursorHooksDoctor()); l.Err {
		t.Fatalf("a healthy enrollment was reported as dangling: %q", l.Text)
	}

	// And the parser itself, against a real Windows-shaped command. A naive scan
	// to the next quote returns this path with every separator doubled.
	const winPath = `C:\Users\dev\.promptster-teams\bin\promptster-teams.exe`
	if got := cursorHookCommandBinary(fmt.Sprintf("%q cursor-hook", winPath)); got != winPath {
		t.Fatalf("escaped Windows path parsed as %q, want %q", got, winPath)
	}
}

func TestCursorHooksDoctorReportsHealthyEnrollment(t *testing.T) {
	home := sandboxHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Enroll for real, then make the canonical binary exist.
	if _, err := EnsureCursorHooks(); err != nil {
		t.Fatal(err)
	}
	bin := cursorHookCommandBinary(cursorHookCommand())
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o700); err != nil { // #nosec G306 -- test fixture.
		t.Fatal(err)
	}

	l := onlyLine(t, CursorHooksDoctor())
	if !l.OK || l.Warn || l.Err {
		t.Fatalf("a real enrollment did not read as healthy: %+v", l)
	}
}

func TestCursorHooksDoctorReportsUnenrolled(t *testing.T) {
	home := sandboxHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	l := onlyLine(t, CursorHooksDoctor())
	if !l.Warn {
		t.Fatalf("an unenrolled machine did not warn: %+v", l)
	}
	if l.Err {
		t.Fatalf("an unenrolled machine reported an ERROR — it is a missing signal, not a broken tool: %+v", l)
	}
}

// A partial enrollment is what a hand-edited hooks.json looks like. It warns,
// naming the missing steps, rather than reading as fully enrolled.
func TestCursorHooksDoctorReportsPartialEnrollment(t *testing.T) {
	home := sandboxHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	bin := filepath.Join(home, "bin", "promptster-teams")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o700); err != nil { // #nosec G306 -- test fixture.
		t.Fatal(err)
	}
	writeCursorHooks(t, path, fmt.Sprintf("%q cursor-hook", bin), cursorHookSteps[:2])

	l := onlyLine(t, CursorHooksDoctor())
	if !l.Warn {
		t.Fatalf("a partial enrollment did not warn: %+v", l)
	}
	if !strings.Contains(l.Text, cursorHookSteps[len(cursorHookSteps)-1]) {
		t.Fatalf("the missing steps were not named: %q", l.Text)
	}
}

// A PARTIAL enrollment whose binary is also gone must still report the dangling
// command. Completeness and runnability are independent facts, and the machine
// with three of seven steps registered against a deleted binary still execs a
// missing command inside the agent loop on those three steps' events — reporting
// only "some steps are missing" there describes the least of its problems.
// Raised by review on PR #125.
func TestCursorHooksDoctorFlagsADanglingCommandUnderPartialEnrollment(t *testing.T) {
	home := sandboxHome(t)
	gone := filepath.Join(home, ".promptster-teams", "bin", "promptster-teams")
	writeCursorHooks(t, filepath.Join(home, ".cursor", "hooks.json"),
		fmt.Sprintf("%q cursor-hook", gone), cursorHookSteps[:3])

	lines := CursorHooksDoctor()
	var sawDangling, sawPartial bool
	for _, l := range lines {
		if l.Err && strings.Contains(l.Text, "does not exist") {
			sawDangling = true
		}
		if l.Warn && strings.Contains(l.Text, "only some steps") {
			sawPartial = true
		}
	}
	if !sawDangling {
		t.Fatalf("the partial enrollment hid the dangling command: %+v", lines)
	}
	if !sawPartial {
		t.Fatalf("the partial enrollment itself went unreported: %+v", lines)
	}
	// Severity order: the broken tool outranks the missing signal.
	if !lines[0].Err {
		t.Fatalf("the dangling command was not reported first: %+v", lines)
	}
}

// A hooks.json that does not parse is the state enrollment REFUSES to touch, so
// it stays unenrolled until a human fixes it — which they will never know to do
// unless doctor says so.
func TestCursorHooksDoctorReportsUnparseableConfig(t *testing.T) {
	home := sandboxHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := onlyLine(t, CursorHooksDoctor())
	if !l.Warn {
		t.Fatalf("an unparseable hooks.json did not warn: %+v", l)
	}
}

// No ~/.cursor at all is not a problem, and saying so answers "why wasn't my
// Cursor session captured" outright.
func TestCursorHooksDoctorSaysWhenCursorIsNotInstalled(t *testing.T) {
	home := sandboxHome(t)
	// sandboxHome creates ~/.cursor for the enrollment tests; this is the one
	// case that needs it gone.
	if err := os.RemoveAll(filepath.Join(home, ".cursor")); err != nil {
		t.Fatal(err)
	}
	l := onlyLine(t, CursorHooksDoctor())
	if !l.OK || l.Warn || l.Err {
		t.Fatalf("a machine without Cursor was reported as a problem: %+v", l)
	}
	if !strings.Contains(l.Text, "not installed") {
		t.Fatalf("unhelpful text for a machine without Cursor: %q", l.Text)
	}
}

// Doctor is READ-ONLY. A diagnostic that repairs the thing it is diagnosing
// cannot be trusted to describe it — and enrolling from `doctor` would write
// hooks.json on a machine whose owner only asked what was wrong.
func TestCursorHooksDoctorWritesNothing(t *testing.T) {
	home := sandboxHome(t)
	dir := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hooks.json")

	// Case 1: no hooks.json at all — doctor must not create one.
	CursorHooksDoctor()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("doctor created %s (err=%v)", path, err)
	}

	// Case 2: an existing file must come back byte-identical.
	original := []byte(`{"version":1,"hooks":{"beforeSubmitPrompt":[{"type":"command","command":"/usr/local/bin/theirs"}]}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	CursorHooksDoctor()
	after, err := os.ReadFile(path) // #nosec G304 -- test fixture path.
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("doctor rewrote hooks.json:\n got %s\nwant %s", after, original)
	}
}
