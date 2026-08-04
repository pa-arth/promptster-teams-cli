package cli

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// worstLevel is the severity doctor actually communicates: one warn among the
// lines makes the whole check a warning.
func worstLevel(lines []queueLine) queueLevel {
	w := queueOK
	for _, l := range lines {
		if l.level > w {
			w = l.level
		}
	}
	return w
}

func allText(lines []queueLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		b.WriteByte('\n')
	}
	return b.String()
}

func levelName(l queueLevel) string {
	switch l {
	case queueErr:
		return "err"
	case queueWarn:
		return "warn"
	default:
		return "ok"
	}
}

func TestCheckQueueHealth(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-10 * time.Second)
	stale := now.Add(-14 * time.Minute)

	cases := []struct {
		name      string
		in        queueInputs
		want      queueLevel
		contains  []string
		omits     []string
		wantLines int // 0 = don't care
	}{
		{
			name: "empty queue is ok and says so",
			in: queueInputs{
				pending: 0, haveOutbox: true, size: 4096,
				draining: true, lastProgress: fresh, haveProgress: true, now: now,
			},
			want:     queueOK,
			contains: []string{"empty"},
		},
		{
			// THE cry-wolf guard. A machine that captured events and then stopped
			// watching holds a backlog forever, and that is the normal idle state
			// of every laptop overnight. lastProgress is deliberately ancient here:
			// the absence of a drainer must win over the staleness probe, because
			// "stale" is meaningless when nothing is supposed to be draining.
			name: "backlog with no watcher running is not a warning",
			in: queueInputs{
				pending: 4210, haveOutbox: true, size: 1 << 20,
				draining: false, lastProgress: stale, haveProgress: true, now: now,
			},
			want:     queueOK,
			contains: []string{"nothing is draining", "capture is not running", "4210 events"},
		},
		{
			name: "backlog with watcher running and cursor advancing is just backlog",
			in: queueInputs{
				pending: 37, haveOutbox: true, size: 1 << 20,
				draining: true, lastProgress: fresh, haveProgress: true, now: now,
			},
			want:     queueOK,
			contains: []string{"draining", "37 events"},
		},
		{
			name: "backlog with watcher running and stale cursor warns with an action",
			in: queueInputs{
				pending: 900, haveOutbox: true, size: 1 << 20,
				draining: true, lastProgress: stale, haveProgress: true, now: now,
			},
			want: queueWarn,
			contains: []string{
				"stuck", "900 events", "14m",
				"401",        // revoked key
				"ingest",     // endpoint down
				"state dir",  // disk full / unwritable
				"daemon.log", // where to look
			},
		},
		{
			name: "draining with no progress timestamp does not guess a verdict",
			in: queueInputs{
				pending: 5, haveOutbox: true, size: 4096,
				draining: true, haveProgress: false, now: now,
			},
			want:     queueOK,
			contains: []string{"delivery is running"},
		},
		{
			name: "outbox at the cap is an error that names the data loss",
			in: queueInputs{
				pending: 160000, haveOutbox: true, size: outbox.OutboxMaxBytes,
				draining: true, lastProgress: fresh, haveProgress: true, now: now,
			},
			want:     queueErr,
			contains: []string{"FULL", "DROPPED", "daemon.log"},
		},
		{
			name: "outbox past the cap is still an error",
			in: queueInputs{
				pending: 160000, haveOutbox: true, size: outbox.OutboxMaxBytes + (1 << 20),
				draining: true, lastProgress: fresh, haveProgress: true, now: now,
			},
			want:     queueErr,
			contains: []string{"FULL", "DROPPED"},
		},
		{
			// Append drops on file size, not backlog depth, so a queue that has
			// drained but not yet compacted is simultaneously "full" and "empty".
			// Both are true; printing both reads as a contradiction. FULL wins.
			name: "full outbox with a drained backlog reports the data loss, not emptiness",
			in: queueInputs{
				pending: 0, haveOutbox: true, size: outbox.OutboxMaxBytes,
				draining: true, lastProgress: fresh, haveProgress: true, now: now,
			},
			want:      queueErr,
			contains:  []string{"FULL", "DROPPED"},
			omits:     []string{"empty", "shipped"},
			wantLines: 1,
		},
		{
			// Exactly on the threshold: the warning has to fire here, because the
			// only thing it buys anyone is lead time before the cap starts
			// dropping events.
			name: "outbox exactly at the near-full threshold warns before events are dropped",
			in: queueInputs{
				pending: 120000, haveOutbox: true, size: outbox.OutboxMaxBytes * queueNearFullPercent / 100,
				draining: true, lastProgress: fresh, haveProgress: true, now: now,
			},
			want:     queueWarn,
			contains: []string{"75% full", "of 64.0 MB", "DROPPED at the cap"},
		},
		{
			// One byte under the threshold. Pins the boundary as inclusive and
			// proves the size line stays silent on a merely-large queue.
			name: "outbox one byte below the threshold says nothing about size",
			in: queueInputs{
				pending: 10, haveOutbox: true, size: outbox.OutboxMaxBytes*queueNearFullPercent/100 - 1,
				draining: true, lastProgress: fresh, haveProgress: true, now: now,
			},
			want:     queueOK,
			contains: []string{"draining"},
		},
		{
			name: "half-full outbox says nothing about size",
			in: queueInputs{
				pending: 10, haveOutbox: true, size: outbox.OutboxMaxBytes / 2,
				draining: true, lastProgress: fresh, haveProgress: true, now: now,
			},
			want:     queueOK,
			contains: []string{"draining"},
		},
		{
			// A machine that has never captured has no outbox. That is a fresh
			// install, not a fault.
			name:     "fresh install with no outbox at all is ok",
			in:       queueInputs{pending: 0, haveOutbox: false, draining: false, now: now},
			want:     queueOK,
			contains: []string{"empty"},
		},
		{
			// PendingCount returns 0 for an unreadable queue exactly as it does for
			// an empty one. Reporting "every captured event has shipped" about a
			// file we cannot open is a confident all-clear about nothing.
			name: "unreadable outbox does not masquerade as an empty one",
			in: queueInputs{
				pending: 0, haveOutbox: true, unreadable: true, size: 1 << 20,
				draining: true, lastProgress: fresh, haveProgress: true, now: now,
			},
			want:      queueWarn,
			contains:  []string{"unreadable", "permissions"},
			omits:     []string{"empty", "shipped"},
			wantLines: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := checkQueueHealth(tc.in)
			if len(lines) == 0 {
				t.Fatal("checkQueueHealth returned no lines; doctor would print nothing")
			}
			if got := worstLevel(lines); got != tc.want {
				t.Errorf("level = %s, want %s\nlines:\n%s", levelName(got), levelName(tc.want), allText(lines))
			}
			if tc.wantLines != 0 && len(lines) != tc.wantLines {
				t.Errorf("got %d lines, want %d:\n%s", len(lines), tc.wantLines, allText(lines))
			}
			text := allText(lines)
			for _, want := range tc.contains {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in:\n%s", want, text)
				}
			}
			for _, bad := range tc.omits {
				if strings.Contains(text, bad) {
					t.Errorf("contradictory %q present in:\n%s", bad, text)
				}
			}
			for _, l := range lines {
				if l.glyph() == "" {
					t.Error("queueLine rendered an empty glyph")
				}
			}
		})
	}
}

