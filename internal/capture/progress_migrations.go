package capture

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// A PROGRESS-SCHEMA BUMP IS A FLEET-WIDE REPLAY EVENT, AND ITS COST IS DECLARED
// HERE RATHER THAN IMPLIED BY A `p.V < N` COMPARISON.
//
// This file exists because of #140. Bumping `claudeProgressSchemaV` to 2 was one
// line of diff. It cleared `Offsets`, so every in-window transcript on every
// device was re-read from zero: 62,302 replayed events on one device, a
// 20,761-deep delivery backlog whose oldest entry was 15 days stale, and a
// dashboard reporting that engineer idle. None of that was visible in the diff,
// none of it landed on whoever wrote it, and all of it landed days later when
// each device next started the upgraded binary.
//
// The fix is not "be careful". It is to make the cost a REQUIRED FIELD:
//
//   - `claudeProgressSchemaV` / `codexProgressSchemaV` are DERIVED from these
//     tables, so a version cannot be bumped without adding a row.
//   - A migration that clears offsets must declare a `ReplayHorizon`. The build
//     check in progress_migrations_check_test.go parses the loaders' AST and
//     fails when an offset-clearing `p.V < N` block has no row, or a row with no
//     horizon. That is the check that would have caught #140.
//   - The horizon is USED, not merely recorded: `describeProgressReplay` turns a
//     fired migration into the loud startup line an operator can act on.
//
// Delivery of the replay itself is handled — since the outbox lane split, events
// older than `outbox.LiveHorizon` from a durable source are classified onto the
// backfill lane, so a replay of this size no longer queues ahead of live work.
// That bounds the damage; it does not make the cost free, which is why it is
// still declared.

// progressMigration is one schema step and what it costs the fleet.
type progressMigration struct {
	// V is the schema version this step migrates TO. Applied when the file on
	// disk reads `V < v`.
	V int

	// ReplayHorizon is how far back this step causes transcripts to be re-read.
	//
	// ZERO means the step clears no offsets and costs no replay — the honest
	// value for a migration that only drops cached decisions. Any step that
	// clears offsets MUST declare a non-zero horizon; the build check enforces
	// it, because "how much history does this re-read" is the one question a
	// reviewer needs and the one the diff never answers.
	ReplayHorizon time.Duration

	// Why is one line, shown to the operator when the migration fires. It has to
	// justify the replay to someone who did not write it and is watching their
	// laptop upload three weeks of history.
	Why string
}

// claudeProgressMigrations is the ordered history of claude-watcher-progress.json.
//
// APPEND ONLY. Editing a shipped row rewrites history for devices that already
// applied it; they will never re-run it, so the row describes what they did, not
// what it now says.
var claudeProgressMigrations = []progressMigration{
	{
		V:   1,
		Why: `drop cached "no" decisions written by the old timestamp gate; genuinely cwd-mismatched files re-cache on the next poll`,
	},
	{
		V:             2,
		ReplayHorizon: transcriptHistoryWindow,
		Why:           "reopen previously matched files so the bounded history policy gets exactly one chance to import the window",
	},
}

// codexProgressMigrations mirrors claudeProgressMigrations for
// codex-watcher-progress.json. The two watchers bump independently; a device can
// sit on different versions for each.
var codexProgressMigrations = []progressMigration{
	{
		V:   1,
		Why: `drop cached "no" decisions written by the old timestamp gate`,
	},
	{
		V:             2,
		ReplayHorizon: transcriptHistoryWindow,
		Why:           "reset matched offsets so classification replays only the new bounded window",
	},
}

// claudeProgressSchemaV / codexProgressSchemaV are the current schema versions,
// DERIVED from the tables above rather than written by hand.
//
// Derivation is the enforcement. A hand-written const can be bumped in one line
// with no row and no declared cost, which is exactly what #140 was; reading the
// version off the table means the row is not optional.
var (
	claudeProgressSchemaV = latestProgressV(claudeProgressMigrations)
	codexProgressSchemaV  = latestProgressV(codexProgressMigrations)
)

