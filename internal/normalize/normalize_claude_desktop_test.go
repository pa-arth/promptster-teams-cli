package normalize

import (
	"strings"
	"testing"
)

// The stub must never emit events and never panic, whatever it's fed. Until the
// schema lands, ZERO events is the correct output for every input.

// A recognized-shape blob (has a messages array) is accepted quietly and still
// emits nothing — the parser is a stub.
func TestDesktopProcessorRecognizedShapeEmitsNothing(t *testing.T) {
	blob := []byte(`{"sessionId":"s1","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`)
	p := NewDesktopSessionProcessor("s1")
	if evs := p.Process(blob); len(evs) != 0 {
		t.Fatalf("stub must emit no events, got %d", len(evs))
	}
}

// Drift guard: a non-trivial blob that is not even valid JSON must return nil
// and not panic (the surrounding watcher relies on this to fail safe).
func TestDesktopProcessorMalformedBlobDoesNotPanic(t *testing.T) {
	// > desktopDriftGuardMinBytes of garbage so it clears the "tiny partial write"
	// silent path and actually exercises the drift guard.
	blob := []byte(strings.Repeat("this is not json ", 100))
	p := NewDesktopSessionProcessor("s1")
	if evs := p.Process(blob); evs != nil {
		t.Fatalf("malformed blob must return nil, got %d events", len(evs))
	}
}

// Drift guard: a valid JSON object with NO recognizable messages array (an
// unknown schema) must return nil and not panic.
func TestDesktopProcessorUnrecognizedShapeReturnsNil(t *testing.T) {
	blob := []byte(`{"someUnknownKey":` + `"` + strings.Repeat("x", 600) + `"}`)
	p := NewDesktopSessionProcessor("s1")
	if evs := p.Process(blob); evs != nil {
		t.Fatalf("unrecognized shape must return nil, got %d events", len(evs))
	}
}

// A tiny / truncated write (below the drift-guard floor) is skipped silently:
// nil, no panic, and it must NOT be treated as schema drift.
func TestDesktopProcessorTinyBlobSilent(t *testing.T) {
	p := NewDesktopSessionProcessor("s1")
	if evs := p.Process([]byte(`{`)); evs != nil {
		t.Fatalf("tiny partial blob must return nil, got %d events", len(evs))
	}
	if evs := p.Process([]byte("")); evs != nil {
		t.Fatalf("empty blob must return nil, got %d events", len(evs))
	}
}

// The drift guard warns at most once per processor, so a re-read of the same
// unparseable blob every 3s doesn't flood the log. We can't easily capture
// stderr here without wiring, so assert the state flag the suppression keys off.
func TestDesktopProcessorDriftGuardWarnsOnce(t *testing.T) {
	blob := []byte(strings.Repeat("nope ", 200))
	p := NewDesktopSessionProcessor("s1")
	_ = p.Process(blob)
	if !p.warned {
		t.Fatal("expected drift guard to have fired (warned=true)")
	}
	// Second read: still nil, warned stays true, no panic.
	if evs := p.Process(blob); evs != nil {
		t.Fatalf("second read must still return nil, got %d", len(evs))
	}
}

// TODO(desktop-schema): once a redacted sample local_*.json is available, add
// golden tests asserting the emitted event kinds (prompt / ai_response / command)
// and stable ids. No schema means no golden fixture yet.
