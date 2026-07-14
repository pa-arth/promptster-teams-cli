package cli

import (
	"os"
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
