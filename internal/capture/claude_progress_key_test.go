package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// claudeProjectsRoot points CLAUDE_CONFIG_DIR at a temp dir and returns the
// projects root, so tests can build realistic transcript paths.
func claudeProjectsRoot(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	root := filepath.Join(cfg, "projects")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestClaudeProgressKeyStripsProjectSlug is the core of bug 1: a transcript's
// identity is its session UUID, NOT its absolute path. The same session is
// filed under a worktree slug and the bare repo slug (and moves between them
// when a worktree is removed), so both must reduce to one key.
func TestClaudeProgressKeyStripsProjectSlug(t *testing.T) {
	root := claudeProjectsRoot(t)

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "worktree slug",
			path: filepath.Join(root, "-Users-me-repo--claude-worktrees-fix", "abc-123.jsonl"),
			want: "abc-123.jsonl",
		},
		{
			name: "bare repo slug, same session",
			path: filepath.Join(root, "-Users-me-repo", "abc-123.jsonl"),
			want: "abc-123.jsonl",
		},
		{
			name: "subagent sidechain keeps its nested path",
			path: filepath.Join(root, "-Users-me-repo", "abc-123", "subagents", "agent-7.jsonl"),
			want: "abc-123/subagents/agent-7.jsonl",
		},
		{
			name: "subagent under the worktree slug maps to the same key",
			path: filepath.Join(root, "-Users-me-repo--claude-worktrees-fix", "abc-123", "subagents", "agent-7.jsonl"),
			want: "abc-123/subagents/agent-7.jsonl",
		},
		{
			name: "path outside the projects dir falls back to itself",
			path: "/somewhere/else/abc-123.jsonl",
			want: "/somewhere/else/abc-123.jsonl",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeProgressKey(tc.path); got != tc.want {
				t.Errorf("claudeProgressKey(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestTwoProjectSlugsShareOneProgressKey is the direct regression test for the
// 2x emission: 25 tracked paths were worktree slugs that vanished and
// reappeared under the bare-repo slug, and the path-keyed watcher re-read each
// from offset 0 and re-emitted everything (2,182 duplicate events, ~32% of
// traffic — the 429 storm). Both aliases must resolve to ONE key, so the second
// one inherits the first's offset and reads nothing new.
func TestTwoProjectSlugsShareOneProgressKey(t *testing.T) {
	root := claudeProjectsRoot(t)
	const session = "9f8e7d6c-dead-beef-0000-111122223333.jsonl"

	worktreeSlug := filepath.Join(root, "-Users-me-repo--claude-worktrees-mutable-beacon", session)
	bareSlug := filepath.Join(root, "-Users-me-repo", session)

	kw, kb := claudeProgressKey(worktreeSlug), claudeProgressKey(bareSlug)
	if kw != kb {
		t.Fatalf("two slugs of one transcript must share a key: %q != %q", kw, kb)
	}

	// And the shared key must actually carry the offset across the rename.
	p := claudeWatchProgress{Offsets: map[string]int64{kw: 4096}, Match: map[string]string{kw: "yes"}}
	if got := p.Offsets[claudeProgressKey(bareSlug)]; got != 4096 {
		t.Errorf("offset must survive the slug change; got %d, want 4096 (a 0 here re-emits the whole transcript)", got)
	}
}

// TestMigrateClaudeProgressKeys pins the upgrade path: progress files on disk
// hold absolute-path keys. They must be re-keyed on load, and a collision must
// keep the MAX offset — keeping the min would re-read the difference and
// re-emit exactly the duplicates being fixed.
func TestMigrateClaudeProgressKeys(t *testing.T) {
	root := claudeProjectsRoot(t)
	abs := func(slug, name string) string { return filepath.Join(root, slug, name) }

	tests := []struct {
		name        string
		in          claudeWatchProgress
		wantOffsets map[string]int64
		wantMatch   map[string]string
	}{
		{
			name: "absolute keys are rewritten to slug-relative",
			in: claudeWatchProgress{
				Offsets: map[string]int64{abs("-Users-me-repo", "s1.jsonl"): 100},
				Match:   map[string]string{abs("-Users-me-repo", "s1.jsonl"): "yes"},
			},
			wantOffsets: map[string]int64{"s1.jsonl": 100},
			wantMatch:   map[string]string{"s1.jsonl": "yes"},
		},
		{
			name: "collision keeps the MAX offset (never re-read, never re-emit)",
			in: claudeWatchProgress{
				Offsets: map[string]int64{
					abs("-Users-me-repo--claude-worktrees-x", "s1.jsonl"): 4096,
					abs("-Users-me-repo", "s1.jsonl"):                     512,
				},
				Match: map[string]string{},
			},
			wantOffsets: map[string]int64{"s1.jsonl": 4096},
			wantMatch:   map[string]string{},
		},
		{
			name: "collision keeps MAX regardless of map iteration order",
			in: claudeWatchProgress{
				Offsets: map[string]int64{
					abs("-Users-me-repo", "s1.jsonl"):                     512,
					abs("-Users-me-repo--claude-worktrees-x", "s1.jsonl"): 4096,
					abs("-Users-me-repo--claude-worktrees-y", "s1.jsonl"): 2048,
				},
				Match: map[string]string{},
			},
			wantOffsets: map[string]int64{"s1.jsonl": 4096},
			wantMatch:   map[string]string{},
		},
		{
			name: "already-relative keys are left alone (migration is idempotent)",
			in: claudeWatchProgress{
				Offsets: map[string]int64{"s1.jsonl": 77},
				Match:   map[string]string{"s1.jsonl": "no"},
			},
			wantOffsets: map[string]int64{"s1.jsonl": 77},
			wantMatch:   map[string]string{"s1.jsonl": "no"},
		},
		{
			name: "match conflict resolves to yes (a cached no loses the session forever)",
			in: claudeWatchProgress{
				Offsets: map[string]int64{},
				Match: map[string]string{
					abs("-Users-me-repo", "s1.jsonl"):                     "yes",
					abs("-Users-me-repo--claude-worktrees-x", "s1.jsonl"): "no",
				},
			},
			wantOffsets: map[string]int64{},
			wantMatch:   map[string]string{"s1.jsonl": "yes"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := migrateClaudeProgressKeys(tc.in)
			if len(got.Offsets) != len(tc.wantOffsets) {
				t.Fatalf("offsets = %v, want %v", got.Offsets, tc.wantOffsets)
			}
			for k, want := range tc.wantOffsets {
				if got.Offsets[k] != want {
					t.Errorf("offset[%q] = %d, want %d", k, got.Offsets[k], want)
				}
			}
			for k, want := range tc.wantMatch {
				if got.Match[k] != want {
					t.Errorf("match[%q] = %q, want %q", k, got.Match[k], want)
				}
			}
		})
	}
}

// TestLoadClaudeWatchProgressMigratesOnDisk proves the migration actually runs
// on the real load path, against a progress file written by the old build.
func TestLoadClaudeWatchProgressMigratesOnDisk(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)

	oldKeyA := filepath.Join(root, "-Users-me-repo--claude-worktrees-gone", "s1.jsonl")
	oldKeyB := filepath.Join(root, "-Users-me-repo", "s1.jsonl")
	legacy := map[string]interface{}{
		"offsets": map[string]int64{oldKeyA: 9000, oldKeyB: 1000},
		"match":   map[string]string{oldKeyA: "yes", oldKeyB: "yes"},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeWatchProgressPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got := loadClaudeWatchProgress()
	if len(got.Offsets) != 1 {
		t.Fatalf("two aliases must collapse to one key; got %v", got.Offsets)
	}
	if got.Offsets["s1.jsonl"] != 9000 {
		t.Errorf("offset = %d, want 9000 (the MAX)", got.Offsets["s1.jsonl"])
	}
	if got.Match["s1.jsonl"] != "yes" {
		t.Errorf("match = %q, want \"yes\"", got.Match["s1.jsonl"])
	}
}

// TestPollDoesNotReEmitAcrossSlugs is the end-to-end regression: the SAME
// transcript content present under two slugs at once must be tailed exactly
// once. Before the fix each slug was a fresh path with offset 0, so the second
// one re-parsed and re-emitted every line.
func TestPollDoesNotReEmitAcrossSlugs(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	const uuid = "abc-123.jsonl"
	line := `{"type":"user","cwd":"` + workspace + `","timestamp":"` +
		time.Now().UTC().Format(time.RFC3339) + `","message":{"role":"user","content":"hello"}}` + "\n"

	for _, slug := range []string{"-worktree-slug", "-bare-slug"} {
		dir := filepath.Join(root, slug)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, uuid), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	session := Session{DeviceID: "sess-dup", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now().Add(-time.Minute)}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}

	// dryRun: count parses without queueing, so this measures the parser alone.
	parsed, _ := pollClaudeTranscripts(session, resolvePath(workspace),
		session.StartedAt.Add(-2*time.Minute), processors, true, false)

	// One line of content, present under two slugs => exactly one parse.
	if parsed != 1 {
		t.Errorf("two slugs of one transcript must parse once; got %d parsed events (2 = the duplicate-emission bug)", parsed)
	}
}