// A fresh install has no outbox and no cursor. Nothing here may look like a
// fault, and nothing may panic on the missing files.
func TestGatherQueueInputsFreshInstall(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	in := gatherQueueInputs(time.Now(), capture.CaptureSnapshot{})

	if in.haveOutbox {
		t.Error("haveOutbox = true with no outbox file")
	}
	if in.pending != 0 {
		t.Errorf("pending = %d, want 0", in.pending)
	}
	if in.haveProgress {
		t.Error("haveProgress = true with no cursor and no watcher")
	}
	if in.draining {
		t.Error("draining = true with no capture process")
	}
	if got := worstLevel(checkQueueHealth(in)); got != queueOK {
		t.Errorf("fresh install reported %s, want ok", levelName(got))
	}
}

// The cursor's mtime is the progress probe.
func TestGatherQueueInputsUsesCursorMtime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	seedOutbox(t, 3)

	cursorMtime := time.Now().Add(-9 * time.Minute).Truncate(time.Second)
	writeFileAt(t, state.OutboxCursorPath(), "0", cursorMtime)

	in := gatherQueueInputs(time.Now(), liveSnapshot(time.Now().Add(-30*time.Minute)))

	if !in.haveProgress {
		t.Fatal("haveProgress = false with a cursor on disk")
	}
	if !in.lastProgress.Equal(cursorMtime) {
		t.Errorf("lastProgress = %v, want cursor mtime %v", in.lastProgress, cursorMtime)
	}
	if !in.haveOutbox || in.size == 0 {
		t.Errorf("outbox not seen: haveOutbox=%v size=%d", in.haveOutbox, in.size)
	}
	if in.pending != 3 {
		t.Errorf("pending = %d, want 3", in.pending)
	}
	if got := worstLevel(checkQueueHealth(in)); got != queueWarn {
		t.Errorf("stale cursor with a live watcher reported %s, want warn", levelName(got))
	}
}