// latestProgressV is the highest declared version in a table. Zero for an empty
// table, which is the correct "no migrations yet" answer.
func latestProgressV(ms []progressMigration) int {
	v := 0
	for _, m := range ms {
		if m.V > v {
			v = m.V
		}
	}
	return v
}

// progressReplayPending reports the migrations that WILL fire for a file
// currently at version `from`, and the widest replay horizon among them.
//
// Called before the migration is applied, because after it the file reads as
// current and the cost is unrecoverable from the data.
func progressReplayPending(ms []progressMigration, from int) (fired []progressMigration, horizon time.Duration) {
	for _, m := range ms {
		if from < m.V {
			fired = append(fired, m)
			if m.ReplayHorizon > horizon {
				horizon = m.ReplayHorizon
			}
		}
	}
	return fired, horizon
}

// describeProgressReplay is the operator-facing line for a migration that is
// about to re-read history, or "" when nothing will be replayed.
//
// Deliberately loud and deliberately at START. The failure this addresses is not
// that the replay happens — it is a shipped feature and usually worth it — but
// that it happened SILENTLY, on someone else's machine, days after the commit,
// with the only symptom being a dashboard that called an active engineer idle.
// A device that says what it is about to do can be understood without reading
// the diff that caused it.
func describeProgressReplay(watcher string, ms []progressMigration, from int) string {
	fired, horizon := progressReplayPending(ms, from)
	if len(fired) == 0 || horizon == 0 {
		return ""
	}
	reasons := ""
	for _, m := range fired {
		if m.ReplayHorizon == 0 {
			continue
		}
		if reasons != "" {
			reasons += "; "
		}
		reasons += fmt.Sprintf("v%d: %s", m.V, m.Why)
	}
	return fmt.Sprintf(
		"%s-watcher: progress schema v%d -> v%d — RE-READING up to %s of local transcripts on this device. "+
			"Replayed events are queued on the backfill lane, so live capture is unaffected, but this device "+
			"will upload a backlog. Reason — %s",
		watcher, from, latestProgressV(ms), humanizeReplayHorizon(horizon), reasons)
}

// humanizeReplayHorizon renders a horizon in days, which is the unit the window
// is actually reasoned about in (28 days, not 672h).
func humanizeReplayHorizon(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	switch {
	case days > 1:
		return fmt.Sprintf("%d days", days)
	case days == 1:
		return "1 day"
	default:
		return d.String()
	}
}

// announceProgressReplay prints describeProgressReplay to stderr, AT MOST ONCE
// PER WATCHER PER PROCESS.
//
// Unconditional, not debug-gated. A replay that only a `PROMPTSTER_DEBUG=1`
// operator can see is the silent one this whole file exists to end.
//
// The once-per-process latch is NOT belt-and-braces, and review caught why. The
// loader runs on every 3s poll and is normally quiet on the second one, because
// the first save stamps the current version — but `saveClaudeWatchProgress`
// swallows its write errors by design. On a read-only or full state dir the
// version never persists, so every poll re-reads the OLD version, re-migrates,
// and would re-print this line: a stderr flood every 3 seconds for as long as
// the disk stays bad, burying the daemon log the message tells you to read.
// Latching means the operator gets it once. That the migration is also failing
// to persist is a different fault with its own report (§2.5), not something to
// say 1,200 times an hour.
func announceProgressReplay(watcher string, ms []progressMigration, from int) {
	line := describeProgressReplay(watcher, ms, from)
	if line == "" {
		return
	}
	announcedMu.Lock()
	if announced[watcher] {
		announcedMu.Unlock()
		return
	}
	announced[watcher] = true
	announcedMu.Unlock()
	fmt.Fprintln(progressReplayOut, line)
}

