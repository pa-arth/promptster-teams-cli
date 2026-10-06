package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// TestAttributionNotDuplicatedAcrossWorktreesInOnePoll (Greptile P1): the same
// commit is reachable from a repo AND from each of its worktrees, which are
// SEPARATE roots in the same poll loop. The ledger snapshot is loaded once
// before the loop, so unless a just-emitted SHA is added to the in-memory set
// immediately, every root emits it again within that single poll — the exact
// duplication the SHA-keyed design claims to prevent.
func TestAttributionNotDuplicatedAcrossWorktreesInOnePoll(t *testing.T) {
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

	// A detached worktree of the SAME repo, so both roots can sit on one commit.
	wt := filepath.Join(home, "repos", "proj-wt")
	git("worktree", "add", "--detach", wt, "HEAD")

	session := Session{DeviceID: "dev-wt", TaskRoot: home}
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj-wt/foo.go")
	pollGitWatchWorkspace(session) // cold-start baseline for both roots

	writeCommitFile(t, repo, "bar.go", "package main\n\nfunc bar() {}\n")
	git("add", "-A")
	git("commit", "-m", "ai adds bar")
	sha := gitOut("rev-parse", "HEAD")
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/bar.go")
	// Move the worktree onto the very same commit, so ONE poll detects it twice.
	git("-C", wt, "checkout", "--detach", sha)

	pollGitWatchWorkspace(session)

	got := attributedShas(t, state.OutboxPath())
	assertNoRepeats(t, got)
	if countSha(got, sha) != 1 {
		t.Fatalf("commit %s reachable from repo AND worktree was attributed %d times in one poll (%v), want 1",
			sha, countSha(got, sha), got)
	}
}

// TestFailedEnqueueIsNotRecordedAsAttributed (Greptile P1): emitCommitAttribution
// used to swallow a queue failure, so a commit that never reached the outbox was
// still written to the ledger — suppressing the retry for the ledger's whole TTL
// and losing that attribution permanently. A failed enqueue must leave the SHA
// unrecorded so the next poll tries again.
func TestFailedEnqueueIsNotRecordedAsAttributed(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	// An outbox path that is a DIRECTORY: every append fails to open it.
	blocked := filepath.Join(tmp, "blocked-outbox")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMPTSTER_OUTBOX_PATH", blocked)
	if _, err := sign.GenerateSessionKeypair(); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	repo := filepath.Join(home, "repos", "proj")
	git, gitOut := gitRepoAt(t, repo)
	writeCommitFile(t, repo, "foo.go", "package main\n")
	git("add", "-A")
	git("commit", "-m", "baseline")

	session := Session{DeviceID: "dev-fail", TaskRoot: home}
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")
	pollGitWatchWorkspace(session)

	writeCommitFile(t, repo, "bar.go", "package main\n\nfunc bar() {}\n")
	git("add", "-A")
	git("commit", "-m", "ai adds bar")
	sha := gitOut("rev-parse", "HEAD")
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/bar.go")

	nowMs := int64(1_700_000_000_000)
	pollGitWatchWorkspace(session)

	if seen := loadAttributedCommits(nowMs); len(seen) != 0 {
		t.Fatalf("a commit whose enqueue FAILED was recorded as attributed (%v) — the retry is now "+
			"suppressed for the ledger TTL and the attribution is lost", seen)
	}

	// With a working outbox the retry must actually go out.
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	cursors := loadGitWatchCursors()
	bogus := map[string]string{}
	for key := range cursors {
		bogus[key] = "0000000000000000000000000000000000000000"
	}
	saveGitWatchCursors(bogus)
	pollGitWatchWorkspace(session)

	if got := attributedShas(t, state.OutboxPath()); countSha(got, sha) != 1 {
		t.Fatalf("after the outbox recovered, %s appears %d times in %v, want exactly 1", sha, countSha(got, sha), got)
	}
}

