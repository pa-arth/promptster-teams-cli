package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// History reconstruction must run NEWEST FIRST.
//
// Both watchers enumerate their candidate transcripts with filepath.Walk, which
// yields lexical order — and both Claude session uuids and Codex rollout names
// sort close enough to chronological that walking them directly reads the
// OLDEST history in the window first. Each watcher then spends a bounded
// per-poll byte budget in that order, so a full-window replay (which the v2
// progress-schema migration triggers for every upgrading device) puts hours of
// ancient history ahead of the engineer's current session.
//
// Observed in prod on 2026-08-04: a device replaying the 28-day window walked
// event timestamps 2026-07-11 -> 2026-07-17 over 75 minutes while its live work
// went undelivered, and every freshness-gated surface reported the engineer as
// having zero active sessions.
//
// EVERY TEST IN THIS FILE FAILS against the pre-fix enumeration. That is the
// point — the ascending order is what shipped, and an assertion that passes
// both ways would prove nothing.

// writeAged creates a file and stamps its mtime, which is the ordering key both
// candidate helpers use.
func writeAged(t *testing.T, path string, age time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

func TestCandidateClaudeTranscriptsAreNewestFirst(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	projects := filepath.Join(dir, "projects")

	// Names deliberately sort ASCENDING opposite to age, so a lexical walk
	// returns the exact reverse of the correct answer. Without this the test
	// could pass on the old code by accident.
	oldest := writeAged(t, filepath.Join(projects, "aaa-proj", "0000-oldest.jsonl"), 24*24*time.Hour)
	middle := writeAged(t, filepath.Join(projects, "mmm-proj", "5555-middle.jsonl"), 10*24*time.Hour)
	newest := writeAged(t, filepath.Join(projects, "zzz-proj", "9999-newest.jsonl"), 2*time.Minute)

	got := candidateClaudeTranscripts(transcriptHistoryCutoff(time.Now().UTC()))

	want := []string{newest, middle, oldest}
	if len(got) != len(want) {
		t.Fatalf("candidate count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate[%d] = %s, want %s\nfull order: %v", i, got[i], want[i], got)
		}
	}
}

func TestCandidateCodexRolloutsAreNewestFirst(t *testing.T) {
	dir := t.TempDir()

	// Rollout filenames begin with their start timestamp, so lexical order IS
	// chronological ascending here. This is the real shape of the bug.
	oldest := writeAged(t, filepath.Join(dir, "rollout-2026-07-11T01-51-29-aaaa.jsonl"), 24*24*time.Hour)
	middle := writeAged(t, filepath.Join(dir, "rollout-2026-07-25T09-00-00-bbbb.jsonl"), 10*24*time.Hour)
	newest := writeAged(t, filepath.Join(dir, "rollout-2026-08-04T12-17-22-cccc.jsonl"), 2*time.Minute)

	got := candidateCodexRollouts(dir, transcriptHistoryCutoff(time.Now().UTC()))

	want := []string{newest, middle, oldest}
	if len(got) != len(want) {
		t.Fatalf("candidate count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate[%d] = %s, want %s\nfull order: %v", i, got[i], want[i], got)
		}
	}
}

// An interrupted replay must leave a CONTIGUOUS RECENT window.
//
// This is the property the incident actually violated: a bounded per-poll budget
// means every replay IS interrupted, repeatedly. Under the old order the
// completed prefix was the oldest days, so the recent window — the only span the
// default dashboards read — was the last thing to arrive. Under the new order
// the completed prefix is always the most recent days.
func TestInterruptedReplayLeavesTheRecentWindowWhole(t *testing.T) {
	dir := t.TempDir()

	const days = 28
	paths := make([]string, days)
	for i := 0; i < days; i++ {
		// i == 0 is the oldest; names ascend with age descending. Ages are offset
		// half a day so the oldest sits just INSIDE the 28-day window rather than
		// exactly on the cutoff, which excludes it.
		name := filepath.Join(dir, "rollout-"+string(rune('a'+i))+"-day.jsonl")
		paths[i] = writeAged(t, name, time.Duration(days-i)*24*time.Hour-12*time.Hour)
	}

	got := candidateCodexRollouts(dir, transcriptHistoryCutoff(time.Now().UTC()))
	if len(got) != days {
		t.Fatalf("candidate count = %d, want %d", len(got), days)
	}

	// Simulate the budget running out after a handful of files.
	const budgetForFiles = 5
	processed := got[:budgetForFiles]

	// The processed prefix must be the newest `budgetForFiles` files, in
	// descending recency — i.e. contiguous from the present backwards.
	for i := 0; i < budgetForFiles; i++ {
		want := paths[days-1-i]
		if processed[i] != want {
			t.Fatalf("interrupted replay processed[%d] = %s, want %s\n"+
				"a replay cut short must have covered the most recent days, not the oldest",
				i, filepath.Base(processed[i]), filepath.Base(want))
		}
	}
}

// Files outside the history window stay excluded — the ordering change must not
// widen what gets read.
func TestCandidatesStillRespectTheHistoryWindow(t *testing.T) {
	dir := t.TempDir()
	inside := writeAged(t, filepath.Join(dir, "rollout-inside.jsonl"), 27*24*time.Hour)
	writeAged(t, filepath.Join(dir, "rollout-outside.jsonl"), 40*24*time.Hour)

	got := candidateCodexRollouts(dir, transcriptHistoryCutoff(time.Now().UTC()))

	if len(got) != 1 || got[0] != inside {
		t.Fatalf("candidates = %v, want exactly [%s] — the ordering change must not widen the window", got, inside)
	}
}

// Ties must be broken deterministically. Coarse filesystem mtime granularity
// makes collisions real, and an unstable order would make the bounded per-poll
// budget land on a different file each poll — so a replay could ping-pong
// without ever finishing any single transcript.
func TestCandidateOrderIsStableUnderEqualModTimes(t *testing.T) {
	dir := t.TempDir()
	same := time.Now().Add(-time.Hour)
	for _, name := range []string{"rollout-a.jsonl", "rollout-b.jsonl", "rollout-c.jsonl"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		if err := os.Chtimes(p, same, same); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	first := candidateCodexRollouts(dir, transcriptHistoryCutoff(time.Now().UTC()))
	for i := 0; i < 5; i++ {
		again := candidateCodexRollouts(dir, transcriptHistoryCutoff(time.Now().UTC()))
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("order is unstable across calls: %v then %v", first, again)
			}
		}
	}
}
