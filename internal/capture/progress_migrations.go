package capture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	// Keyed "-read", separately from the write fault below. A state dir can fail
	// both ways at once, and one fault silencing the other is the same silence
	// this reporting exists to end.
	if !faultWarnAllowed(watcher + "-read") {
		return
	}
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

// faultWarnAllowed reports whether `key` may warn now, and records it if so.
// One rate limiter, many keys — see the callers for why they must not share one.
func faultWarnAllowed(key string) bool {
	progressFaultMu.Lock()
	defer progressFaultMu.Unlock()
	last, seen := progressFaultLastWarn[key]
	now := progressFaultClock()
	if seen && now.Sub(last) < progressFaultRepeatInterval {
		return false
	}
	progressFaultLastWarn[key] = now
	return true
}

// resetProgressFaultReports clears the rate limiter and the write-fault state.
// Tests only.
func resetProgressFaultReports() {
	progressFaultMu.Lock()
	progressFaultLastWarn = map[string]time.Time{}
	progressFaultMu.Unlock()

	writeFaultMu.Lock()
	writeFaulted = map[string]bool{}
	writeFaultMu.Unlock()
}

// --- the WRITE side: a device that cannot SAVE its progress ------------------

// reportProgressWriteFault warns when progress could not be PERSISTED.
//
// This is the worse half of the progress-file fault, and the read-side report
// cannot cover it. A state directory that is read-only or full still SERVES the
// existing file perfectly: the read succeeds, it parses, and it returns stale
// offsets. Nothing is corrupt, so nothing on the read path has anything to
// complain about — the machine with the problem is exactly the machine that
// looks healthy.
//
// The cost is also worse. A device that cannot READ replays the window ONCE and
// then writes a good file. A device that cannot WRITE replays it on EVERY
// RESTART, forever: in-memory progress still advances, so the run in front of
// you is correct and looks fine, and the bill arrives at the next start. Self
// update re-execs, so restarts are routine rather than rare.
//
// Saying "up to 28 days will be re-read" would duplicate the read-side report
// and omit the one fact that makes this fault different, so the message leads
// with the repeat.
func reportProgressWriteFault(watcher, path, what string, err error) {
	markProgressWriteFault(watcher, true)
	if !faultWarnAllowed(watcher + "-write") {
		return
	}
	fmt.Fprintf(progressReplayOut,
		"%s-watcher: cannot SAVE capture progress — %s %s (%v). Read offsets are not being "+
			"recorded, so this device will re-read up to %s of local transcripts AGAIN ON THE "+
			"NEXT RESTART, and on every restart until this is fixed. Check permissions and free "+
			"space on the state directory.\n",
		watcher, path, what, err, humanizeReplayHorizon(transcriptHistoryWindow))
}

// markProgressWriteFault records (or clears) a watcher's inability to persist.
//
// A successful save CLEARS it, and that is not bookkeeping tidiness: once
// progress lands on disk the replay-on-restart cost is genuinely gone, so a
// transient full disk that recovers must stop warning. A flag that only ever
// sets would leave doctor red on a machine that fixed itself, which is how a
// warning becomes furniture.
func markProgressWriteFault(watcher string, faulted bool) {
	writeFaultMu.Lock()
	defer writeFaultMu.Unlock()
	if faulted {
		writeFaulted[watcher] = true
		return
	}
	delete(writeFaulted, watcher)
}

// ProgressWriteFaulted names the watchers that have failed to persist progress
// IN THIS PROCESS, sorted. Empty on a healthy device.
//
// Only the watcher process can answer this, which is exactly why the operator
// surfaces do not use it — see ProgressPersistenceFault.
func ProgressWriteFaulted() []string {
	writeFaultMu.Lock()
	defer writeFaultMu.Unlock()
	out := make([]string, 0, len(writeFaulted))
	for w := range writeFaulted {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

var (
	writeFaultMu sync.Mutex
	// writeFaulted is process-scoped: it describes what this daemon has observed,
	// and a restart is precisely when the question is asked again from scratch.
	writeFaulted = map[string]bool{}
)

// ProgressPersistenceFault reports whether this device can persist capture
// progress RIGHT NOW, by trying it. nil means it can.
//
// IT RE-DERIVES THE ANSWER RATHER THAN PROPAGATING IT, and review is why. The
// first version of this change recorded the fault in the process-local map above
// and read that map from `status` and `doctor`. But `claude-watch` and
// `codex-watch` are SEPARATE PROCESSES — `internal/cli/cli.go` dispatches them
// as their own commands, spawned detached by the daemon — so the CLI's map was
// always empty, and every operator surface stayed silent while the watcher
// failed to save on every poll. The reporting existed and could not fire.
//
// Sharing state across that boundary would mean writing a marker file into the
// directory whose unwritability IS the fault. Asking the question directly has
// no such catch-22 and no staleness: the probe runs the same syscalls, against
// the same directory, that a real save runs.
//
// It DOES write, which is why it is called from `status` and `doctor` only and
// never from a poll loop. It creates and removes one scratch file and touches no
// capture state — cursor, queue and ledger are untouched, so doctor's
// read-only-toward-capture guarantee holds.
//
// Known limit, stated rather than papered over: this answers "can the state
// directory be written now", which covers the persistent faults that matter —
// permissions, a full disk, a read-only mount. A transient failure the watcher
// hit and the probe cannot reproduce is reported by the watcher's own stderr
// line and not here.
func ProgressPersistenceFault() error {
	dir := filepath.Dir(claudeWatchProgressPath())

	f, err := os.CreateTemp(dir, ".progress-probe-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write([]byte("probe")); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	// The rename is what COMMITS a real save, and it fails independently of the
	// write — a probe that stopped at the write would miss precisely the case the
	// discarded `_ = os.Rename(...)` was hiding.
	dst := tmp + ".committed"
	if err := progressProbeRename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Remove(dst)
	return nil
}

// progressProbeRename is os.Rename. A var because a probe that silently skipped
// the commit step would pass every black-box test — a sealed directory fails at
// CreateTemp long before the rename is reached, so nothing else can tell the two
// implementations apart. Injecting a rename failure is the only way to prove the
// probe checks the step that the discarded `_ = os.Rename(...)` was hiding.
var progressProbeRename = os.Rename