// No cursor at all means delivery has NEVER succeeded — exactly what a revoked
// key looks like on a machine that has only ever 401'd. Cursor mtime is
// unavailable, so the watcher's start time has to carry the probe. Without the
// fallback this machine — the one this feature exists for — reports nothing.
func TestGatherQueueInputsNoCursorFallsBackToWatcherStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	seedOutbox(t, 12)

	if _, err := os.Stat(state.OutboxCursorPath()); !os.IsNotExist(err) {
		t.Fatalf("precondition: cursor should not exist, got err=%v", err)
	}

	started := time.Now().Add(-14 * time.Minute)
	in := gatherQueueInputs(time.Now(), liveSnapshot(started))

	if !in.haveProgress {
		t.Fatal("haveProgress = false; the watcher-start fallback did not fire")
	}
	if !in.lastProgress.Equal(started) {
		t.Errorf("lastProgress = %v, want watcher StartedAt %v", in.lastProgress, started)
	}
	lines := checkQueueHealth(in)
	if got := worstLevel(lines); got != queueWarn {
		t.Errorf("never-delivered queue reported %s, want warn\nlines:\n%s", levelName(got), allText(lines))
	}
	if !strings.Contains(allText(lines), "401") {
		t.Errorf("stuck line does not name the revoked-key cause:\n%s", allText(lines))
	}
}

// The flip side of the fallback: a watcher that restarted seconds ago has not
// had time to deliver anything, so it must not be called stuck.
func TestGatherQueueInputsFreshWatcherIsNotStuck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	seedOutbox(t, 12)

	in := gatherQueueInputs(time.Now(), liveSnapshot(time.Now().Add(-10*time.Second)))

	if got := worstLevel(checkQueueHealth(in)); got != queueOK {
		t.Errorf("just-restarted watcher reported %s, want ok", levelName(got))
	}
}

