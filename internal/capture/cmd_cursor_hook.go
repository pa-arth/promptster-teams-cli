package capture

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// cursorHookMaxPayload bounds what we read from stdin. An afterFileEdit payload
// carries every edit's old_string and new_string, so a large refactor can make
// the payload megabytes. We only count their lines, but we still have to read
// them, and an unbounded read on a blocking hook is how a hook becomes a hang.
const cursorHookMaxPayload = 8 << 20 // 8 MiB

// cursorHookBudget is the wall-clock ceiling for the whole invocation.
//
// A HOOK RUNS SYNCHRONOUSLY INSIDE THE ENGINEER'S AGENT LOOP. Cursor waits for
// this process. Every millisecond spent here is a millisecond the engineer's
// agent is stalled, and a wedged hook is a wedged agent. That is the one way this
// rail can hurt someone in a way the transcript watcher never could, so the
// budget is small and enforced rather than assumed.
const cursorHookBudget = 2 * time.Second

// RunCursorHook is the `cursor-hook` subcommand: Cursor's registered command.
// It reads one JSON payload on stdin, normalizes it, and queues the events.
//
// THREE INVARIANTS, IN DESCENDING ORDER OF HOW BADLY YOU WOULD REGRET BREAKING
// THEM:
//
//  1. IT ALWAYS EXITS 0 AND IT NEVER BLOCKS FOR LONG. It is inside the
//     engineer's agent loop (see cursorHookBudget). A non-zero exit or a stall
//     degrades their tool to collect telemetry, which is never a trade worth
//     making. Errors go to stderr, which Cursor discards, and the transcript
//     watcher independently captures the same session anyway — so a failure here
//     costs metadata richness, not capture.
//
//  2. IT DOES NO NETWORK I/O. Events go to the durable outbox and the resident
//     daemon ships them. A hook that waited on an HTTP round trip would put the
//     ingest endpoint's latency — and its outages — directly into the engineer's
//     agent loop.
//
//  3. IT REDACTS BEFORE IT PARSES. redact.RedactBytes runs on the raw payload,
//     matching the raw-JSON-before-parse ordering the other rails use. The
//     payload contains file bodies, command output and the engineer's email
//     address; parsing first would mean a struct field holding a secret before
//     anything had a chance to scrub it.
func RunCursorHook() error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCursorHookInner()
	}()
	select {
	case <-done:
	case <-time.After(cursorHookBudget):
		// Budget blown. Abandon the work and return; the transcript rail still
		// covers this session. Do NOT wait for the goroutine — waiting is the
		// thing the budget exists to prevent.
		fmt.Fprintln(os.Stderr, "promptster-teams: cursor hook exceeded its time budget, skipping")
	}
	return nil
}

func runCursorHookInner() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, cursorHookMaxPayload))
	if err != nil || len(raw) == 0 {
		return
	}

	session, err := loadSession()
	if err != nil {
		// Not logged in, or no key yet. Silent: Cursor would fire this on every
		// event and a chatty hook on an unconfigured machine is pure noise.
		return
	}

	redacted := redact.RedactBytes(raw)
	res, ok := normalize.NormalizeCursorHook(redacted)
	if !ok {
		return
	}

	// Claim the session for the hook rail so the transcript watcher skips it.
	// The payload names the exact transcript file, so this is an identity, not a
	// heuristic. Recorded BEFORE emitting: if we crash between the two, the worst
	// case is a session captured only by the watcher — thinner, never doubled.
	// Drop a model we have already reported for this session BEFORE claiming, so
	// the claim's model field still reflects what was actually queued.
	events := res.Events
	if cursorHookModelAlreadyReported(res.TranscriptPath, res.SessionID, res.Model) {
		kept := events[:0]
		for _, ev := range events {
			if ev.Kind == "ai_response" {
				continue
			}
			kept = append(kept, ev)
		}
		events = kept
	}
	if res.TranscriptPath != "" {
		recordCursorHookClaim(res.TranscriptPath, res.SessionID, res.Model)
	}

	// captureProse=false unconditionally. The resolver does a network fetch, and
	// invariant 2 forbids network I/O on this path; more to the point, this rail
	// emits no assistant prose at all — the one prose field it carries is the
	// engineer's own prompt, which is not gated by that policy.
	for _, ev := range events {
		emitCursorEvent(ev, session, false)
	}
}

// EnsureCursorHooksBestEffort installs the user-scope hook entries and reports
// what happened, never failing its caller.
//
// This is the automatic-migration entry point: an already-installed fleet
// self-updates within ~30m and re-execs, this runs at the new binary's watch
// startup, and the engineer does nothing at all. It is idempotent, so running it
// on every start costs one stat and one parse when nothing has changed.
func EnsureCursorHooksBestEffort() {
	changed, err := EnsureCursorHooks()
	if err != nil {
		// Loud enough to diagnose, never fatal. The most likely cause is an
		// engineer's own hooks.json that does not parse — which we refuse to
		// overwrite (see loadCursorHookConfig).
		fmt.Fprintf(os.Stderr, "promptster-teams: could not enroll Cursor hooks (%v) — continuing with transcript capture only\n", err)
		return
	}
	if changed && verboseWatch() {
		fmt.Fprintf(os.Stderr, "promptster-teams: enrolled Cursor hooks in %s\n", cursorUserHooksPath())
	}
}
