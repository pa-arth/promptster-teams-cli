package capture

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// TestRegisterCaptureRootPersists proves the round trip: a registered directory
// survives into a fresh read, which is what lets a running daemon see it.
func TestRegisterCaptureRootPersists(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	a := t.TempDir()
	added, all, err := RegisterCaptureRoot(a)
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Error("first registration must report a change")
	}
	if len(all) != 1 || all[0] != resolvePath(a) {
		t.Errorf("want [%s]; got %v", resolvePath(a), all)
	}
	if got := RegisteredCaptureRoots(); len(got) != 1 || got[0] != resolvePath(a) {
		t.Errorf("re-read must return the registered root; got %v", got)
	}
}

// TestRegisterCaptureRootFoldsContainment: registering the same directory
// twice, or a child of one already registered, must not grow the list — and a
// broader root must SUBSUME the narrower ones it now covers. The printed
// confirmation is only worth printing if it describes the real scope.
func TestRegisterCaptureRootFoldsContainment(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	parent := t.TempDir()
	child := filepath.Join(parent, "repo")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, _, err := RegisterCaptureRoot(child); err != nil {
		t.Fatal(err)
	}
	if added, _, _ := RegisterCaptureRoot(child); added {
		t.Error("re-registering the same directory must report no change")
	}

	grandchild := filepath.Join(child, "nested")
	if err := os.MkdirAll(grandchild, 0o700); err != nil {
		t.Fatal(err)
	}
	added, all, _ := RegisterCaptureRoot(grandchild)
	if added {
		t.Error("a directory already covered by a registered root must not be added")
	}
	if len(all) != 1 {
		t.Errorf("covered registration must leave the list alone; got %v", all)
	}

	added, all, _ = RegisterCaptureRoot(parent)
	if !added {
		t.Error("a broader root is a real change")
	}
	if len(all) != 1 || all[0] != resolvePath(parent) {
		t.Errorf("broader root must subsume the child; got %v", all)
	}
}

