package capture

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// findBashWindow returns the first window recorded for sessionID, or false.
func findBashWindow(windows []bashWindow, sessionID string) (bashWindow, bool) {
	for _, w := range windows {
		if w.SessionID == sessionID {
			return w, true
		}
	}
	return bashWindow{}, false
}

// TestRecordAndReadBashWindows mirrors TestReadAiTouchedPaths: two sessions each
// record a window, and the reader must return each window tagged with the
// session that recorded it.
func TestRecordAndReadBashWindows(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	recordBashWindow("sess-A", "", 1000, 2000)
	recordBashWindow("sess-B", "", 5000, 6000)

	got := readBashWindows("")
	a, ok := findBashWindow(got, "sess-A")
	if !ok {
		t.Fatalf("sess-A window missing: %+v", got)
	}
	if a.StartMs != 1000 || a.EndMs != 2000 {
		t.Errorf("sess-A window = %d..%d, want 1000..2000", a.StartMs, a.EndMs)
	}
	b, ok := findBashWindow(got, "sess-B")
	if !ok {
		t.Fatalf("sess-B window missing: %+v", got)
	}
	if b.StartMs != 5000 || b.EndMs != 6000 {
		t.Errorf("sess-B window = %d..%d, want 5000..6000", b.StartMs, b.EndMs)
	}
}

// TestReadBashWindowsExpiresByTTL ages one session's timestamp past aiPathsTTL on
// disk and asserts the reader drops its windows while keeping the fresh session's
// — same TTL discipline as the ai-paths ledger.
func TestReadBashWindowsExpiresByTTL(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	recordBashWindow("fresh", "", 1000, 2000)
	recordBashWindow("stale", "", 3000, 4000)

	// Rewrite the "stale" session's timestamp to well beyond the TTL window.
	data, err := os.ReadFile(bashWindowsLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	var ledger bashWindowsLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	e := ledger.Sessions["stale"]
	e.TsMs = time.Now().Add(-aiPathsTTL - time.Hour).UnixMilli()
	ledger.Sessions["stale"] = e
	out, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bashWindowsLedgerPath(), out, 0o600); err != nil {
		t.Fatal(err)
	}

	got := readBashWindows("")
	if _, ok := findBashWindow(got, "stale"); ok {
		t.Error("window from an expired session must be excluded by TTL")
	}
	if _, ok := findBashWindow(got, "fresh"); !ok {
		t.Error("fresh session window must survive")
	}
}

// TestReadBashWindowsWorkspaceScoping: windows recorded under different root keys
// are separated by a scoped read, and an unscoped read returns all — mirrors the
// ai-paths ledger scoping.
func TestReadBashWindowsWorkspaceScoping(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	recordBashWindow("sess-A", "keyA", 1000, 2000)
	recordBashWindow("sess-B", "keyB", 5000, 6000)

	a := readBashWindows("keyA")
	if _, ok := findBashWindow(a, "sess-A"); !ok {
		t.Errorf("keyA scope must include sess-A: %+v", a)
	}
	if _, ok := findBashWindow(a, "sess-B"); ok {
		t.Errorf("keyA scope must exclude sess-B (recorded under keyB): %+v", a)
	}

	b := readBashWindows("keyB")
	if _, ok := findBashWindow(b, "sess-B"); !ok {
		t.Errorf("keyB scope must include sess-B: %+v", b)
	}
	if _, ok := findBashWindow(b, "sess-A"); ok {
		t.Errorf("keyB scope must exclude sess-A: %+v", b)
	}

	all := readBashWindows("")
	if _, ok := findBashWindow(all, "sess-A"); !ok {
		t.Errorf("unscoped read must include sess-A: %+v", all)
	}
	if _, ok := findBashWindow(all, "sess-B"); !ok {
		t.Errorf("unscoped read must include sess-B: %+v", all)
	}
}

// TestRecordAiBashWindowOnlyAICommands feeds command events through the recorder
// exactly as the watcher queue does, and asserts ONLY the AI-attributed one lands
// a window (human/unknown bash never records). The stored window is the observed
// END time as a point [end, end]; the δ/ε tolerance is applied later at recovery.
func TestRecordAiBashWindowOnlyAICommands(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	ts := "2026-07-17T12:00:00.000Z"
	endMs := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC).UnixMilli()

	ai := event.NewEvent("command", "ai-sess")
	ai.Ts = ts
	ai.Provenance = event.AIProvenance()
	recordAiBashWindow(&ai, "")

	human := event.NewEvent("command", "human-sess")
	human.Ts = ts
	human.Provenance = event.HumanProvenance()
	recordAiBashWindow(&human, "")

	noProv := event.NewEvent("command", "noprov-sess")
	noProv.Ts = ts
	recordAiBashWindow(&noProv, "")

	// A non-command AI event must never record a bash window.
	fileDiff := event.NewEvent("file_diff", "ai-sess")
	fileDiff.Ts = ts
	fileDiff.Provenance = event.AIProvenance()
	recordAiBashWindow(&fileDiff, "")

	got := readBashWindows("")
	if len(got) != 1 {
		t.Fatalf("want exactly one recorded window (the AI command), got %d: %+v", len(got), got)
	}
	w, ok := findBashWindow(got, "ai-sess")
	if !ok {
		t.Fatalf("AI command window missing: %+v", got)
	}
	// Only the command END time is observable, so the stored window is the point
	// [end, end]; tolerance is a recovery-time concern, not baked in here.
	if w.StartMs != endMs || w.EndMs != endMs {
		t.Errorf("window = %d..%d, want observed end point %d..%d", w.StartMs, w.EndMs, endMs, endMs)
	}
}
