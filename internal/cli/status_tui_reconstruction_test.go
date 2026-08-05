package cli

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
)

// THE DASHBOARD IS WHERE THIS HAS TO APPEAR, and review caught that it did not.
//
// `status` opens the TUI by default — the static print only runs on --once,
// --plain, or a non-TTY stdout. So an engineer typing `promptster-teams status`
// during a replay got the dashboard, and the dashboard never asked about
// reconstruction: a five-figure pending count under a warn dot, and nothing on
// screen saying why. That is the #140 experience unchanged, on the one surface
// anyone actually looks at.

// fakeRecon points the dashboard at a fixed reconstruction state.
func fakeRecon(t *testing.T, r capture.ReconstructionState) *int {
	t.Helper()
	calls := 0
	prev := reconNow
	reconNow = func() capture.ReconstructionState {
		calls++
		return r
	}
	t.Cleanup(func() { reconNow = prev })
	return &calls
}

func replayingState() capture.ReconstructionState {
	return capture.ReconstructionState{
		Running: true,
		Files:   3,
		Bytes:   41_943_040, // 40 MB
		Oldest:  time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}
}

func TestTheDashboardExplainsTheBacklogWhileHistoryIsReplaying(t *testing.T) {
	fakeRecon(t, replayingState())

	m := newStatusModel()
	m.buffered = 20761 // the #140 backlog depth, which is what prompts the question
	view := m.View()

	// The bytes, the file count and the DATE reached — the date is the number that
	// answers "how much of the window is left", because the window is bounded by
	// date rather than by size.
	for _, want := range []string{"replaying", "40", "3 transcripts", "2026-07-08", "backfill lane"} {
		if !strings.Contains(view, want) {
			t.Errorf("dashboard missing %q — this is the view `status` opens by default\n---\n%s", want, view)
		}
	}

	// Read TOGETHER or not at all: the alarming number and its explanation are in
	// one panel on purpose, so neither can be seen without the other.
	if !strings.Contains(m.bufferPanel(), "20761") || !strings.Contains(m.bufferPanel(), "replaying") {
		t.Errorf("the backlog and its cause must share a panel:\n%s", m.bufferPanel())
	}
}

// The ordinary state is not replaying, and a permanent "replaying: no" row on a
// dashboard that redraws every second is a row nobody reads twice.
func TestTheDashboardHasNoReplayRowWhenNothingIsReplaying(t *testing.T) {
	fakeRecon(t, capture.ReconstructionState{})

	view := newStatusModel().View()

	if strings.Contains(view, "replaying") {
		t.Errorf("an idle machine printed a replay row:\n%s", view)
	}
}

// The scan walks both transcript trees and stats every in-window file. The
// dashboard ticks once a second; running it per tick would spend that walk 3,600
// times an hour to move a number that changes over hours.
func TestTheDashboardScansHistoryOnAScheduleNotEveryTick(t *testing.T) {
	calls := fakeRecon(t, replayingState())

	var m tea.Model = newStatusModel() // one scan at construction
	if *calls != 1 {
		t.Fatalf("newStatusModel did %d scans, want 1", *calls)
	}

	for i := 0; i < reconEveryTicks-1; i++ {
		m, _ = m.Update(statusTickMsg(time.Now()))
	}
	if *calls != 1 {
		t.Errorf("scanned %d times over %d ticks, want 1 — a per-tick directory walk is "+
			"the cost this schedule exists to avoid", *calls, reconEveryTicks-1)
	}

	m.Update(statusTickMsg(time.Now()))
	if *calls != 2 {
		t.Errorf("scanned %d times, want 2 — the number must still move while someone "+
			"watches it, or the dashboard is a screenshot", *calls)
	}
}

// `r` means "tell me now". Making the explicit refresh wait out the schedule
// would make the one key that promises freshness the one that does not deliver
// it.
func TestAnExplicitRefreshRescansHistoryImmediately(t *testing.T) {
	calls := fakeRecon(t, replayingState())

	var m tea.Model = newStatusModel()
	before := *calls

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})

	if *calls != before+1 {
		t.Errorf("`r` did %d scans, want 1 — an explicit refresh must not wait for the tick schedule",
			*calls-before)
	}
}
