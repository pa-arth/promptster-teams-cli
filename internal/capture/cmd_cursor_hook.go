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

// cursorHookStdout is the ENTIRE response this command ever writes to stdout.
//
// `stop` IS A GATING HOOK. Cursor parses the handler's stdout and, if it finds
// `followup_message`, submits that text as a new chat turn — up to
// stopHookLoopCount 5 times. So a handler that echoed its input, or printed a
// debug blob that happened to contain that key, would be DRIVING THE CUSTOMER'S
// AGENT with our telemetry. Nothing about that failure would look like a bug in
// a hook: the agent would simply appear to talk to itself.
//
// Containment is structural rather than a review rule: the response is a
// compile-time constant, no part of the payload is ever serialised to stdout,
// and it is written from RunCursorHook — outside the goroutine — so a budget
// overrun answers with the same constant rather than with nothing.
const cursorHookStdout = `{"continue": true}`

// RunCursorHook is the `cursor-hook` subcommand: Cursor's registered command.
// It reads one JSON payload on stdin, normalizes it, and queues the events.
//
// FOUR INVARIANTS, IN DESCENDING ORDER OF HOW BADLY YOU WOULD REGRET BREAKING
// THEM:
//
//  0. IT NEVER PRINTS ANYTHING DERIVED FROM THE PAYLOAD. See cursorHookStdout.
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
		//
		// Counted, because this line goes to a stderr Cursor discards and the
		// abandoned work leaves no other trace: an overrun and an idle engineer
		// were indistinguishable from anywhere off the machine. The counter is
		// deliberately NOT in cursor-generations.json — that file's lock is the
		// leading candidate for what wedged the goroutine, and blocking on it
		// here would defeat the budget while reporting it. See
		// cursor_hook_overruns.go.
		recordCursorHookOverrun()
		fmt.Fprintln(os.Stderr, "promptster-teams: cursor hook exceeded its time budget, skipping")
	}
	// Both paths answer identically. A constant cannot say followup_message.
	fmt.Fprintln(os.Stdout, cursorHookStdout)
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
	// THE HOOK'S CWD IS CURSOR'S, NOT OURS — SO IT MUST NOT DECIDE THE PATH SPACE.
	//
	// loadSession falls back to os.Getwd() for TaskRoot, which is right for a
	// process the engineer started and wrong for one Cursor spawns: Cursor picks
	// the cwd, and it is not the daemon's workspace. That single value decides two
	// things downstream — RelativizeEventPaths' base, and the rootKey/relPath
	// recordAiTouchedPath writes — so getting it wrong does not merely mislabel a
	// path, it writes ledger keys in a DIFFERENT SPACE from the one the git
	// watcher reads back (which is anchored to the daemon's workspace). The lookup
	// in reconcileCommitAttribution then misses every time, and Cursor edits
	// attribute as unknown/human: no likely_ai, no ai_revised_by_human, no
	// durability. Silently, because a missing ledger hit is indistinguishable from
	// a human edit.
	//
	// Measured before the fix: 35 of 40 absolute Cursor file paths were under HOME
	// and should have relativized, against 0 of 33 for claude-code, whose watcher
	// IS the daemon and therefore already had the right root.
	//
	// The daemon persists its workspace for exactly this reason — see WatchDir on
	// claudeWatcherState, added because `status` "falls back to its own cwd, which
	// is routinely wrong". Same failure, same fix, one rail later.
	session.TaskRoot = cursorHookTaskRoot(session.TaskRoot)

	redacted := redact.RedactBytes(raw)
	// The model join: `stop` reports the routing sentinel, so the tokens it
	// carries reach the normalizer alongside whatever afterAgentThought recorded
	// for the SAME generation. Reading is a file stat and a parse of a bounded
	// file; it happens on `stop` alone, once per turn.
	res, ok := normalize.NormalizeCursorHook(redacted, normalize.CursorHookOptions{
		ResolveModel: cursorGenerationModel,
	})
	// The cache write comes BEFORE the ok check, deliberately. afterAgentThought
	// now emits no event of its own, so `ok` is false for exactly the payload
	// whose only product is this entry — returning early on it would leave every
	// usage row modelless while every individual piece looked correct.
	if res.Step == "afterAgentThought" {
		recordCursorGenerationModel(res.GenerationID, res.Model)
	}
	if !ok {
		// COUNT THE DROP BEFORE RETURNING. Every counter this rail had was on the
		// far side of this return, so a `stop` that produced nothing incremented
		// nothing — the one outcome worth measuring was the one outcome that left
		// no record. 38% of one live machine's turns went missing this way with
		// no evidence on the device for a single one of them.
		recordCursorHookDrop(res)
		return
	}

	// Stamp the repo identity, exactly as the transcript rail does. This rail
	// CLAIMS the session away from the watcher, so skipping it would silently
	// lose repo attribution for every Cursor session on an enrolled machine.
	//
	// The git resolution is the only disk work on this path beyond the ledger,
	// and it happens on `beforeSubmitPrompt` alone — once per turn, not per tool
	// call — which is why it fits inside cursorHookBudget.
	if res.Workdir != "" {
		root, host, tracked := sessionRepoIdentity(res.Workdir)
		normalize.StampCursorHookRepoIdentity(
			res.Events, normalize.CursorSessionWorkdir(res.Workdir), root, host, tracked)
	}

	// THERE IS NO LONGER A REPEAT-MODEL SUPPRESSION HERE, AND REINTRODUCING ONE
	// WOULD DELETE SPEND. It existed because afterAgentThought minted an
	// ai_response carrying the model, many times per turn, all identical. That
	// event is gone; the only ai_response this rail emits now is one turn's usage
	// row, and consecutive turns of one session legitimately report the same
	// model. A suppression keyed on (session, model) would drop every turn after
	// the first — silently, since the surviving row looks perfectly healthy.
	events := res.Events

	// Count what the coverage probe measured, so the number outlives the probe.
	// The SAME call books the drop above, with an empty event set — one function,
	// both outcomes, so the ratio it reports cannot be booked inconsistently.
	if res.Step == "stop" {
		recordCursorStopOutcome(res.SessionID, res.Model, events)
	}

	// captureProse=false unconditionally. The resolver does a network fetch, and
	// invariant 2 forbids network I/O on this path; more to the point, this rail
	// emits no assistant prose at all — the one prose field it carries is the
	// engineer's own prompt, which is not gated by that policy.
	queued := 0
	for _, ev := range events {
		queued += emitCursorEvent(ev, session, false)
	}

	// CLAIM ONLY AFTER SOMETHING WAS DURABLY QUEUED, AND ONLY THEN.
	//
	// The claim tells the transcript watcher to stand down and advance that
	// transcript to EOF. Claiming first — which this did originally, on the
	// reasoning that a crash in between would leave the session "captured only by
	// the watcher" — has it exactly backwards: the claim is what STOPS the
	// watcher, so a claim followed by a failed enqueue means the session is
	// captured by neither rail and the transcript is seeked past. A full outbox,
	// a read-only state dir or a signing failure would silently erase the
	// session, and those are ordinary conditions, not crashes.
	//
	// Ordering it this way inverts the residual risk into the survivable
	// direction: a kill between the enqueue and the claim leaves the watcher
	// covering records the hook already sent, which costs duplicates rather than
	// data. Given the choice, duplicate beats gone.
	if queued > 0 && res.TranscriptPath != "" {
		recordCursorHookClaim(res.TranscriptPath, res.SessionID)
	}
}

