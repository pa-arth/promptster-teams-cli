package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// The beat carries the rail's health, and carries it as a MEASUREMENT at zero.
//
// A healthy machine must SAY it is healthy. The server distinguishes a reported
// state from silence, and a client that omits the field is indistinguishable
// from one too old to report — which is the state the whole fleet was in while
// this defect was live, and the reason it took a hand-inspection of one laptop
// to find it.
func TestPresenceEventCarriesTheCursorHookRail(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	writeCursorHooks(t, filepath.Join(home, ".cursor", "hooks.json"),
		installedBinary(t, home), cursorHookSteps)

	data, ok := buildPresenceEvent(Session{DeviceID: "dev-1"}).Data.(map[string]interface{})
	if !ok {
		t.Fatal("presence Data is not a map")
	}
	if got := data["cursorHooks"]; got != string(CursorHookRailOK) {
		t.Fatalf("cursorHooks = %v, want %q", got, CursorHookRailOK)
	}
	// PRESENT AND ZERO, never absent — the same rule pendingEvents learned. "We
	// repaired nothing of theirs" is a reading; a field that vanishes at zero
	// cannot be told apart from a fleet that cannot report at all.
	for _, k := range []string{"cursorHookRepairs", "cursorHookUnverifiable"} {
		got, present := data[k]
		if !present {
			t.Fatalf("%s absent — a measured zero must reach the server", k)
		}
		if got != 0 && got != float64(0) {
			t.Fatalf("%s = %v, want 0", k, got)
		}
	}
}

// A machine with no Cursor says so, rather than saying nothing.
//
// The single most useful value in the enum: it turns "this engineer captured no
// Cursor sessions" from a mystery with five identical-looking causes into a
// settled fact.
func TestPresenceEventSaysNotInstalledRatherThanNothing(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	if err := os.RemoveAll(filepath.Join(home, ".cursor")); err != nil {
		t.Fatal(err)
	}

	data := buildPresenceEvent(Session{DeviceID: "dev-1"}).Data.(map[string]interface{})
	if got := data["cursorHooks"]; got != string(CursorHookRailNotInstalled) {
		t.Fatalf("cursorHooks = %v, want %q", got, CursorHookRailNotInstalled)
	}
}

// THE TRAP THIS PINS, and it is the same one the backlog fields pinned:
// redact.ProjectEvent default-DENIES. A field the CLI sends that the allowlist
// does not name is stripped SILENTLY — the beat still returns 201 and the value
// simply never arrives, for no visible reason anywhere.
//
// That is the identical failure mode as the defect this whole change exists to
// make visible, which is why it is worth its own test rather than trusting the
// lockstep table: the lockstep test compares two ALLOWLISTS, and this one runs
// the projector.
//
// Verified to fail first: removing "cursorHooks" from the presence entry in
// internal/redact/project.go turns this red and nothing else in the suite.
func TestCursorHookFieldsSurviveTheRedactionProjector(t *testing.T) {
	for _, kind := range []string{"presence", "heartbeat"} {
		e := &event.Event{Kind: kind, Data: map[string]interface{}{
			"device":                 "dev-1",
			"cliVersion":             "0.18.1",
			"cursorHooks":            string(CursorHookRailRejected),
			"cursorHookRepairs":      2,
			"cursorHookUnverifiable": 1,
		}}
		redact.ProjectEvent(e, false)
		out, ok := e.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("%s: Data is %T after projection", kind, e.Data)
		}
		for _, k := range []string{"cursorHooks", "cursorHookRepairs", "cursorHookUnverifiable"} {
			if _, ok := out[k]; !ok {
				t.Fatalf("%s: %s stripped by the projector", kind, k)
			}
		}
	}
}

// The beat says WHAT is wrong, never WHOSE entry it is.
//
// ~/.cursor/hooks.json is shared with every other tool on the machine, so a
// reason string leaving here could carry a neighbour's command line, an
// argument, or a path. The state is a closed enum for exactly that reason, and
// this asserts the boundary on the bytes that actually leave rather than on the
// type: a value carrying the offending command would be a privacy regression the
// enum's declaration alone does not prevent.
func TestTheBeatNeverCarriesANeighboursCommand(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	writeRawHooks(t, home, `{"version":1,"hooks":{"beforeShellExecution":[
		{"type":"","command":"herdr-hook --secret-endpoint https://internal.example/ingest"}]}}`)

	data := buildPresenceEvent(Session{DeviceID: "dev-1"}).Data.(map[string]interface{})
	if got := data["cursorHooks"]; got != string(CursorHookRailRejected) {
		t.Fatalf("cursorHooks = %v, want %q", got, CursorHookRailRejected)
	}
	for k, v := range data {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if strings.Contains(s, "herdr") || strings.Contains(s, "internal.example") {
			t.Fatalf("%s = %q — the beat is carrying another tool's command line", k, s)
		}
	}
}