// TestBashEditedSiblingWorktreeIsPolled: an agent that edits a worktree only
// through Bash records no ai-paths entry there, so discovery used to find the
// repo's main checkout and never the worktree — its commits were never
// attributed (prod 2026-10-05: 37 of 82 merged PRs in a week). The sibling must
// be polled, and the bash-window recovery pass must credit the commit to the
// session whose Bash command wrote the file.
func TestBashEditedSiblingWorktreeIsPolled(t *testing.T) {
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
	wt := filepath.Join(home, "repos", "proj-feature")
	git("worktree", "add", "-b", "feature", wt)

	session := Session{DeviceID: "dev-sib", TaskRoot: home}
	// AI evidence exists ONLY for the main checkout.
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")
	pollGitWatchWorkspace(session) // cold-start baseline

	start := time.Now().UnixMilli()
	writeCommitFile(t, wt, "bar.go", "package main\n\nfunc bar() {}\n")
	recordBashWindow("bash-sess", gitWatchRootKey(home), start, time.Now().UnixMilli())
	git("-C", wt, "add", "-A")
	git("-C", wt, "commit", "-m", "agent adds bar via bash")
	sha := strings.TrimSpace(gitOut("-C", wt, "rev-parse", "HEAD"))

	pollGitWatchWorkspace(session)

	if countSha(attributedShas(t, state.OutboxPath()), sha) != 1 {
		t.Fatalf("commit %s in a Bash-edited sibling worktree was not attributed", sha)
	}
	out, err := os.ReadFile(state.OutboxPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"sessionId":"bash-sess"`) {
		t.Fatalf("sibling-worktree commit not credited to the Bash session")
	}
}

func aiCommandEvent(sessionID, command string) *event.Event {
	e := event.NewEvent("command", sessionID)
	e.Ts = time.Now().UTC().Format(time.RFC3339Nano)
	e.Data = map[string]interface{}{"command": command}
	e.Provenance = &event.Provenance{Attribution: "likely_ai"}
	return &e
}

// TestBashOnlyRepoIsDiscoveredFromPromptWorkdir: a repo that NO Edit/Write ever
// touched (prod: the teams-cli checkout had zero ai-paths entries) is found
// through the session's prompt workdir, and its commit is credited.
func TestBashOnlyRepoIsDiscoveredFromPromptWorkdir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))
	if _, err := sign.GenerateSessionKeypair(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	repo := filepath.Join(home, "repos", "cli")
	git, gitOut := gitRepoAt(t, repo)
	writeCommitFile(t, repo, "foo.go", "package main\n")
	git("add", "-A")
	git("commit", "-m", "baseline")

	session := Session{DeviceID: "dev-cwd", TaskRoot: home}
	prompt := event.NewEvent("prompt", "bash-sess")
	prompt.Ts = time.Now().UTC().Format(time.RFC3339Nano)
	prompt.Data = map[string]interface{}{"workdir": repo}
	recordAiBashWindow(&prompt, home, false)
	pollGitWatchWorkspace(session) // cold-start baseline — only possible if discovered

	writeCommitFile(t, repo, "bar.go", "package main\n\nfunc bar() {}\n")
	recordAiBashWindow(aiCommandEvent("bash-sess", "sed -i s/a/b/ bar.go"), home, false)
	git("add", "-A")
	git("commit", "-m", "bash edit")
	sha := strings.TrimSpace(gitOut("rev-parse", "HEAD"))
	pollGitWatchWorkspace(session)

	if countSha(attributedShas(t, state.OutboxPath()), sha) != 1 {
		t.Fatalf("commit in a repo known only from the prompt workdir was not attributed")
	}
}

// TestBashRecoveryCreditsTheSessionThatWorkedThere: two agents run Bash at the
// same instant; only one of them worked in this checkout. The nearer window
// belongs to the other — it must not win.
func TestBashRecoveryCreditsTheSessionThatWorkedThere(t *testing.T) {
	dir := t.TempDir()
	writeCommitFile(t, dir, "x.go", "package x\n")
	info, err := os.Stat(filepath.Join(dir, "x.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := info.ModTime().UnixMilli()
	got, ok := recoverBashSession(dir, "x.go", []bashWindow{
		{SessionID: "elsewhere", StartMs: m, EndMs: m, Roots: []string{"/some/other/checkout"}},
		{SessionID: "here", StartMs: m + 2000, EndMs: m + 2000, Roots: []string{resolvePath(dir)}},
	})
	if !ok || got != "here" {
		t.Fatalf("credited %q (ok=%v), want the session that worked in this checkout", got, ok)
	}
}

// TestWorktreeCutAndCommittedBetweenPollsIsAttributed (Greptile, #270): a
// worktree that appears and gets its first commit between two polls used to be
// cold-started at that commit and never reported.
func TestWorktreeCutAndCommittedBetweenPollsIsAttributed(t *testing.T) {
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
	session := Session{DeviceID: "dev-late", TaskRoot: home}
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")
	pollGitWatchWorkspace(session) // daemon now holds cursors

	wt := filepath.Join(home, "repos", "proj-late")
	git("worktree", "add", "-b", "late", wt)
	writeCommitFile(t, wt, "bar.go", "package main\n\nfunc bar() {}\n")
	git("-C", wt, "add", "-A")
	git("-C", wt, "commit", "-m", "first commit before any poll saw the worktree")
	sha := strings.TrimSpace(gitOut("-C", wt, "rev-parse", "HEAD"))

	pollGitWatchWorkspace(session)
	if countSha(attributedShas(t, state.OutboxPath()), sha) != 1 {
		t.Fatalf("commit made before the worktree's first poll was skipped")
	}
}

// TestBashCommandOutsideCaptureScopeAddsNoRoot (Greptile, #270): naming a repo
// outside the workspace in a command must not widen capture.
func TestBashCommandOutsideCaptureScopeAddsNoRoot(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	inside := filepath.Join(home, "repos", "in")
	outside := filepath.Join(t.TempDir(), "elsewhere")
	gitRepoAt(t, inside)
	gitRepoAt(t, outside)
	got := commandCheckouts("ls "+outside+" "+inside, "", home)
	if len(got) != 1 || got[0] != resolvePath(inside) {
		t.Fatalf("roots = %v, want only the in-scope %s", got, inside)
	}
}

// TestBashWindowKeepsItsOwnCheckout (Greptile, #270): a session that later works
// in checkout B must not make its earlier checkout-A command eligible for B.
func TestBashWindowKeepsItsOwnCheckout(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	a := filepath.Join(home, "repos", "a")
	b := filepath.Join(home, "repos", "b")
	gitRepoAt(t, a)
	gitRepoAt(t, b)
	recordAiBashWindow(aiCommandEvent("s", "cd "+a+" && sed -i x f"), home, false)
	recordAiBashWindow(aiCommandEvent("s", "cd "+b+" && sed -i x f"), home, false)
	ws := readBashWindows(gitWatchRootKey(home))
	if len(ws) != 2 {
		t.Fatalf("windows = %d, want 2", len(ws))
	}
	for _, w := range ws {
		if len(w.Roots) != 1 {
			t.Fatalf("window roots = %v, want exactly its own checkout", w.Roots)
		}
	}
}

// TestEmptyDotGitIsNotARepo: a stray empty `.git` dir (seen at /tmp) made every
// path beneath it resolve to a fake repo root.
func TestEmptyDotGitIsNotARepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isGitRepoRoot(dir) {
		t.Fatal("an empty .git dir was treated as a repository")
	}
}

// lateWorktreeFixture: a daemon already holding cursors, then a worktree cut and
// given two commits before any poll sees it. Returns the session, the worktree
// and its commits (oldest first).
func lateWorktreeFixture(t *testing.T) (Session, string, []string, func(...string), func(...string) string) {
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
	session := Session{DeviceID: "dev-late", TaskRoot: home}
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")
	pollGitWatchWorkspace(session)

	wt := filepath.Join(home, "repos", "proj-late2")
	git("worktree", "add", "-b", "late2", wt)
	var shas []string
	for _, f := range []string{"a.go", "b.go"} {
		writeCommitFile(t, wt, f, "package main\n")
		git("-C", wt, "add", "-A")
		git("-C", wt, "commit", "-m", f)
		shas = append(shas, strings.TrimSpace(gitOut("-C", wt, "rev-parse", "HEAD")))
	}
	return session, wt, shas, git, gitOut
}

// TestLateWorktreeReplayIgnoresMergedInHistory (Greptile, #270): an older branch
// merged into the new worktree is not its recent work.
func TestLateWorktreeReplayIgnoresMergedInHistory(t *testing.T) {
	session, wt, shas, git, gitOut := lateWorktreeFixture(t)
	// An older side branch, merged into the worktree.
	git("-C", wt, "checkout", "-q", "-b", "side", shas[0]+"~1")
	writeCommitFile(t, wt, "side.go", "package main\n")
	git("-C", wt, "add", "-A")
	git("-C", wt, "commit", "-m", "side")
	side := strings.TrimSpace(gitOut("-C", wt, "rev-parse", "HEAD"))
	git("-C", wt, "checkout", "-q", "late2")
	git("-C", wt, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	pollGitWatchWorkspace(session)
	if countSha(attributedShas(t, state.OutboxPath()), side) != 0 {
		t.Fatalf("merged-in side-branch commit %s was replayed as new work", side)
	}
}

// TestBashWindowKeepsTheWorkdirAlongsideNamedPaths (Greptile, #270):
// `git -C /B status && sed -i … foo.go` edits foo.go in the workdir A.
func TestBashWindowKeepsTheWorkdirAlongsideNamedPaths(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	a := filepath.Join(home, "repos", "a")
	b := filepath.Join(home, "repos", "b")
	gitRepoAt(t, a)
	gitRepoAt(t, b)
	prompt := event.NewEvent("prompt", "s")
	prompt.Ts = time.Now().UTC().Format(time.RFC3339Nano)
	prompt.Data = map[string]interface{}{"workdir": a}
	recordAiBashWindow(&prompt, home, false)
	recordAiBashWindow(aiCommandEvent("s", "git -C "+b+" status && sed -i x foo.go"), home, false)
	ws := readBashWindows(gitWatchRootKey(home))
	if len(ws) != 1 || !slices.Contains(ws[0].Roots, resolvePath(a)) || !slices.Contains(ws[0].Roots, resolvePath(b)) {
		t.Fatalf("window roots = %v, want both the workdir and the named checkout", ws)
	}
}

// TestCdAwayCommandDoesNotClaimTheWorkdir (Greptile, #270): `cd /B && …` works
// in B only.
func TestCdAwayCommandDoesNotClaimTheWorkdir(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	a := filepath.Join(home, "repos", "a")
	b := filepath.Join(home, "repos", "b")
	gitRepoAt(t, a)
	gitRepoAt(t, b)
	prompt := event.NewEvent("prompt", "s")
	prompt.Ts = time.Now().UTC().Format(time.RFC3339Nano)
	prompt.Data = map[string]interface{}{"workdir": a}
	recordAiBashWindow(&prompt, home, false)
	recordAiBashWindow(aiCommandEvent("s", "cd "+b+" && sed -i x foo.go"), home, false)
	ws := readBashWindows(gitWatchRootKey(home))
	if len(ws) != 1 || len(ws[0].Roots) != 1 || ws[0].Roots[0] != resolvePath(b) {
		t.Fatalf("window roots = %v, want only %s", ws, b)
	}
}

func assertBothReportedOnce(t *testing.T, shas []string) {
	t.Helper()
	got := attributedShas(t, state.OutboxPath())
	for _, sha := range shas {
		if countSha(got, sha) != 1 {
			t.Fatalf("late-worktree commit %s reported %d times, want 1 (%v)", sha, countSha(got, sha), got)
		}
	}
}

// TestLateWorktreeReplayOverTheCapStaysOwed (Greptile, #270): more owed commits
// than one poll may report drain across polls, oldest first — never dropped.
func TestLateWorktreeReplayOverTheCapStaysOwed(t *testing.T) {
	session, _, shas, _, _ := lateWorktreeFixture(t)
	prev := gitWatchMaxCommitsPerPoll
	gitWatchMaxCommitsPerPoll = 1
	t.Cleanup(func() { gitWatchMaxCommitsPerPoll = prev })
	pollGitWatchWorkspace(session)
	got := attributedShas(t, state.OutboxPath())
	if countSha(got, shas[0]) != 1 || countSha(got, shas[1]) != 0 {
		t.Fatalf("first capped poll must report only the OLDEST owed commit: %v", got)
	}
	pollGitWatchWorkspace(session)
	assertBothReportedOnce(t, shas)
}

// TestLateWorktreeReplayOverBudgetStaysOwed: no budget this poll still saves the
// replay as owed; the next poll reports it.
func TestLateWorktreeReplayOverBudgetStaysOwed(t *testing.T) {
	session, _, shas, _, _ := lateWorktreeFixture(t)
	prev := gitWatchMaxCommitsPerPollTotal
	gitWatchMaxCommitsPerPollTotal = 0
	pollGitWatchWorkspace(session)
	gitWatchMaxCommitsPerPollTotal = prev
	pollGitWatchWorkspace(session)
	assertBothReportedOnce(t, shas)
}

// TestEditThenCdKeepsTheWorkdir (Greptile, #270): `sed … foo.go && cd /B` edits
// foo.go in the workdir; the cd only moves later segments.
func TestEditThenCdKeepsTheWorkdir(t *testing.T) {
	home := t.TempDir()
	a := filepath.Join(home, "repos", "a")
	b := filepath.Join(home, "repos", "b")
	gitRepoAt(t, a)
	gitRepoAt(t, b)
	got := commandCheckouts("sed -i x foo.go && cd "+b, resolvePath(a), home)
	if len(got) != 1 || got[0] != resolvePath(a) {
		t.Fatalf("checkouts = %v, want only the workdir %s", got, a)
	}
}

// TestWindowInNoCapturedCheckoutClaimsNothing (Greptile, #270): a command that
// ran only outside capture records that, rather than an empty list that
// recovery would read as "unknown, eligible everywhere".
func TestWindowInNoCapturedCheckoutClaimsNothing(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	a := filepath.Join(home, "repos", "a")
	gitRepoAt(t, a)
	outside := filepath.Join(t.TempDir(), "x")
	gitRepoAt(t, outside)
	recordAiBashWindow(aiCommandEvent("s", "cd "+outside+" && sed -i x f"), home, false)
	ws := readBashWindows(gitWatchRootKey(home))
	if len(ws) != 1 {
		t.Fatalf("windows = %d", len(ws))
	}
	writeCommitFile(t, a, "y.go", "package y\n")
	info, _ := os.Stat(filepath.Join(a, "y.go"))
	m := info.ModTime().UnixMilli()
	ws[0].StartMs, ws[0].EndMs = m, m
	if got, ok := recoverBashSession(a, "y.go", ws); ok {
		t.Fatalf("a window that ran outside capture claimed %q in a captured checkout", got)
	}
}