var (
	announcedMu sync.Mutex
	// announced latches per watcher name. Process-scoped on purpose: a restart
	// SHOULD re-announce, because a restart is when the migration re-runs.
	announced = map[string]bool{}
)

// resetProgressReplayAnnouncements clears the latch. Tests only.
func resetProgressReplayAnnouncements() {
	announcedMu.Lock()
	defer announcedMu.Unlock()
	announced = map[string]bool{}
}

// progressReplayOut is where announceProgressReplay writes. A var so tests can
// capture it; nothing else should reassign it.
var progressReplayOut io.Writer = os.Stderr

// findMigration looks up one declared step by version.
func findMigration(ms []progressMigration, v int) (progressMigration, bool) {
	for _, m := range ms {
		if m.V == v {
			return m, true
		}
	}
	return progressMigration{}, false
}

// --- §2.5: a lost or unreadable progress file --------------------------------

// reportProgressFileFault warns when a progress file EXISTS but could not be
// used, and reports how much history that costs.
//
// The three outcomes are not equally interesting and were previously all
// silent:
//
//   - MISSING is a fresh install. Silent, correctly: there is no history to
//     lose and nothing was reconstructed that should not have been.
//   - UNREADABLE (permissions, IO) or UNPARSEABLE (a truncated write, a disk
//     that returned garbage) means every offset this device had recorded is
//     gone. The consequence is identical to a schema bump — the full window is
//     re-read — except that nobody chose it, nothing announced it, and it
//     REPEATS ON EVERY POLL, because the load fails again three seconds later
//     and the save that would fix it writes to the same broken path.
//
// That last part is why this is separate from announceProgressReplay and why it
// is not latched the same way: the schema replay is a one-time cost that a latch
// correctly reports once, while this is a persistent fault the operator has to
// fix. It is rate-limited rather than latched, so it keeps saying so without
// becoming the log.
func reportProgressFileFault(watcher, path string, readErr error, parseErr error) {
	switch {
	case readErr != nil && os.IsNotExist(readErr):
		return // fresh install
	case readErr != nil:
		warnProgressFault(watcher, path, "cannot be READ", readErr)
	case parseErr != nil:
		warnProgressFault(watcher, path, "is CORRUPT", parseErr)
	}
}

func warnProgressFault(watcher, path, what string, err error) {
	progressFaultMu.Lock()
	last, seen := progressFaultLastWarn[watcher]
	now := progressFaultClock()
	if seen && now.Sub(last) < progressFaultRepeatInterval {
		progressFaultMu.Unlock()
		return
	}
	progressFaultLastWarn[watcher] = now
	progressFaultMu.Unlock()

	fmt.Fprintf(progressReplayOut,
		"%s-watcher: the capture progress file %s %s (%v) — every recorded read offset is "+
			"LOST, so up to %s of local transcripts will be re-read and re-uploaded, and this "+
			"repeats on every poll until it is fixed. Check permissions and free space on the "+
			"state directory.\n",
		watcher, path, what, err, humanizeReplayHorizon(transcriptHistoryWindow))
}

// progressFaultRepeatInterval bounds how often the fault repeats. Long enough
// not to become the log, short enough that an operator who arrives an hour into
// the problem still sees it — the failure mode being addressed is silence, and
// a warning that scrolled past once an hour ago is close to silent.
const progressFaultRepeatInterval = 15 * time.Minute

var (
	progressFaultMu       sync.Mutex
	progressFaultLastWarn = map[string]time.Time{}
	// progressFaultClock is time.Now. A var so tests can advance it without
	// sleeping fifteen minutes.
	progressFaultClock = time.Now
)

// resetProgressFaultReports clears the rate limiter. Tests only.
func resetProgressFaultReports() {
	progressFaultMu.Lock()
	defer progressFaultMu.Unlock()
	progressFaultLastWarn = map[string]time.Time{}
}
