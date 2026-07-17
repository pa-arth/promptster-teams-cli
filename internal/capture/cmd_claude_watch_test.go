package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// TestClassifyClaudeTranscriptCutoffRouting pins the go-forward capture-time
// gate: cwd is authoritative, and the first line's timestamp only routes a
// matched session to new-vs-preexisting. A cwd match started BEFORE the cutoff
// must NOT be dropped (the bug that silently lost every restart-spanning
// session) — it is captured go-forward from EOF as claudeMatchYesPreexisting.
func TestClassifyClaudeTranscriptCutoffRouting(t *testing.T) {
	tmp := t.TempDir()
	ws := resolvePath(tmp)
	cutoff := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)

	write := func(name, cwd, ts string) string {
		p := filepath.Join(tmp, name)
		line := fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"%s","message":{"content":"hi"}}`+"\n", cwd, ts)
		if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	outside := resolvePath(t.TempDir())

	tests := []struct {
		name string
		cwd  string
		ts   string
		want claudeMatchResult
	}{
		{
			name: "after cutoff + cwd in workspace -> yes (tail from start)",
			cwd:  ws,
			ts:   "2026-06-10T10:00:00Z",
			want: claudeMatchYes,
		},
		{
			name: "before cutoff + cwd in workspace -> yesPreexisting (NOT dropped)",
			cwd:  ws,
			ts:   "2026-06-10T08:00:00Z",
			want: claudeMatchYesPreexisting,
		},
		{
			name: "cwd outside workspace, after cutoff -> no",
			cwd:  outside,
			ts:   "2026-06-10T10:00:00Z",
			want: claudeMatchNo,
		},
		{
			name: "cwd outside workspace, before cutoff -> no (cwd is authoritative)",
			cwd:  outside,
			ts:   "2026-06-10T08:00:00Z",
			want: claudeMatchNo,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := write(fmt.Sprintf("t%d.jsonl", i), tc.cwd, tc.ts)
			if got := classifyClaudeTranscript(path, []string{ws}, cutoff); got != tc.want {
				t.Errorf("classifyClaudeTranscript = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoadClaudeWatchProgressDropsStaleNo is the load-migration regression: a
// v0 progress file's cached "no" entries were written under the OLD timestamp
// rule, which dropped pre-cutoff sessions. On upgrade to v1 they must be
// deleted (forcing one re-classification), while "yes" entries survive and the
// schema version advances so this only runs once.
func TestLoadClaudeWatchProgressDropsStaleNo(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)

	// v0 file (no "v" field): x cached "no", y cached "yes".
	legacy := map[string]interface{}{
		"offsets": map[string]int64{"x.jsonl": 10, "y.jsonl": 20},
		"match":   map[string]string{"x.jsonl": "no", "y.jsonl": "yes"},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeWatchProgressPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got := loadClaudeWatchProgress()
	if _, ok := got.Match["x.jsonl"]; ok {
		t.Errorf(`stale "no" entry must be dropped on v0->v1 upgrade; still present: %v`, got.Match)
	}
	if got.Match["y.jsonl"] != "yes" {
		t.Errorf(`"yes" entry must survive; got %q`, got.Match["y.jsonl"])
	}
	if got.V != claudeProgressSchemaV {
		t.Errorf("schema version = %d, want %d", got.V, claudeProgressSchemaV)
	}

	// Persist and reload: the drop must not repeat, and a "no" cached AFTER the
	// upgrade (a genuine cwd mismatch) must now stick.
	got.Match["z.jsonl"] = "no"
	saveClaudeWatchProgress(got)
	again := loadClaudeWatchProgress()
	if again.V != claudeProgressSchemaV {
		t.Errorf("version must persist across save/load; got %d", again.V)
	}
	if again.Match["z.jsonl"] != "no" {
		t.Errorf(`a "no" cached at/after v1 must survive reload; got %q`, again.Match["z.jsonl"])
	}
}

// TestPollSeedsPreexistingOffsetToEOF is the poll-level go-forward proof: a
// matched transcript whose first activity predates the cutoff has its offset
// seeded to the current file size, so pre-watcher history is NOT re-uploaded and
// only future content is tailed. Parses nothing this poll (already at EOF).
func TestPollSeedsPreexistingOffsetToEOF(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	const uuid = "preexisting-sess.jsonl"
	// First (and only) line's timestamp is well before the watch start, so the
	// session is pre-existing.
	line := fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"%s","message":{"role":"user","content":"old history"}}`+"\n",
		resolvePath(workspace), time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339))
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, uuid)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// StartedAt now; startCutoff = now-2min. The line's -2h timestamp is
	// pre-cutoff, so classify returns yesPreexisting.
	session := Session{DeviceID: "sess-pre", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	startCutoff := session.StartedAt.Add(-2 * time.Minute)
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}

	parsed, _ := pollClaudeTranscripts(session, resolvePath(workspace), startCutoff, processors, false, false)
	if parsed != 0 {
		t.Errorf("pre-existing history must NOT be parsed (offset seeded to EOF); got %d parsed", parsed)
	}

	key := claudeProgressKey(path)
	saved := loadClaudeWatchProgress()
	if saved.Match[key] != "yes" {
		t.Errorf("pre-existing match must be cached yes; got %q", saved.Match[key])
	}
	if saved.Offsets[key] != info.Size() {
		t.Errorf("offset must be seeded to file size %d (go-forward); got %d", info.Size(), saved.Offsets[key])
	}
}

// TestPollPreservesExistingOffsetForPreexisting proves the go-forward seed is
// guarded by the unseen check: a transcript with a real prior offset (already
// being tailed) must NOT be re-seeded to EOF when re-classified as preexisting,
// or unread content between the offset and EOF would be skipped. The file has
// two pre-cutoff lines and a prior offset parked at the start of line 2; the
// guard must let line 2 be tailed (parsed==1). A broken guard would re-seed the
// offset to file size and read nothing (parsed==0).
func TestPollPreservesExistingOffsetForPreexisting(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	const uuid = "resumed-sess.jsonl"
	ts := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	line1 := fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"%s","message":{"role":"user","content":"first"}}`+"\n",
		resolvePath(workspace), ts)
	line2 := fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"%s","message":{"role":"user","content":"second"}}`+"\n",
		resolvePath(workspace), ts)
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, uuid)
	if err := os.WriteFile(path, []byte(line1+line2), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed a prior offset at the START of line 2 WITHOUT a cached match, so the
	// classify branch runs (returns yesPreexisting) but the offset seed must be
	// skipped by the `!ok` guard — leaving line 2 to be tailed.
	key := claudeProgressKey(path)
	saveClaudeWatchProgress(claudeWatchProgress{
		Offsets: map[string]int64{key: int64(len(line1))},
		Match:   map[string]string{},
		V:       claudeProgressSchemaV,
	})

	session := Session{DeviceID: "sess-res", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	startCutoff := session.StartedAt.Add(-2 * time.Minute)
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}

	// dryRun: count parses without queueing.
	parsed, _ := pollClaudeTranscripts(session, resolvePath(workspace), startCutoff, processors, true, false)
	if parsed != 1 {
		t.Errorf("prior offset must be preserved so line 2 is tailed; got %d parsed (0 = offset wrongly re-seeded to EOF)", parsed)
	}
}
