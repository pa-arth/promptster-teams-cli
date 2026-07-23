package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// attributedShas returns every commit SHA that has a commit_attribution event on
// the outbox, in order — so a test can assert on REPEATS, not just presence.
func attributedShas(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read outbox: %v", err)
	}
	var shas []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev event.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal outbox line: %v", err)
		}
		if ev.Kind != "commit_attribution" {
			continue
		}
		d, ok := ev.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("commit_attribution Data is %T, want map", ev.Data)
		}
		sha, _ := d["commitSha"].(string)
		shas = append(shas, sha)
	}
	return shas
}

// countSha reports how many times a SHA was attributed.
func countSha(shas []string, want string) int {
	n := 0
	for _, s := range shas {
		if s == want {
			n++
		}
	}
	return n
}

// assertNoRepeats is THE invariant these tests exist for: a commit SHA must
// never be attributed twice by the same device, no matter how many times a poll
// re-detects it. It is deliberately not a raw event count — the recovery window
// legitimately surfaces commits for the FIRST time (the repo's pre-existing
// history, which cold start skipped on purpose), and counting events would
// conflate that correct behaviour with the re-emission bug.
func assertNoRepeats(t *testing.T, shas []string) {
	t.Helper()
	seen := map[string]int{}
	for _, s := range shas {
		seen[s]++
	}
	for sha, n := range seen {
		if n > 1 {
			t.Errorf("commit %s attributed %d times, want exactly 1 (full outbox: %v)", sha, n, shas)
		}
	}
}

// daemonWatchFixture stands up the daemon-mode shape used by these tests: a
// non-repo HOME as TaskRoot with a real repo under it, one AI-authored commit
// already attributed by a first poll. Returns the session and that commit's SHA.
func daemonWatchFixture(t *testing.T) (Session, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	if _, err := sign.GenerateSessionKeypair(); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	repo := filepath.Join(home, "repos", "proj")
	git, gitOut := gitRepoAt(t, repo)
	writeCommitFile(t, repo, "foo.go", "package main\n")
	git("add", "-A")
	git("commit", "-m", "baseline")

	session := Session{DeviceID: "dev-reemit", TaskRoot: home}
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")
	pollGitWatchWorkspace(session) // cold-start baseline, emits nothing

	writeCommitFile(t, repo, "bar.go", "package main\n\nfunc bar() {}\n")
	git("add", "-A")
	git("commit", "-m", "ai adds bar")
	sha := gitOut("rev-parse", "HEAD")
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/bar.go")

	pollGitWatchWorkspace(session)
	if got := attributedShas(t, state.OutboxPath()); len(got) != 1 || got[0] != sha {
		t.Fatalf("setup: want exactly one attribution for %s, got %v", sha, got)
	}
	return session, sha
}

// TestAttributionNotRepeatedWhenCursorUnreachable is the regression for the
// commit_attribution flood measured on teams prod (125,877 POSTs storing 4,173
// useful rows; one burst of 7,482 events 0.1s apart).
//
// A cursor becomes unreachable whenever history is rewritten under it — a
// rebase, a deleted worktree, a gc. gitNewCommits then falls back to
// `rev-list -n <cap> head`, which re-surfaces the newest commits WHOLESALE, and
// every one of them has typically already been attributed. "Detected" means HEAD
// moved relative to a cursor; it does not mean "not yet attributed". Before the
// attributed-commits ledger, that difference put the same SHA on the wire again
// on every such poll.
func TestAttributionNotRepeatedWhenCursorUnreachable(t *testing.T) {
	session, sha := daemonWatchFixture(t)

	// Point the cursor at a SHA that is not an object in the repo — exactly what a
	// rebase or a pruned worktree leaves behind. `rev-list <bogus>..HEAD` errors,
	// so gitNewCommits takes its recovery window and re-surfaces `sha`.
	cursors := loadGitWatchCursors()
	if len(cursors) == 0 {
		t.Fatal("expected at least one persisted cursor")
	}
	bogus := map[string]string{}
	for key := range cursors {
		bogus[key] = "0000000000000000000000000000000000000000"
	}
	saveGitWatchCursors(bogus)

	pollGitWatchWorkspace(session)

	got := attributedShas(t, state.OutboxPath())
	assertNoRepeats(t, got)
	if countSha(got, sha) != 1 {
		t.Fatalf("recovery poll re-attributed %s: it appears %d times in %v, want 1",
			sha, countSha(got, sha), got)
	}
}

