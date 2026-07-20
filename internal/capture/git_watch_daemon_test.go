package capture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// gitRepoAt initializes a git repo at an arbitrary dir (which need NOT be a temp
// root), so a test can place a repo UNDER a non-repo "home" and reproduce the
// autostart-daemon layout (TaskRoot == home, real work in a sub-repo). Returns a
// runner and a trimmed-stdout runner scoped to that dir.
func gitRepoAt(t *testing.T, dir string) (run func(args ...string), out func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	run = func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, o)
		}
	}
	out = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		o, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(o))
	}
	run("init")
	return run, out
}

// TestResolveLedgerScope pins the mapping from a polled repo root onto capture's
// workspace-anchored AI ledgers for the three cases that matter in production.
func TestResolveLedgerScope(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "repos", "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil { // must exist so resolvePath canonicalizes both sides consistently
		t.Fatal(err)
	}

	// 1. root == taskRoot (explicit-repo / dev / subcommand): identity — same key,
	//    no prefix, so behavior is byte-for-byte what it was before discovery.
	s := resolveLedgerScope(repo, repo)
	if s.aiKey != gitWatchRootKey(repo) || s.prefix != "" {
		t.Fatalf("root==taskRoot: got key=%q prefix=%q, want key=%q prefix=\"\"", s.aiKey, s.prefix, gitWatchRootKey(repo))
	}
	if got := s.ledgerPath("foo.go"); got != "foo.go" {
		t.Fatalf("identity ledgerPath = %q, want foo.go", got)
	}

	// 2. root UNDER taskRoot (the daemon: TaskRoot=home, repo discovered under it):
	//    read under the HOME key, look up with the sub-repo prefix.
	s = resolveLedgerScope(repo, home)
	if s.aiKey != gitWatchRootKey(home) {
		t.Fatalf("root-under-home: key=%q, want home key %q", s.aiKey, gitWatchRootKey(home))
	}
	if s.prefix != "repos/proj" {
		t.Fatalf("root-under-home: prefix=%q, want repos/proj", s.prefix)
	}
	if got := s.ledgerPath("foo.go"); got != "repos/proj/foo.go" {
		t.Fatalf("translated ledgerPath = %q, want repos/proj/foo.go", got)
	}

	// 3. root OUTSIDE taskRoot (a repo not under home — rare): capture stored its
	//    evidence UNDER the workspace key as an ABSOLUTE path (RelativizeEventPaths
	//    leaves out-of-workspace paths unrewritten), so the scope must still read
	//    under the HOME key and look the path up by its absolute form. Reading under
	//    the repo key would miss the evidence entirely.
	other := t.TempDir()
	s = resolveLedgerScope(other, home)
	if s.aiKey != gitWatchRootKey(home) {
		t.Fatalf("outside-home: key=%q, want home key %q (evidence was stored under it)", s.aiKey, gitWatchRootKey(home))
	}
	wantAbs := filepath.ToSlash(filepath.Join(resolvePath(other), "x.go"))
	if got := s.ledgerPath("x.go"); got != wantAbs {
		t.Fatalf("outside-home ledgerPath = %q, want absolute %q", got, wantAbs)
	}
}

// TestDiscoverAiRepoRoots: the AI-paths ledger IS the discovery source. A path
// recorded workspace(HOME)-relative resolves to the sub-repo that owns it via a
// stat-only walk to its .git — no git spawn, no new ledger.
func TestDiscoverAiRepoRoots(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	repo := filepath.Join(home, "repos", "proj")
	gitRepoAt(t, repo) // creates repo/.git

	writeCommitFile(t, repo, "foo.go", "package main\n")

	// Capture in daemon mode stores the path HOME-relative under the HOME root key.
	recordAiTouchedPath("sess-1", gitWatchRootKey(home), "repos/proj/foo.go")

	roots := discoverAiRepoRoots(home)
	if len(roots) != 1 {
		t.Fatalf("discovered %d roots, want 1: %v", len(roots), roots)
	}
	if resolvePath(roots[0]) != resolvePath(repo) {
		t.Fatalf("discovered root = %q, want the sub-repo %q", roots[0], repo)
	}

	// A path with no .git ancestor under home contributes nothing (no spurious root).
	recordAiTouchedPath("sess-2", gitWatchRootKey(home), "loose/note.txt")
	if roots := discoverAiRepoRoots(home); len(roots) != 1 {
		t.Fatalf("a non-repo path must not add a root, got %v", roots)
	}
}