// TestRegisterCaptureRootBounded: the list is capped, so a script that runs
// `start` from throwaway directories cannot grow it without limit (every root
// costs a `git worktree list` on every poll).
func TestRegisterCaptureRootBounded(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	base := t.TempDir()
	for i := 0; i < maxCaptureRoots+5; i++ {
		d := filepath.Join(base, "sib", string(rune('a'+i%26)), string(rune('a'+i/26)))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := RegisterCaptureRoot(d); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(RegisteredCaptureRoots()); got > maxCaptureRoots {
		t.Errorf("root list must stay bounded at %d; got %d", maxCaptureRoots, got)
	}
}

// TestCaptureRootsFingerprintIgnoresOrder: the fingerprint must describe the
// SET. Order-sensitivity would make it flip on every poll (workspaceMatchRoots
// emits git worktrees in git's order), dropping the whole match cache each time.
func TestCaptureRootsFingerprintIgnoresOrder(t *testing.T) {
	a := captureRootsFingerprint([]string{"/x", "/y", "/z"})
	b := captureRootsFingerprint([]string{"/z", "/x", "/y", "/x"})
	if a != b {
		t.Errorf("fingerprint must ignore order and duplicates: %s vs %s", a, b)
	}
	if c := captureRootsFingerprint([]string{"/x", "/y"}); c == a {
		t.Error("a genuinely different root set must fingerprint differently")
	}
}

// TestSyncMatchCacheToRoots pins BOTH halves of the invalidation decision.
//
// The "unchanged" case matters as much as the "changed" one and cannot be
// observed through a poll: the poll re-caches an unchanged "no" in the same
// pass, so invalidating on every poll produces an identical end state while
// re-parsing every mismatched file every 3 seconds. On a machine with
// thousands of rollouts that cache is the difference between a background
// daemon and a busy one.
func TestSyncMatchCacheToRoots(t *testing.T) {
	roots := []string{"/a", "/b"}
	fp := captureRootsFingerprint(roots)

	t.Run("unchanged roots keep the cache", func(t *testing.T) {
		m := map[string]string{"x": "no", "y": "yes"}
		got, dropped, changed := syncMatchCacheToRoots(m, fp, []string{"/b", "/a"})
		if changed || dropped != 0 {
			t.Errorf("an unchanged root set must not invalidate; changed=%v dropped=%d", changed, dropped)
		}
		if got != fp {
			t.Errorf("fingerprint must be stable; %s vs %s", got, fp)
		}
		if len(m) != 2 {
			t.Errorf("cache must be untouched; got %v", m)
		}
	})

	t.Run("widened roots revalidate every cached decision", func(t *testing.T) {
		m := map[string]string{"x": "no", "y": "yes", "z": "no"}
		got, dropped, changed := syncMatchCacheToRoots(m, fp, []string{"/a", "/b", "/c"})
		if !changed || dropped != 3 {
			t.Errorf("a changed root set must drop every cached decision; changed=%v dropped=%d", changed, dropped)
		}
		if got == fp {
			t.Error("the stored fingerprint must advance, or every later poll re-invalidates")
		}
		if len(m) != 0 {
			t.Errorf("all cached decisions must be revalidated; got %v", m)
		}
	})

	t.Run("narrowed roots invalidate accepted transcripts", func(t *testing.T) {
		m := map[string]string{"inside": "yes", "outside": "yes"}
		_, dropped, changed := syncMatchCacheToRoots(m, fp, []string{"/a"})
		if !changed || dropped != 2 {
			t.Fatalf("a narrowed root set must revalidate accepted transcripts; changed=%v dropped=%d", changed, dropped)
		}
		if len(m) != 0 {
			t.Fatalf("cached matches outlived the watched roots: %v", m)
		}
	})

	t.Run("first run adopts the fingerprint", func(t *testing.T) {
		m := map[string]string{"x": "no"}
		_, _, changed := syncMatchCacheToRoots(m, "", roots)
		if !changed {
			t.Error("an empty stored fingerprint must be treated as a change")
		}
	})
}

// TestWorkspaceMatchRootsIncludesRegisteredRoots is the core of the fix: a
// second tree that shares no repository with the workspace is matched once
// registered. Before this change workspaceMatchRoots returned the workspace and
// its git worktrees only, so this second tree was invisible to every watcher.
func TestWorkspaceMatchRootsIncludesRegisteredRoots(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	workspace := resolvePath(t.TempDir())
	second := resolvePath(t.TempDir())

	if got := workspaceMatchRoots(workspace); containsPath(got, second) {
		t.Fatalf("unregistered directory must not be matched; got %v", got)
	}
	if _, _, err := RegisterCaptureRoot(second); err != nil {
		t.Fatal(err)
	}
	roots := workspaceMatchRoots(workspace)
	if !containsPath(roots, workspace) {
		t.Errorf("the workspace itself must stay matched; got %v", roots)
	}
	if !containsPath(roots, second) {
		t.Errorf("registered root %s must be matched; got %v", second, roots)
	}
}

// TestClassifyCodexRolloutMatchesRegisteredRoot: the Codex matcher compared cwd
// against the single workspace, so a registered second tree stayed a mismatch
// there even after the Claude side saw it. Both watchers must agree on scope.
func TestClassifyCodexRolloutMatchesRegisteredRoot(t *testing.T) {
	root := codexSessionsRoot(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	workspace := resolvePath(t.TempDir())
	second := resolvePath(t.TempDir())
	dir := filepath.Join(root, "2026", "06", "12")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-06-12T09-00-00-019eb780-3081-7ce0-9ba0-8a0bad13b100.jsonl")
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(codexSessionMetaLine(second, ts)), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := classifyCodexRollout(path, []string{workspace}, time.Now().Add(-time.Minute)); got != codexMatchNo {
		t.Fatalf("a rollout outside every root must not match; got %v", got)
	}
	got := classifyCodexRollout(path, []string{workspace, second}, time.Now().Add(-time.Minute))
	if got != codexMatchYes {
		t.Errorf("a rollout whose cwd is inside a registered root must match; got %v", got)
	}
}

// TestPollCodexRescansMismatchesAfterRootRegistered is the end-to-end proof and
// the subtlest half of the fix: both watchers cache a per-file "no" FOREVER, so
// registering a directory whose sessions were already classified would have
// changed nothing for exactly the engineer this feature exists for — someone
// who starts capture in one tree and only later realizes the other is missing.
func TestPollCodexRescansMismatchesAfterRootRegistered(t *testing.T) {
	root := codexSessionsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := resolvePath(t.TempDir())
	second := resolvePath(t.TempDir())
	dir := filepath.Join(root, "2026", "06", "13")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-06-13T09-00-00-019eb780-3081-7ce0-9ba0-8a0bad13b200.jsonl")

	ts := time.Now().UTC().Format(time.RFC3339)
	line := codexSessionMetaLine(second, ts) +
		`{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"user_message","message":"work in the second tree","images":[]}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	session := Session{DeviceID: "sess-roots", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	cutoff := session.StartedAt.Add(-2 * time.Minute)
	processors := map[string]*normalize.CodexRolloutProcessor{}

	// First poll: the second tree is not registered yet, so the rollout is a
	// mismatch and that "no" is cached.
	if sent := pollCodexRollouts(session, workspace, cutoff, processors, false); sent != 0 {
		t.Fatalf("work outside every capture root must not be captured; got %d", sent)
	}
	if got := loadCodexWatchProgress().Match[path]; got != "no" {
		t.Fatalf("mismatch must be cached; got %q", got)
	}

	// The engineer runs `start` in the second tree.
	if _, _, err := RegisterCaptureRoot(second); err != nil {
		t.Fatal(err)
	}

	if sent := pollCodexRollouts(session, workspace, cutoff, processors, false); sent == 0 {
		t.Error("registering the second tree must resurrect its cached mismatch; nothing was captured")
	}
	if got := loadCodexWatchProgress().Match[path]; got != "yes" {
		t.Errorf("re-classification must cache the match; got %q", got)
	}
}

// TestRegisterWatchDirUsesEnv proves the already-running path can widen capture
// WITHOUT resolving a credential — a second `start` from a shell with no key in
// scope must still hand its directory to the daemon that owns the credential.
func TestRegisterWatchDirUsesEnv(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_TEAMS_WATCH_DIR", dir)
	t.Setenv("PROMPTSTER_TEAMS_TOKEN", "")

	got, err := registerWatchDir()
	if err != nil {
		t.Fatalf("registration must succeed here: %v", err)
	}
	if got != dir {
		t.Errorf("want %s; got %s", dir, got)
	}
	if got := RegisteredCaptureRoots(); len(got) != 1 || got[0] != resolvePath(dir) {
		t.Errorf("the watch dir must be registered; got %v", got)
	}
}

func containsPath(haystack []string, want string) bool {
	for _, h := range haystack {
		if h == want {
			return true
		}
	}
	return false
}

// TestRegisterCaptureRootReportsWriteFailure: a registration that cannot be
// persisted must say so. Callers print "capture now covers <dir>" off this
// result, and a failure that reads as success recreates the exact bug capture
// roots exist to fix — an engineer told their tree is covered when it is not.
func TestRegisterCaptureRootReportsWriteFailure(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)

	// Make the roots path itself a directory: every write and rename against it
	// fails, standing in for a full disk or a read-only state dir.
	if err := os.MkdirAll(captureRootsPath(), 0o700); err != nil {
		t.Fatal(err)
	}

	added, _, err := RegisterCaptureRoot(t.TempDir())
	if err == nil {
		t.Error("an unwritable roots file must return an error, not silent success")
	}
	if added {
		t.Error("a failed registration must not report that the set changed")
	}
}

// TestRegisterCaptureRootConcurrent: two `start` invocations racing in
// different trees must both end up captured. Read-modify-write on a shared set
// without serialization drops the loser's directory — silently uncaptured,
// which is the whole bug.
func TestRegisterCaptureRootConcurrent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	base := t.TempDir()
	dirs := make([]string, 8)
	for i := range dirs {
		d := filepath.Join(base, "tree", string(rune('a'+i)))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		dirs[i] = resolvePath(d)
	}

	var wg sync.WaitGroup
	for _, d := range dirs {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			if _, _, err := RegisterCaptureRoot(d); err != nil {
				t.Errorf("register %s: %v", d, err)
			}
		}(d)
	}
	wg.Wait()

	got := RegisteredCaptureRoots()
	for _, d := range dirs {
		if !containsPath(got, d) {
			t.Errorf("concurrent registration lost %s; got %v", d, got)
		}
	}
}