// recordCursorHookDrop books a normalization failure against the step that
// caused it.
//
// Its own function for the reason cursorHookTaskRoot is: the call site is one
// line and the interesting behaviour is entirely here, so the DISPATCH is
// testable without standing up a whole hook run. That matters more than usual
// here — the defect being fixed was a call in the wrong place, not a wrong
// calculation, so a test that exercised only the recorders would pass against
// an implementation that had stopped calling them.
//
// TWO NAMED OUTCOMES, AND SILENCE FOR THE REST. A `stop` here is an EMPTY turn:
// usageEvent declined it because Cursor reported no token counts and no model
// resolved, which is an honest drop of a turn we were told nothing about. An
// empty Step is a payload that never parsed at all.
//
// Every other `!ok` is deliberately not counted — afterAgentThought, which emits
// nothing by design and whose only product is the model cache entry; an
// unregistered step; a prompt step with no prompt. None of those is a drop of
// spend, and a counter that mixes "nothing to say" with "we lost a turn" is the
// exact ambiguity this instrument exists to remove.
func recordCursorHookDrop(res normalize.CursorHookResult) {
	switch res.Step {
	case "stop":
		recordCursorStopOutcome(res.SessionID, res.Model, nil)
	case "":
		recordCursorHookUnparsed()
	}
}

// cursorHookTaskRoot picks the workspace this hook invocation writes paths
// against: the daemon's recorded workspace when there is one, else the caller's
// value (loadSession's env-or-cwd). Kept as its own function so the choice is
// testable without standing up a whole hook run — the call site is one line and
// the interesting behaviour is entirely here.
func cursorHookTaskRoot(fallback string) string {
	if root := daemonWatchRoot(); root != "" {
		return root
	}
	return fallback
}

// daemonWatchRoot reports the workspace the local daemon is scoped to, or "" if
// there is nothing recorded to trust.
//
// Read from the watcher state the daemon writes, cursor rail first and claude
// rail second. Both are written by the SAME process with the same TaskRoot
// (autostart runs one `watch`), so the second is a plain redundancy for the case
// where the cursor watcher has not yet had its first heartbeat.
//
// Deliberately NOT gated on the recorded pid being alive. A stale entry still
// names the workspace every existing ai-paths key was written under, so honouring
// it keeps the hook in the ledger's key space; falling back to cwd because the
// daemon is momentarily down would start writing keys nothing can ever read. The
// value is a path space, not a liveness claim.
func daemonWatchRoot() string {
	if s, err := loadCursorWatcherState(); err == nil && s.WatchDir != "" {
		return s.WatchDir
	}
	if s, err := loadClaudeWatcherState(); err == nil && s.WatchDir != "" {
		return s.WatchDir
	}
	return ""
}

// EnsureCursorHooksBestEffort installs the user-scope hook entries and reports
// what happened, never failing its caller.
//
// This is the automatic-migration entry point: an already-installed fleet
// self-updates within ~30m and re-execs, this runs at the new binary's watch
// startup, and the engineer does nothing at all. It is idempotent, so running it
// on every start costs one stat and one parse when nothing has changed.
func EnsureCursorHooksBestEffort() {
	changed, repairs, err := ensureCursorHooks()
	for _, r := range repairs {
		// ALWAYS printed, never gated on verbose: this is us editing a file we do
		// not own, to undo damage we did. The persistent record is in
		// cursorHookRepairLogPath() and `status` reports it, because a line on a
		// daemon's stderr is exactly the kind of signal nobody reads — which is
		// how this defect survived two days in the first place.
		fmt.Fprintf(os.Stderr, "promptster-teams: repaired %s in %s — %s (%s)\n",
			r.Step, cursorUserHooksPath(), r.Action, r.Reason)
	}
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