// TestCommitAttributionTranslatesHomeAnchoredLedger is the core regression: when
// TaskRoot is the HOME dir (the autostart daemon) and the commit lands in a
// sub-repo, the AI evidence — stored home-relative under the HOME key — must still
// attribute likely_ai. Before the translation this attributed `unknown`, because
// the reconciler read under the repo key and matched repo-relative paths against
// home-relative ledger keys.
func TestCommitAttributionTranslatesHomeAnchoredLedger(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	repo := filepath.Join(home, "repos", "proj")
	git, gitOut := gitRepoAt(t, repo)

	writeCommitFile(t, repo, "foo.go", "package main\n\nfunc main() {}\n")
	git("add", "-A")
	git("commit", "-m", "add foo")
	sha := gitOut("rev-parse", "HEAD")

	// Daemon-mode capture: HOME-relative path under the HOME root key.
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")

	// Session mirrors the daemon: TaskRoot is HOME, the polled root is the sub-repo.
	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: home}, repo, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	if ev.SessionID != "ai-sess-1" {
		t.Errorf("sessionId = %q, want the AI session that touched the file", ev.SessionID)
	}
	f, present := filesByPath(t, ev)["foo.go"]
	if !present {
		t.Fatalf("foo.go missing from attribution")
	}
	for _, r := range f["lineRanges"].([]interface{}) {
		if rm := r.(map[string]interface{}); rm["attribution"] != attributionLikelyAI {
			t.Errorf("attribution = %v, want likely_ai (translation failed)", rm["attribution"])
		}
	}
}

// TestCommitAttributionOutsideWorkspace: a repo OUTSIDE the workspace (e.g. code
// kept in /work while home is the workspace). RelativizeEventPaths leaves such a
// path ABSOLUTE under the workspace key, so attribution must match by absolute
// path — reading under the repo key would miss it and attribute unknown.
func TestCommitAttributionOutsideWorkspace(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	repo := t.TempDir() // a sibling of home, NOT under it
	git, gitOut := gitRepoAt(t, repo)

	writeCommitFile(t, repo, "foo.go", "package main\n\nfunc main() {}\n")
	git("add", "-A")
	git("commit", "-m", "add foo")
	sha := gitOut("rev-parse", "HEAD")

	// Capture stored the out-of-workspace evidence ABSOLUTE, under the HOME key
	// (mirrors RelativizeEventPaths leaving a `..`-relative path unrewritten).
	absFoo := filepath.ToSlash(filepath.Join(resolvePath(repo), "foo.go"))
	recordAiTouchedPath("ai-sess-ext", gitWatchRootKey(home), absFoo)

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: home}, repo, sha)
	if !ok {
		t.Fatal("expected an emittable event")
	}
	if ev.SessionID != "ai-sess-ext" {
		t.Errorf("sessionId = %q, want ai-sess-ext (absolute-path match failed)", ev.SessionID)
	}
	f := filesByPath(t, ev)["foo.go"]
	for _, r := range f["lineRanges"].([]interface{}) {
		if rm := r.(map[string]interface{}); rm["attribution"] != attributionLikelyAI {
			t.Errorf("attribution = %v, want likely_ai", rm["attribution"])
		}
	}
}