// THE cry-wolf guard, at the layer where it actually broke.
//
// snap.Live ORs in DaemonStatus(), which only checks that the PID in
// supervisor.json exists — no heartbeat. A supervisor killed without a clean
// stop leaves that pidfile behind, and after a reboot the OS reuses the PID, so
// Live reads true forever with both watchers dead. Gating on Live told an idle
// laptop with a backlog that its queue was stuck and blamed a revoked key.
//
// The equivalent table-driven case sets draining:false by hand and so cannot see
// this: the bug was entirely in the snapshot -> draining mapping.
func TestGatherQueueInputsStaleSupervisorIsNotDraining(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	seedOutbox(t, 4210)

	// A supervisor pidfile naming a live PID, with no watcher pidfile at all —
	// what a recycled PID looks like after an unclean shutdown and a reboot.
	snap := capture.CaptureSnapshot{
		Live:      true,
		DaemonPID: os.Getpid(),
		Claude:    capture.WatcherStat{Name: "claude", Running: false},
		Codex:     capture.WatcherStat{Name: "codex", Running: false},
	}

	in := gatherQueueInputs(time.Now(), snap)

	if in.draining {
		t.Error("draining = true with a stale supervisor and no live watcher; " +
			"the drain signal must require a heartbeat-backed watcher, not snap.Live")
	}
	lines := checkQueueHealth(in)
	if got := worstLevel(lines); got != queueOK {
		t.Errorf("idle machine with a stale supervisor reported %s, want ok\nlines:\n%s",
			levelName(got), allText(lines))
	}
	if text := allText(lines); strings.Contains(text, "stuck") || strings.Contains(text, "401") {
		t.Errorf("doctor blamed delivery for a queue nothing is draining:\n%s", text)
	}
}

// The real-filesystem half of the unreadable case: os.Stat succeeds on a file
// the process cannot open, so haveOutbox/size look fine while PendingCount
// silently reports 0. Without the readability probe this renders as "delivery
// queue empty — every captured event has shipped".
func TestGatherQueueInputsUnreadableOutbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not deny read access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	seedOutbox(t, 4210)

	if err := os.Chmod(state.OutboxPath(), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(state.OutboxPath(), 0o600) }) // let TempDir clean up

	in := gatherQueueInputs(time.Now(), liveSnapshot(time.Now().Add(-time.Minute)))

	if !in.haveOutbox {
		t.Fatal("precondition: stat should still see the outbox")
	}
	if !in.unreadable {
		t.Fatal("unreadable = false for an outbox that cannot be opened")
	}
	lines := checkQueueHealth(in)
	if got := worstLevel(lines); got != queueWarn {
		t.Errorf("unreadable outbox reported %s, want warn\nlines:\n%s", levelName(got), allText(lines))
	}
	if text := allText(lines); strings.Contains(text, "shipped") {
		t.Errorf("doctor gave an all-clear about a queue it cannot read:\n%s", text)
	}
}

// watcherDraining must ignore the unguarded supervisor signal and trust only a
// heartbeat-backed watcher.
func TestWatcherDraining(t *testing.T) {
	if watcherDraining(capture.CaptureSnapshot{Live: true, DaemonPID: os.Getpid()}) {
		t.Error("watcherDraining = true from snap.Live alone with no live watcher")
	}
	if !watcherDraining(capture.CaptureSnapshot{Claude: capture.WatcherStat{Running: true}}) {
		t.Error("watcherDraining = false with a live claude watcher")
	}
	// Both watchers share one process-wide drain loop, so codex alone counts.
	if !watcherDraining(capture.CaptureSnapshot{Codex: capture.WatcherStat{Running: true}}) {
		t.Error("watcherDraining = false with a live codex watcher; a codex-only " +
			"user would be told nothing is draining while delivery runs fine")
	}
	if watcherDraining(capture.CaptureSnapshot{}) {
		t.Error("watcherDraining = true with no watchers at all")
	}
}

// latestWatcherStart must ignore watchers that are not running, and must pick
// the newest of the live ones.
func TestLatestWatcherStart(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	recent := time.Now().Add(-time.Minute)

	snap := capture.CaptureSnapshot{
		Claude: capture.WatcherStat{Name: "claude", Running: true, StartedAt: old},
		Codex:  capture.WatcherStat{Name: "codex", Running: true, StartedAt: recent},
	}
	if got := latestWatcherStart(snap); !got.Equal(recent) {
		t.Errorf("latestWatcherStart = %v, want the newest live watcher %v", got, recent)
	}

	// A dead watcher's start time is not evidence of anything.
	snap.Codex.Running = false
	if got := latestWatcherStart(snap); !got.Equal(old) {
		t.Errorf("latestWatcherStart = %v, want %v (dead watcher must be ignored)", got, old)
	}

	if got := latestWatcherStart(capture.CaptureSnapshot{}); !got.IsZero() {
		t.Errorf("latestWatcherStart = %v, want zero with no watchers", got)
	}
}

