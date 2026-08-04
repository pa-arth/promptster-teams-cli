package capture

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The startup announcement is the horizon's only consumer today, and without a
// consumer the declaration is decoration. What it has to get right is narrow:
// say something exactly when history is about to be re-read, say nothing
// otherwise, and say it in a unit an operator can act on.

// captureAnnouncements redirects the announcement and clears the once-per-watcher
// latch, which is process-wide and would otherwise leak between tests.
func captureAnnouncements(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := progressReplayOut
	progressReplayOut = &buf
	resetProgressReplayAnnouncements()
	t.Cleanup(func() {
		progressReplayOut = prev
		resetProgressReplayAnnouncements()
	})
	return &buf
}

func TestAReplayingMigrationAnnouncesItself(t *testing.T) {
	buf := captureAnnouncements(t)

	announceProgressReplay("claude", claudeProgressMigrations, 1)

	got := buf.String()
	if got == "" {
		t.Fatal("a v1 device upgrading to v2 re-reads the whole window and said nothing — " +
			"that silence IS #140")
	}
	for _, want := range []string{"claude-watcher", "v1 -> v2", "28 days", "RE-READING", "backfill"} {
		if !strings.Contains(got, want) {
			t.Errorf("announcement missing %q:\n%s", want, got)
		}
	}
}

// A device already on the current schema migrates nothing. Announcing there
// would print the scary line on every ordinary start, and a warning that fires
// when nothing is wrong is a warning nobody reads.
func TestACurrentDeviceAnnouncesNothing(t *testing.T) {
	buf := captureAnnouncements(t)

	announceProgressReplay("claude", claudeProgressMigrations, claudeProgressSchemaV)

	if got := buf.String(); got != "" {
		t.Errorf("a device already at v%d announced a replay:\n%s", claudeProgressSchemaV, got)
	}
}

// A migration that clears no offsets costs no replay, so it gets no line even
// though it does fire. The trigger is the declared COST, not the version change.
func TestAZeroCostMigrationAnnouncesNothing(t *testing.T) {
	free := []progressMigration{{V: 1, Why: "drop a cache"}}
	if got := describeProgressReplay("claude", free, 0); got != "" {
		t.Errorf("a migration with no declared horizon announced a replay:\n%s", got)
	}
}

// The WIDEST horizon across every step that will fire, not the last one's.
//
// The widest step is deliberately NOT last here. A device at v0 is re-read 30
// days back by the v1 step whatever v3 does, so "last fired wins" is wrong — and
// with the widest step placed last it is wrong INVISIBLY, which is how the first
// draft of this test let that mutation live.
func TestTheAnnouncementReportsTheWidestPendingHorizon(t *testing.T) {
	ms := []progressMigration{
		{V: 1, ReplayHorizon: 30 * 24 * time.Hour, Why: "thirty days"},
		{V: 2, Why: "free"},
		{V: 3, ReplayHorizon: 24 * time.Hour, Why: "one day"},
	}
	got := describeProgressReplay("codex", ms, 0)
	if !strings.Contains(got, "30 days") {
		t.Errorf("want the widest horizon (30 days), got:\n%s", got)
	}
	if strings.Contains(got, "free") {
		t.Errorf("a zero-cost step must not be listed as a reason for the replay:\n%s", got)
	}
}

// Days, not 672h0m0s. The window is reasoned about in days everywhere else, and
// an operator converting hours in their head is one who misreads it.
func TestTheHorizonIsRenderedInDays(t *testing.T) {
	cases := map[time.Duration]string{
		28 * 24 * time.Hour: "28 days",
		24 * time.Hour:      "1 day",
		90 * time.Minute:    "1h30m0s",
	}
	for d, want := range cases {
		if got := humanizeReplayHorizon(d); got != want {
			t.Errorf("humanizeReplayHorizon(%s) = %q, want %q", d, got, want)
		}
	}
}

// A state dir that cannot be written never persists the new version, so the
// loader re-reads the OLD one on every 3s poll, re-migrates, and would re-print
// the replay line forever — burying the daemon log the message tells you to
// read. Once per watcher per process; a restart re-announces, because a restart
// is when the migration re-runs.
func TestARepeatedMigrationAnnouncesOnlyOnce(t *testing.T) {
	buf := captureAnnouncements(t)

	for i := 0; i < 20; i++ {
		announceProgressReplay("claude", claudeProgressMigrations, 1)
	}

	if n := strings.Count(buf.String(), "RE-READING"); n != 1 {
		t.Errorf("announced %d times across 20 polls, want 1 — an unwritable state dir would "+
			"flood stderr every 3s for as long as the disk stays bad", n)
	}
}

// The latch is per WATCHER. Claude and Codex migrate independently, and one
// silencing the other would hide half the fleet-wide replay.
func TestTheAnnouncementLatchIsPerWatcher(t *testing.T) {
	buf := captureAnnouncements(t)

	announceProgressReplay("claude", claudeProgressMigrations, 1)
	announceProgressReplay("codex", codexProgressMigrations, 1)

	got := buf.String()
	if !strings.Contains(got, "claude-watcher") || !strings.Contains(got, "codex-watcher") {
		t.Errorf("both watchers must announce their own replay:\n%s", got)
	}
}