// TestPollGitWatchWorkspaceDaemonMode is the end-to-end proof that the durability
// track actually fires in the installed daemon: TaskRoot is a non-repo HOME, the
// engineer's real repo lives under it, and a new AI-authored commit there produces
// a commit_attribution on the outbox. Before discovery, pollGitWatch([home])
// detected nothing and this emitted zero events.
func TestPollGitWatchWorkspaceDaemonMode(t *testing.T) {
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

	session := Session{DeviceID: "dev-daemon", TaskRoot: home}

	// The AI touched foo.go so discovery can find the repo; baseline poll records
	// HEAD and emits nothing (cold start).
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/foo.go")
	pollGitWatchWorkspace(session)
	if data, _ := os.ReadFile(state.OutboxPath()); len(data) != 0 {
		t.Fatalf("cold-start poll must emit nothing, got:\n%s", data)
	}

	// A new AI-authored commit lands in the sub-repo.
	writeCommitFile(t, repo, "bar.go", "package main\n\nfunc bar() {}\n")
	git("add", "-A")
	git("commit", "-m", "ai adds bar")
	shaBar := gitOut("rev-parse", "HEAD")
	recordAiTouchedPath("ai-sess-1", gitWatchRootKey(home), "repos/proj/bar.go")

	pollGitWatchWorkspace(session)

	ev := lastCommitAttribution(t, state.OutboxPath())
	data := ev.Data.(map[string]interface{})
	if data["commitSha"] != shaBar {
		t.Fatalf("commitSha = %v, want %s", data["commitSha"], shaBar)
	}
	f, present := filesByPath(t, ev)["bar.go"]
	if !present {
		t.Fatalf("bar.go missing from the daemon-mode attribution: %+v", data["files"])
	}
	for _, r := range f["lineRanges"].([]interface{}) {
		if rm := r.(map[string]interface{}); rm["attribution"] != attributionLikelyAI {
			t.Errorf("attribution = %v, want likely_ai", rm["attribution"])
		}
	}
}

// TestDiscoveredReposSurviveAiPathsExpiry: a repo persisted from an earlier poll
// keeps being polled even when the ai-paths ledger no longer references it — so a
// durability span maturing in a repo the engineer stopped AI-editing is still
// harvested. The persisted horizon is durability-scale (>30d), well past the
// 7-day ai-paths TTL that would otherwise drop the repo.
func TestDiscoveredReposSurviveAiPathsExpiry(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	repo := filepath.Join(home, "repos", "proj")
	gitRepoAt(t, repo)

	nowMs := int64(1_000_000_000_000)

	// The repo was discovered on an earlier poll and persisted, but its AI paths
	// have since aged out of the ai-paths ledger (nothing recorded there now).
	refreshDiscoveredRepos([]string{repo}, nowMs)

	// discoverAiRepoRoots (ai-paths only) finds nothing…
	if roots := discoverAiRepoRoots(home); len(roots) != 0 {
		t.Fatalf("ai-paths discovery should be empty, got %v", roots)
	}
	// …but the persisted ledger still yields the repo, both now and 20 days later
	// (past the 7-day ai-paths TTL, inside the durability horizon).
	if got := loadDiscoveredRepos(nowMs); len(got) != 1 || resolvePath(got[0]) != resolvePath(repo) {
		t.Fatalf("persisted repo not returned now: %v", got)
	}
	twentyDaysLater := nowMs + 20*24*60*60*1000
	if got := loadDiscoveredRepos(twentyDaysLater); len(got) != 1 {
		t.Fatalf("persisted repo dropped after 20d (inside horizon): %v", got)
	}
	// Well past the horizon, an idle repo ages out.
	if got := loadDiscoveredRepos(nowMs + discoveredRepoTTLMs + 1); len(got) != 0 {
		t.Fatalf("repo should have aged out past the horizon: %v", got)
	}
}

// lastCommitAttribution returns the last commit_attribution event on the outbox,
// failing if none was queued.
func lastCommitAttribution(t *testing.T, path string) event.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var found *event.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev event.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal outbox line: %v", err)
		}
		if ev.Kind == "commit_attribution" {
			e := ev
			found = &e
		}
	}
	if found == nil {
		t.Fatalf("no commit_attribution on the outbox:\n%s", data)
	}
	return *found
}