// liveSnapshot is a capture process with one running claude watcher.
func liveSnapshot(startedAt time.Time) capture.CaptureSnapshot {
	return capture.CaptureSnapshot{
		Live:      true,
		DaemonPID: os.Getpid(),
		Claude:    capture.WatcherStat{Name: "claude", Running: true, StartedAt: startedAt, LastHeartbeat: time.Now()},
	}
}

// seedOutbox writes n undelivered events to the outbox with no cursor, so
// PendingCount reads them all as pending.
func seedOutbox(t *testing.T, n int) {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(`{"kind":"prompt","sessionId":"s1"}` + "\n")
	}
	if err := os.WriteFile(state.OutboxPath(), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
}

func writeFileAt(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// --- both lanes -------------------------------------------------------------
//
// The queue is two lanes now, and doctor reading only the live one is a real
// blind spot rather than a theoretical one: the backfill lane has its own file,
// its own OutboxMaxBytes ceiling and its own cursor, so it can be at the cap and
// dropping replayed events, or wedged for hours, entirely underneath a healthy
// live lane. Every test below fails if the gather goes back to measuring live.

// seedBackfill fills the backfill lane the way seedOutbox fills the live one.
func seedBackfill(t *testing.T, n int) {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(`{"kind":"prompt","sessionId":"s1"}` + "\n")
	}
	if err := os.WriteFile(state.OutboxBackfillPath(), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("seed backfill outbox: %v", err)
	}
}

// growTo pads a file to at least n bytes without building n bytes in memory.
func growTo(t *testing.T, path string, n int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if err := f.Truncate(n); err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
}

// A backfill lane at the cap is DROPPING replayed events right now. Doctor must
// say so even while the live lane is small and healthy.
func TestGatherQueueInputsSeesAFullBackfillLane(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	seedOutbox(t, 3)
	seedBackfill(t, 1)
	growTo(t, state.OutboxBackfillPath(), outbox.OutboxMaxBytes)

	in := gatherQueueInputs(time.Now(), liveSnapshot(time.Now().Add(-time.Minute)))
	lines := checkQueueHealth(in)

	if got := worstLevel(lines); got != queueErr {
		t.Errorf("a full BACKFILL lane reported %s, want err\nlines:\n%s", levelName(got), allText(lines))
	}
	if text := allText(lines); !strings.Contains(text, "backfill") {
		t.Errorf("the message must name which lane is full, or the engineer cannot act on it:\n%s", text)
	}
}

// The same for readability: a backfill lane that cannot be opened makes its
// depth unknowable, and PendingCount reports 0 for it exactly as for an empty
// one — "every captured event has shipped" over the top of invisible work.
func TestGatherQueueInputsSeesAnUnreadableBackfillLane(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not deny read access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	seedOutbox(t, 3)
	seedBackfill(t, 4210)

	if err := os.Chmod(state.OutboxBackfillPath(), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(state.OutboxBackfillPath(), 0o600) })

	in := gatherQueueInputs(time.Now(), liveSnapshot(time.Now().Add(-time.Minute)))
	if !in.unreadable {
		t.Fatal("unreadable = false for a backfill lane that cannot be opened")
	}
	text := allText(checkQueueHealth(in))
	if !strings.Contains(text, "unreadable") || !strings.Contains(text, "backfill") {
		t.Errorf("an unreadable backfill lane must be reported, and named:\n%s", text)
	}
}

