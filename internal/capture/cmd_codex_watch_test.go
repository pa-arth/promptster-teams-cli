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

// codexSessionsRoot points CODEX_HOME at a temp dir and returns the sessions
// root (CODEX_HOME/sessions), so tests can build realistic rollout paths.
func codexSessionsRoot(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	root := filepath.Join(home, "sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// codexSessionMetaLine builds the first line of a rollout — the session_meta
// header, the only line carrying cwd and the session start timestamp.
func codexSessionMetaLine(cwd, ts string) string {
	return fmt.Sprintf(`{"type":"session_meta","timestamp":"%s","payload":{"cwd":"%s","id":"019eb780-3081-7ce0-9ba0-8a0bad13b532"}}`+"\n", ts, cwd)
}

// TestClassifyCodexRolloutCutoffRouting pins the go-forward capture-time gate:
// cwd is authoritative, and the session_meta timestamp only routes a matched
// rollout to new-vs-preexisting. A cwd match started BEFORE the cutoff must NOT
// be dropped (the bug that silently lost every restart-spanning session, same
// class as PR #68 for Claude) — it is captured go-forward from EOF as
// codexMatchYesPreexisting.
func TestClassifyCodexRolloutCutoffRouting(t *testing.T) {
	tmp := t.TempDir()
	ws := resolvePath(tmp)
	cutoff := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)

	write := func(name, cwd, ts string) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(codexSessionMetaLine(cwd, ts)), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	outside := resolvePath(t.TempDir())

	tests := []struct {
		name string
		cwd  string
		ts   string
		want codexMatchResult
	}{
		{
			name: "after cutoff + cwd in workspace -> yes (tail from start)",
			cwd:  ws,
			ts:   "2026-06-10T10:00:00Z",
			want: codexMatchYes,
		},
		{
			name: "before cutoff + cwd in workspace -> yesPreexisting (NOT dropped)",
			cwd:  ws,
			ts:   "2026-06-10T08:00:00Z",
			want: codexMatchYesPreexisting,
		},
		{
			name: "cwd outside workspace, after cutoff -> no",
			cwd:  outside,
			ts:   "2026-06-10T10:00:00Z",
			want: codexMatchNo,
		},
		{
			name: "cwd outside workspace, before cutoff -> no (cwd is authoritative)",
			cwd:  outside,
			ts:   "2026-06-10T08:00:00Z",
			want: codexMatchNo,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := write(fmt.Sprintf("rollout-%d.jsonl", i), tc.cwd, tc.ts)
			if got := classifyCodexRollout(path, []string{ws}, cutoff); got != tc.want {
				t.Errorf("classifyCodexRollout = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClassifyCodexRolloutUndecidedWhenNotSessionMeta proves a file caught
// mid-creation (line 1 not yet a readable session_meta) stays undecided — it
// must NOT be cached "no", or a rollout whose header is still being written
// would be dropped forever. cwd + timestamp live only on line 1, so retry is
// the only safe answer.
func TestClassifyCodexRolloutUndecidedWhenNotSessionMeta(t *testing.T) {
	tmp := t.TempDir()
	ws := resolvePath(tmp)
	cutoff := time.Now()

	// Empty file (no lines yet).
	empty := filepath.Join(tmp, "rollout-empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := classifyCodexRollout(empty, []string{ws}, cutoff); got != codexMatchUndecided {
		t.Errorf("empty file = %v, want undecided", got)
	}

	// First line present but not a session_meta yet.
	notMeta := filepath.Join(tmp, "rollout-notmeta.jsonl")
	if err := os.WriteFile(notMeta, []byte(`{"type":"response_item","payload":{}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := classifyCodexRollout(notMeta, []string{ws}, cutoff); got != codexMatchUndecided {
		t.Errorf("non-session_meta first line = %v, want undecided", got)
	}
}

// TestPollCodexSeedsPreexistingOffsetToEOF proves the history bound: a rollout
// whose session_meta predates the 28-day cutoff stays GO-FORWARD — its
// offset seeded to current file size so pre-watcher history is NOT re-uploaded.
// A newly appended line is then the only content tailed.
func TestPollCodexSeedsPreexistingOffsetToEOF(t *testing.T) {
	root := codexSessionsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	dir := filepath.Join(root, "2026", "06", "11")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-06-11T11-24-52-019eb780-3081-7ce0-9ba0-8a0bad13b532.jsonl")

	// session_meta is older than the product window. A historical prompt follows.
	preTS := time.Now().Add(-29 * 24 * time.Hour).UTC().Format(time.RFC3339)
	history := codexSessionMetaLine(resolvePath(workspace), preTS) +
		`{"timestamp":"` + preTS + `","type":"event_msg","payload":{"type":"user_message","message":"old history","images":[]}}` + "\n"
	if err := os.WriteFile(path, []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// The file's mod-time is now, so it reaches the authoritative timestamp gate.
	now := time.Now().UTC()
	session := Session{DeviceID: "sess-codex-pre", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: now}
	historyCutoff := transcriptHistoryCutoff(now)
	processors := map[string]*normalize.CodexRolloutProcessor{}

	sent := pollCodexRollouts(session, resolvePath(workspace), historyCutoff, processors, false)
	if sent != 0 {
		t.Errorf("pre-existing history must NOT be queued (offset seeded to EOF); got %d queued", sent)
	}

	saved := loadCodexWatchProgress()
	if saved.Match[path] != "yes" {
		t.Errorf("pre-existing match must be cached yes; got %q", saved.Match[path])
	}
	if saved.Offsets[path] != info.Size() {
		t.Errorf("offset must be seeded to file size %d (go-forward); got %d", info.Size(), saved.Offsets[path])
	}

	// Append a NEW line written after the watch start — only this line must be
	// tailed on the next poll.
	newTS := time.Now().UTC().Format(time.RFC3339)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	newLine := `{"timestamp":"` + newTS + `","type":"event_msg","payload":{"type":"user_message","message":"new prompt after watch start","images":[]}}` + "\n"
	if _, err := f.WriteString(newLine); err != nil {
		t.Fatal(err)
	}
	f.Close()

	sent = pollCodexRollouts(session, resolvePath(workspace), historyCutoff, processors, false)
	if sent != 1 {
		t.Errorf("only the newly appended line must be tailed; got %d queued", sent)
	}

	saved = loadCodexWatchProgress()
	newInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Offsets[path] != newInfo.Size() {
		t.Errorf("offset must advance to new EOF %d; got %d", newInfo.Size(), saved.Offsets[path])
	}
}

func TestPollCodexBackfillsInWindowHistory(t *testing.T) {
	root := codexSessionsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	dir := filepath.Join(root, "2026", "07", "20")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ts := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	history := codexSessionMetaLine(resolvePath(workspace), ts) +
		`{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"user_message","message":"historical prompt","images":[]}}` + "\n"
	path := filepath.Join(dir, "rollout-history.jsonl")
	if err := os.WriteFile(path, []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}

	session := Session{DeviceID: "sess-codex-history", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: now}
	sent := pollCodexRollouts(session, resolvePath(workspace), transcriptHistoryCutoff(now), map[string]*normalize.CodexRolloutProcessor{}, false)
	if sent != 2 {
		t.Fatalf("in-window history queued %d events, want session_start + prompt", sent)
	}
	saved := loadCodexWatchProgress()
	if saved.Offsets[path] != int64(len(history)) {
		t.Fatalf("in-window history offset = %d, want %d", saved.Offsets[path], len(history))
	}
}

// TestLoadCodexWatchProgressDropsStaleNo covers v0 -> v2: stale timestamp
// decisions and accepted offsets are both reopened exactly once.
func TestLoadCodexWatchProgressDropsStaleNo(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)

	// v0 file (no "v" field): x cached "no", y cached "yes".
	legacy := map[string]interface{}{
		"offsets": map[string]int64{"/x.jsonl": 10, "/y.jsonl": 20},
		"match":   map[string]string{"/x.jsonl": "no", "/y.jsonl": "yes"},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexWatchProgressPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got := loadCodexWatchProgress()
	if _, ok := got.Match["/x.jsonl"]; ok {
		t.Errorf(`stale "no" entry must be dropped on v0->v1 upgrade; still present: %v`, got.Match)
	}
	if _, ok := got.Match["/y.jsonl"]; ok {
		t.Errorf(`old "yes" entry must be reopened for bounded backfill; got %v`, got.Match)
	}
	if len(got.Offsets) != 0 {
		t.Errorf("old offsets must be cleared for bounded backfill; got %v", got.Offsets)
	}
	if got.V != codexProgressSchemaV {
		t.Errorf("schema version = %d, want %d", got.V, codexProgressSchemaV)
	}

	// Persist and reload: the drop must not repeat, and a "no" cached AFTER the
	// upgrade (a genuine cwd mismatch) must now stick.
	got.Match["/z.jsonl"] = "no"
	saveCodexWatchProgress(got)
	again := loadCodexWatchProgress()
	if again.V != codexProgressSchemaV {
		t.Errorf("version must persist across save/load; got %d", again.V)
	}
	if again.Match["/z.jsonl"] != "no" {
		t.Errorf(`a "no" cached at/after v1 must survive reload; got %q`, again.Match["/z.jsonl"])
	}
}

func TestLoadCodexWatchProgressV2PreservesCwdMismatch(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	saveCodexWatchProgress(codexWatchProgress{
		Offsets: map[string]int64{"accepted.jsonl": 20},
		Match:   map[string]string{"accepted.jsonl": "yes", "outside.jsonl": "no"},
		V:       1,
	})

	got := loadCodexWatchProgress()
	if len(got.Offsets) != 0 || got.Match["accepted.jsonl"] != "" {
		t.Fatalf("accepted rollout was not reopened: offsets=%v match=%v", got.Offsets, got.Match)
	}
	if got.Match["outside.jsonl"] != "no" {
		t.Fatalf("genuine cwd mismatch must survive v2 migration; got %v", got.Match)
	}
}