// TestAttributionLedgerSurvivesRepeatedRecoveryPolls: the ledger must hold across
// MANY polls, not just the next one — the prod bursts were sustained, not a
// single duplicate. Each poll re-arms the unreachable cursor, so every one of
// them takes the recovery path.
func TestAttributionLedgerSurvivesRepeatedRecoveryPolls(t *testing.T) {
	session, sha := daemonWatchFixture(t)

	for i := 0; i < 5; i++ {
		cursors := loadGitWatchCursors()
		bogus := map[string]string{}
		for key := range cursors {
			bogus[key] = "0000000000000000000000000000000000000000"
		}
		saveGitWatchCursors(bogus)
		pollGitWatchWorkspace(session)
	}

	got := attributedShas(t, state.OutboxPath())
	assertNoRepeats(t, got)
	if countSha(got, sha) != 1 {
		t.Fatalf("after 5 recovery polls %s appears %d times in %v, want 1", sha, countSha(got, sha), got)
	}
}

// TestAttributionStillEmitsGenuinelyNewCommit guards the other direction: the
// ledger must suppress REPEATS without suppressing new work. A commit the device
// has never attributed still goes out, even on a poll that also re-surfaces
// already-attributed commits through the recovery window.
func TestAttributionStillEmitsGenuinelyNewCommit(t *testing.T) {
	session, first := daemonWatchFixture(t)

	repo := filepath.Join(session.TaskRoot, "repos", "proj")
	git, gitOut := gitRepoAt(t, repo)
	writeCommitFile(t, repo, "baz.go", "package main\n\nfunc baz() {}\n")
	git("add", "-A")
	git("commit", "-m", "ai adds baz")
	second := gitOut("rev-parse", "HEAD")
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(session.TaskRoot), "repos/proj/baz.go")

	// Force the recovery window so the poll surfaces BOTH commits: the already
	// attributed one and the new one.
	cursors := loadGitWatchCursors()
	bogus := map[string]string{}
	for key := range cursors {
		bogus[key] = "0000000000000000000000000000000000000000"
	}
	saveGitWatchCursors(bogus)

	pollGitWatchWorkspace(session)

	got := attributedShas(t, state.OutboxPath())
	assertNoRepeats(t, got)
	if countSha(got, first) != 1 {
		t.Errorf("already-attributed %s appears %d times in %v, want 1", first, countSha(got, first), got)
	}
	if countSha(got, second) != 1 {
		t.Errorf("genuinely new commit %s appears %d times in %v, want 1 (the ledger must not suppress new work)",
			second, countSha(got, second), got)
	}
}

// TestAttributedCommitsLedgerEvictsOldestFirst: the file is hard-bounded, and
// when it overflows the OLDEST entries go — the newest SHAs are the ones a
// recovery window re-surfaces, so they are the ones worth remembering.
func TestAttributedCommitsLedgerEvictsOldestFirst(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	orig := attributedCommitsMax
	attributedCommitsMax = 5
	defer func() { attributedCommitsMax = orig }()

	sha := func(i int) string { return fmt.Sprintf("sha%04d", i) }
	now := int64(1_700_000_000_000)
	// Write more than the cap, oldest first, then confirm the survivors are the
	// newest ones and the count is exactly the cap.
	total := attributedCommitsMax + 10
	for i := 0; i < total; i++ {
		recordAttributedCommits([]string{sha(i)}, now+int64(i))
	}
	seen := loadAttributedCommits(now + int64(total))
	if len(seen) != attributedCommitsMax {
		t.Fatalf("ledger holds %d entries, want the cap %d", len(seen), attributedCommitsMax)
	}
	if _, present := seen[sha(0)]; present {
		t.Errorf("oldest entry survived eviction")
	}
	if _, present := seen[sha(total-1)]; !present {
		t.Errorf("newest entry was evicted")
	}
}

// TestAttributedCommitsLedgerExpiresPastTTL: a SHA older than the TTL is
// forgotten, so the file cannot grow without bound on a long-lived device.
func TestAttributedCommitsLedgerExpiresPastTTL(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	now := int64(1_700_000_000_000)
	recordAttributedCommits([]string{"deadbeef"}, now)
	if seen := loadAttributedCommits(now); len(seen) != 1 {
		t.Fatalf("fresh entry missing: %v", seen)
	}
	if seen := loadAttributedCommits(now + attributedCommitTTLMs + 1); len(seen) != 0 {
		t.Fatalf("entry past the TTL is still remembered: %v", seen)
	}
}