// Progress takes the OLDEST lane that actually holds work. A live lane
// delivering every second must not paper over a backfill lane that has not
// advanced in an hour — that is precisely the stall the lane split exists to
// make visible rather than hide.
func TestGatherQueueInputsTakesTheStalestLaneHoldingWork(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	seedOutbox(t, 5)
	seedBackfill(t, 5)

	fresh := time.Now().Add(-2 * time.Second).Truncate(time.Second)
	stale := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	writeFileAt(t, state.OutboxCursorPath(), "0", fresh)
	writeFileAt(t, state.OutboxCursorPathFor(state.OutboxBackfillPath()), "0", stale)

	in := gatherQueueInputs(time.Now(), liveSnapshot(time.Now().Add(-2*time.Hour)))

	if !in.lastProgress.Equal(stale) {
		t.Errorf("lastProgress = %v, want the STALE backfill cursor %v — a healthy live lane must not hide a wedged backfill one",
			in.lastProgress, stale)
	}
	text := allText(checkQueueHealth(in))
	if !strings.Contains(text, "stuck") {
		t.Errorf("a backfill lane with no progress in an hour must read as stuck:\n%s", text)
	}
	if !strings.Contains(text, "backfill") {
		t.Errorf("the stuck message must name the lane:\n%s", text)
	}
}

// The inverse, and the reason progress is filtered by pending: an EMPTY lane has
// an old cursor for the honest reason that there was nothing to advance it.
// Counting it would report every ordinary machine as stuck.
func TestGatherQueueInputsIgnoresAnEmptyLanesOldCursor(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	seedOutbox(t, 5)
	seedBackfill(t, 0)

	fresh := time.Now().Add(-2 * time.Second).Truncate(time.Second)
	ancient := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	writeFileAt(t, state.OutboxCursorPath(), "0", fresh)
	writeFileAt(t, state.OutboxCursorPathFor(state.OutboxBackfillPath()), "0", ancient)

	in := gatherQueueInputs(time.Now(), liveSnapshot(time.Now().Add(-time.Hour)))
	lines := checkQueueHealth(in)

	if got := worstLevel(lines); got != queueOK {
		t.Errorf("an empty backfill lane with a month-old cursor reported %s, want ok\nlines:\n%s",
			levelName(got), allText(lines))
	}
}

// --- history reconstruction --------------------------------------------------

// The line is an OK, not a warning. A replay is a normal, deliberate thing this
// product does; reporting it as a fault would train people to ignore it, and the
// failure being fixed is that it was INVISIBLE, not insufficiently alarming.
func TestReconstructionLineIsInformationalNotAWarning(t *testing.T) {
	r := capture.ReconstructionState{
		Running: true, Files: 12, Bytes: 48 << 20,
		Oldest: time.Now().Add(-15 * 24 * time.Hour),
	}
	lines := reconstructionLines(r, time.Now())
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if lines[0].level != queueOK {
		t.Errorf("a running replay reported %s; it is expected behaviour, not a fault",
			levelName(lines[0].level))
	}
	for _, want := range []string{"reconstructing history", "12 transcripts", "backfill"} {
		if !strings.Contains(lines[0].text, want) {
			t.Errorf("line missing %q: %s", want, lines[0].text)
		}
	}
}

// Nothing to say when nothing is replaying. A permanent "not reconstructing" row
// teaches the reader to skip that line by the third time they see it.
func TestNoReconstructionMeansNoLine(t *testing.T) {
	if lines := reconstructionLines(capture.ReconstructionState{}, time.Now()); len(lines) != 0 {
		t.Errorf("an idle machine printed a reconstruction line: %s", allText(lines))
	}
}

// The DATE the replay has reached, not a percentage: the window is bounded by
// date, so a date says both how far along it is and how far it has to go.
func TestTheReconstructionLineReportsHowFarBackItHasReached(t *testing.T) {
	oldest := time.Now().Add(-15 * 24 * time.Hour)
	lines := reconstructionLines(capture.ReconstructionState{
		Running: true, Files: 1, Bytes: 1024, Oldest: oldest,
	}, time.Now())
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0].text, oldest.Format("2006-01-02")) {
		t.Errorf("line must name the date the replay has reached: %s", lines[0].text)
	}
}
