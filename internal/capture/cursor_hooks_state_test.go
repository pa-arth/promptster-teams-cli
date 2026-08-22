package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// railFixture builds one machine's worth of ~/.cursor state and returns the
// state both readers must agree on.
type railFixture struct {
	name  string
	want  CursorHookRailState
	build func(t *testing.T, home string)
}

// installedBinary writes an executable-shaped file and returns the hook command
// that points at it, so a fixture can be enrolled and RUNNABLE.
func installedBinary(t *testing.T, home string) string {
	t.Helper()
	bin := filepath.Join(home, "bin", "promptster-teams")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o700); err != nil { // #nosec G306 -- test fixture that must be executable-shaped.
		t.Fatal(err)
	}
	return fmt.Sprintf("%q cursor-hook", bin)
}

// writeRawHooks writes a hooks.json body verbatim, so a fixture can express a
// file no serializer of ours would produce — which is exactly the case that
// started this: the entry Cursor rejects was written by another tool.
func writeRawHooks(t *testing.T, home, body string) {
	t.Helper()
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func railFixtures() []railFixture {
	return []railFixture{{
		name: "no ~/.cursor at all",
		want: CursorHookRailNotInstalled,
		build: func(t *testing.T, home string) {
			// sandboxHome creates ~/.cursor because enrollment no-ops without it.
			// This is the one fixture that needs it gone.
			if err := os.RemoveAll(filepath.Join(home, ".cursor")); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		name: "hooks.json is not JSON",
		want: CursorHookRailUnreadable,
		build: func(t *testing.T, home string) {
			writeRawHooks(t, home, "{not json")
		},
	}, {
		// THE STATE THIS WHOLE CHANGE EXISTS FOR. A type-less entry belonging to
		// another tool, which Cursor rejects the whole file over. Note it is
		// enrolled for every one of our steps and the binary is present: without
		// the whole-file verdict this machine reads as perfectly healthy, which
		// is precisely how the outage stayed invisible.
		name: "a neighbour's entry Cursor throws the file away over",
		want: CursorHookRailRejected,
		build: func(t *testing.T, home string) {
			cmd := installedBinary(t, home)
			hooks := map[string][]any{}
			for _, s := range cursorHookSteps {
				hooks[s] = []any{map[string]any{"type": "command", "command": cmd}}
			}
			hooks[cursorHookSteps[0]] = append(hooks[cursorHookSteps[0]],
				map[string]any{"type": "", "command": "herdr-hook"})
			body, err := json.Marshal(map[string]any{"version": 1, "hooks": hooks})
			if err != nil {
				t.Fatal(err)
			}
			writeRawHooks(t, home, string(body))
		},
	}, {
		name: "enrolled against a binary that was deleted",
		want: CursorHookRailDangling,
		build: func(t *testing.T, home string) {
			gone := filepath.Join(home, ".promptster-teams", "bin", "promptster-teams")
			writeCursorHooks(t, filepath.Join(home, ".cursor", "hooks.json"),
				fmt.Sprintf("%q cursor-hook", gone), cursorHookSteps)
		},
	}, {
		name: "Cursor installed, none of our steps registered",
		want: CursorHookRailUnenrolled,
		build: func(t *testing.T, home string) {
			writeRawHooks(t, home, `{"version":1,"hooks":{}}`)
		},
	}, {
		name: "some steps registered, some not",
		want: CursorHookRailPartial,
		build: func(t *testing.T, home string) {
			cmd := installedBinary(t, home)
			writeCursorHooks(t, filepath.Join(home, ".cursor", "hooks.json"),
				cmd, cursorHookSteps[:1])
		},
	}, {
		name: "enrolled everywhere and runnable",
		want: CursorHookRailOK,
		build: func(t *testing.T, home string) {
			cmd := installedBinary(t, home)
			writeCursorHooks(t, filepath.Join(home, ".cursor", "hooks.json"), cmd, cursorHookSteps)
		},
	}}
}

// Every state in the enum is reachable from a file on disk.
//
// A closed enum is only worth having if each word means something a machine can
// actually be in. A value nothing can produce is not a state, it is a comment
// with a type — and the backend refuses unrecognised words, so a word that can
// never arrive silently narrows what the fleet can say.
func TestInspectCursorHookRailNamesEachState(t *testing.T) {
	seen := map[CursorHookRailState]bool{}
	for _, f := range railFixtures() {
		t.Run(f.name, func(t *testing.T) {
			home := sandboxHome(t)
			t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
			f.build(t, home)

			if got := InspectCursorHookRail().State; got != f.want {
				t.Fatalf("state = %q, want %q", got, f.want)
			}
		})
		seen[f.want] = true
	}
	for _, s := range []CursorHookRailState{
		CursorHookRailNotInstalled, CursorHookRailUnreadable, CursorHookRailRejected,
		CursorHookRailDangling, CursorHookRailUnenrolled, CursorHookRailPartial,
		CursorHookRailOK,
	} {
		if !seen[s] {
			t.Errorf("no fixture produces %q — an unreachable state is not a state", s)
		}
	}
}

// The doctor and the beat must not tell an engineer and a dashboard different
// stories about one machine.
//
// PROMISED BY NAME in cursor_hooks_state.go, and the reason is the ordering
// rather than the wording: both readers rank rejected over partial and dangling
// over partial, and they do it in two separate walks of the same file. Nothing
// but this test stops one of those walks being reordered alone — after which a
// manager sees `partial` on a machine whose engineer is being told, correctly,
// that Cursor is throwing the entire file away.
//
// It pins the SEVERITY CLASS, not the prose. The doctor's text is free to change
// (it is a sentence for a human); what may not drift is which of the two readers
// thinks something is wrong.
func TestDoctorAndRailStateAgree(t *testing.T) {
	// worstOf reduces the doctor's report to the same three-way judgement the
	// state word carries: broken / degraded / fine.
	worstOf := func(lines []CursorHookDoctorLine) string {
		out := "ok"
		for _, l := range lines {
			switch {
			case l.Err:
				return "err"
			case l.Warn:
				out = "warn"
			}
		}
		return out
	}
	class := map[CursorHookRailState]string{
		CursorHookRailNotInstalled: "ok",
		CursorHookRailOK:           "ok",
		CursorHookRailUnreadable:   "warn",
		CursorHookRailUnenrolled:   "warn",
		CursorHookRailPartial:      "warn",
		CursorHookRailRejected:     "err",
		CursorHookRailDangling:     "err",
	}

	for _, f := range railFixtures() {
		t.Run(f.name, func(t *testing.T) {
			home := sandboxHome(t)
			t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
			f.build(t, home)

			state := InspectCursorHookRail().State
			if got, want := worstOf(CursorHooksDoctor()), class[state]; got != want {
				t.Fatalf("state %q is class %q, but doctor reports %q:\n%+v",
					state, want, got, CursorHooksDoctor())
			}
		})
	}
}

// `ok` is "nothing PROVABLY wrong", and where the unverifiable count is non-zero
// that is a weaker claim than the word sounds.
//
// The counter is the honesty term on the state, and it exists because the
// validator refuses to condemn a hook type it does not recognise — which is the
// right call (a static support map has no subscriber to Cursor's release notes)
// and is also exactly how a rejected file could read as `ok`. If Cursor IS
// throwing this file away, these are the entries to look at.
func TestUnverifiableEntriesRideAlongsideOK(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	cmd := installedBinary(t, home)
	hooks := map[string][]any{}
	for _, s := range cursorHookSteps {
		hooks[s] = []any{map[string]any{"type": "command", "command": cmd}}
	}
	hooks[cursorHookSteps[0]] = append(hooks[cursorHookSteps[0]],
		map[string]any{"type": "afterSomethingCursorAddedLater", "command": "x"})
	body, err := json.Marshal(map[string]any{"version": 1, "hooks": hooks})
	if err != nil {
		t.Fatal(err)
	}
	writeRawHooks(t, home, string(body))

	rep := InspectCursorHookRail()
	if rep.State != CursorHookRailOK {
		t.Fatalf("an unrecognised hook type must not be condemned: state = %q", rep.State)
	}
	if rep.Unverifiable != 1 {
		t.Fatalf("Unverifiable = %d, want 1 — the OK claim would overstate itself", rep.Unverifiable)
	}
}

// A repaired machine says so, and keeps saying so.
//
// "Did the v0.18.1 repair actually run here" is the fleet question this beacon
// was built to answer, and it is answered by a CUMULATIVE count rather than by a
// row per beat. That only works if the count survives the repair succeeding —
// the file is correct afterwards and nothing on disk admits we changed it, which
// is the same silence the original defect was made of.
func TestRepairsAreCountedAfterTheFileIsHealthyAgain(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	// Installed AT THE MANAGED PATH, because enrollment re-renders every entry to
	// point there. A binary anywhere else would leave this machine `dangling`
	// after a perfectly successful repair, which is enrollment behaving correctly
	// and would make the assertion below a test of the fixture.
	bin := state.CanonicalInstallBin()
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o700); err != nil { // #nosec G306 -- test fixture that must be executable-shaped.
		t.Fatal(err)
	}
	cmd := fmt.Sprintf("%q cursor-hook", bin)
	hooks := map[string][]any{}
	for _, s := range cursorHookSteps {
		hooks[s] = []any{map[string]any{"type": "command", "command": cmd}}
	}
	hooks[cursorHookSteps[0]] = append(hooks[cursorHookSteps[0]],
		map[string]any{"type": "", "command": "herdr-hook"})
	body, err := json.Marshal(map[string]any{"version": 1, "hooks": hooks})
	if err != nil {
		t.Fatal(err)
	}
	writeRawHooks(t, home, string(body))

	if got := InspectCursorHookRail(); got.State != CursorHookRailRejected || got.Repairs != 0 {
		t.Fatalf("before repair: %+v, want rejected with 0 repairs", got)
	}
	if _, _, err := ensureCursorHooks(); err != nil {
		t.Fatal(err)
	}

	rep := InspectCursorHookRail()
	if rep.State != CursorHookRailOK {
		t.Fatalf("after repair: state = %q, want ok", rep.State)
	}
	if rep.Repairs != 1 {
		t.Fatalf("after repair: Repairs = %d, want 1 — a repair that leaves no trace is invisible to the fleet", rep.Repairs)
	}
}
