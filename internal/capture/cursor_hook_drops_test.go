package capture

import (
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
)

// The drop this rail could not see.
//
// A `stop` whose turn was aborted carries no token counts and resolves no model,
// so usageEvent declines it and NormalizeCursorHook answers false. Every counter
// this rail had was on the far side of the resulting early return, so the one
// outcome worth measuring was the one outcome that incremented nothing: 38% of a
// live machine's Cursor turns went missing with no evidence on the device for a
// single one of them.
//
// The assertion is on the DENOMINATOR moving. A test that only checked
// StopEmpty would pass against an implementation that counted the drop and
// forgot the turn, which is the same hole one bucket smaller.
func TestAbortedStopIsCountedRatherThanDroppedSilently(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	// No input_tokens, no output_tokens, no cache counts, no model_id — the shape
	// Cursor sends for a turn the engineer interrupted.
	stop := []byte(`{"hook_event_name":"stop","conversation_id":"c1",
		"generation_id":"3a8b6e45-a04d-4bca-8922-617844887809","status":"aborted"}`)
	res, ok := normalize.NormalizeCursorHook(stop, normalize.CursorHookOptions{ResolveModel: cursorGenerationModel})
	if ok {
		t.Fatalf("aborted stop produced %d events, want none", len(res.Events))
	}
	if res.Step != "stop" {
		t.Fatalf("step = %q — the drop is only attributable because the step survives the failure", res.Step)
	}
	recordCursorHookDrop(res)

	c := loadCursorGenerations()
	if c.StopSeen != 1 || c.StopEmpty != 1 || c.UsageRows != 0 {
		t.Fatalf("stopSeen/stopEmpty/usageRows = %d/%d/%d, want 1/1/0",
			c.StopSeen, c.StopEmpty, c.UsageRows)
	}
	// The residual is what says the drop was EXPLAINED rather than merely counted:
	// seen - empty - usageRows is the bucket for a stop that parsed, was not
	// empty, and still produced nothing, and that one is our defect.
	if lost := c.StopSeen - c.StopEmpty - c.UsageRows; lost != 0 {
		t.Fatalf("residual = %d, want 0 — an aborted turn must land in StopEmpty, not in the unexplained bucket", lost)
	}
}

// A payload that never names a step. Everything else here is conditional on a
// parse, so this is the counter that says whether the others describe the
// traffic or a subset of it.
func TestUnparsedPayloadIsCountedAndDoesNotTouchTheStopRatio(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	res, ok := normalize.NormalizeCursorHook([]byte(`{"hook_event_name":`), normalize.CursorHookOptions{})
	if ok || res.Step != "" {
		t.Fatalf("malformed payload resolved step %q, ok=%v", res.Step, ok)
	}
	recordCursorHookDrop(res)

	c := loadCursorGenerations()
	if c.Unparsed != 1 {
		t.Fatalf("unparsed = %d, want 1", c.Unparsed)
	}
	// It must NOT inflate the stop denominator. A payload we could not read is
	// not evidence that a turn happened, and folding it in would make the
	// coverage ratio understate itself for a reason unrelated to coverage.
	if c.StopSeen != 0 || c.StopEmpty != 0 {
		t.Fatalf("stopSeen/stopEmpty = %d/%d, want 0/0 — an unparsed payload is not a stop",
			c.StopSeen, c.StopEmpty)
	}
}

// afterAgentThought answers false by design — it emits no event at all, its only
// product is the model cache entry. It must never be counted as a drop, because a
// counter that mixes "nothing to say" with "we lost a turn" is the ambiguity this
// whole instrument exists to remove.
func TestThoughtIsNotADrop(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	thought := []byte(`{"hook_event_name":"afterAgentThought","conversation_id":"c1",
		"generation_id":"3a8b6e45-a04d-4bca-8922-617844887809-3-1v0i","model_id":"composer-2.5","text":"reasoning"}`)
	res, ok := normalize.NormalizeCursorHook(thought, normalize.CursorHookOptions{ResolveModel: cursorGenerationModel})
	if ok {
		t.Fatalf("thought emitted %d events, want none", len(res.Events))
	}
	// The REAL dispatch, not a copy of it: neither branch fires for this step.
	recordCursorHookDrop(res)

	c := loadCursorGenerations()
	if c.StopSeen != 0 || c.StopEmpty != 0 || c.Unparsed != 0 {
		t.Fatalf("thought booked seen/empty/unparsed = %d/%d/%d, want 0/0/0",
			c.StopSeen, c.StopEmpty, c.Unparsed)
	}
}

// THE REASON THE OVERRUN COUNTER LIVES IN ITS OWN FILE.
//
// RunCursorHook abandons its worker goroutine at cursorHookBudget and returns
// without waiting — waiting is the thing the budget exists to prevent. The
// goroutine is left running, and it may be running precisely because it is
// blocked inside sign.WithBufferLock on cursor-generations.json, which is a
// BLOCKING flock with no timeout.
//
// So recording the overrun through that same lock would park the parent on the
// very wedge it is reporting, past the budget, for as long as the wedge lasts:
// the budget enforced in name and defeated in fact, producing exactly the
// stalled agent it exists to prevent. This test holds that lock and requires the
// overrun to be booked anyway.
//
// It is a DESIGN assertion, not a behavioural one — it fails the moment someone
// moves this counter into cursorGenerations "so the counters live together",
// which is the tidy-looking change that would reintroduce the hang.
func TestOverrunIsRecordedWhileTheGenerationsLockIsHeld(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = sign.WithBufferLock(cursorGenerationsPath()+".lock", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	done := make(chan struct{})
	go func() {
		defer close(done)
		recordCursorHookOverrun()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("recordCursorHookOverrun blocked on the generations lock — the budget is defeated by the counter that reports it")
	}
	close(release)

	if o := loadCursorHookOverruns(); o.Overruns != 1 {
		t.Fatalf("overruns = %d, want 1", o.Overruns)
	}
}
