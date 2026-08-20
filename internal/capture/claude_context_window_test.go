package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A real Claude Code 2.1.237 status-line blob, trimmed to the keys the shim
// names. The shape is the one Claude Code documents for itself, and the
// 1000000 is what this machine actually reports for model id `claude-opus-5` —
// the same id that reports 200000 on a non-1M session, which is the reason the
// window is read from the vendor rather than looked up from the id.
const cwBlob = `{
  "session_id": "60d0d35e-ca8a-490b-a816-e0b05b29b202",
  "transcript_path": "/Users/dev/.claude/projects/-Users-dev-repo/60d0d35e-ca8a-490b-a816-e0b05b29b202.jsonl",
  "cwd": "/Users/dev/repo",
  "model": {"id": "claude-opus-5", "display_name": "Opus 5"},
  "context_window": {
    "total_input_tokens": 184577,
    "total_output_tokens": 198,
    "context_window_size": 1000000,
    "current_usage": {"input_tokens": 2, "output_tokens": 198, "cache_creation_input_tokens": 3558, "cache_read_input_tokens": 181017},
    "used_percentage": 18,
    "remaining_percentage": 82
  },
  "rate_limits": {"five_hour": {"used_percentage": 31.5, "resets_at": 1787260000}}
}`

func TestParseClaudeContextWindow(t *testing.T) {
	id, s, ok := parseClaudeContextWindow([]byte(cwBlob), 1787252149)
	if !ok {
		t.Fatal("a blob carrying context_window_size must parse")
	}
	if id != "60d0d35e-ca8a-490b-a816-e0b05b29b202" {
		t.Errorf("session id = %q", id)
	}
	if s.ContextWindowTokens != 1_000_000 {
		t.Errorf("window = %d, want 1000000", s.ContextWindowTokens)
	}
	if s.ObservedAt != 1787252149 {
		t.Errorf("observedAt = %d", s.ObservedAt)
	}
}

// The rate-limit reading and the context window are lifted from ONE blob by two
// independent parsers, and each must tolerate the other's absence: rate_limits
// is documented as present only "for subscribers after first API response",
// while context_window_size is there from the first tick. A blob with one and
// not the other is the normal case at both ends of a session.
func TestParseClaudeContextWindowIndependentOfRateLimits(t *testing.T) {
	blob := []byte(`{"session_id":"abc-123","context_window":{"context_window_size":200000}}`)
	if _, s, ok := parseClaudeContextWindow(blob, 100); !ok || s.ContextWindowTokens != 200_000 {
		t.Fatalf("context window must parse with no rate_limits: ok=%v tokens=%d", ok, s.ContextWindowTokens)
	}
	if _, ok := parseClaudeStatuslineBlob(blob, 100); ok {
		t.Fatal("no rate_limits must not yield a window reading")
	}
	rlOnly := []byte(`{"session_id":"abc-123","rate_limits":{"five_hour":{"used_percentage":10,"resets_at":5}}}`)
	if _, _, ok := parseClaudeContextWindow(rlOnly, 100); ok {
		t.Fatal("no context_window must not yield a context reading")
	}
	if _, ok := parseClaudeStatuslineBlob(rlOnly, 100); !ok {
		t.Fatal("rate_limits alone must still yield a window reading")
	}
}

// Absent, never 0 — enforced at the boundary the value enters through, so a
// malformed vendor field cannot become a division by zero or a session drawn as
// completely full four systems downstream.
func TestParseClaudeContextWindowRejectsUnusable(t *testing.T) {
	for name, blob := range map[string]string{
		"no session id": `{"context_window":{"context_window_size":200000}}`,
		"no window":     `{"session_id":"abc-123","context_window":{}}`,
		"null window":   `{"session_id":"abc-123","context_window":{"context_window_size":null}}`,
		"zero window":   `{"session_id":"abc-123","context_window":{"context_window_size":0}}`,
		"negative":      `{"session_id":"abc-123","context_window":{"context_window_size":-200000}}`,
		"not json":      `nonsense`,
		"empty":         ``,
		"no ctx at all": `{"session_id":"abc-123"}`,
	} {
		if _, _, ok := parseClaudeContextWindow([]byte(blob), 100); ok {
			t.Errorf("%s: must not yield a reading", name)
		}
	}
}

// The session id becomes a FILENAME and arrives on stdin from another process,
// so it is refused rather than sanitised: a sanitiser that turns a traversal
// into a plausible name is worse than a refusal, because it writes somewhere
// nobody expected under a name that looks fine.
func TestClaudeContextSessionIDRejectsUnsafe(t *testing.T) {
	for _, id := range []string{
		"", "../../etc/passwd", "a/b", `a\b`, "..", "sess id",
		"session.json", "sess_1", "zzz",
	} {
		if claudeContextSessionIDOK(id) {
			t.Errorf("session id %q must be refused as a filename", id)
		}
	}
	if !claudeContextSessionIDOK("60d0d35e-ca8a-490b-a816-e0b05b29b202") {
		t.Error("a real Claude session uuid must be accepted")
	}
}

func TestWriteClaudeContextSpoolRefusesUnsafeID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	if err := writeClaudeContextSpool("../escape", claudeContextSpool{ContextWindowTokens: 1, ObservedAt: 1}); err != nil {
		t.Fatalf("a refused id must be a no-op, not an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.json")); err == nil {
		t.Fatal("a traversal id wrote a file")
	}
}

// Two Claude Code windows open side by side is the normal case, not an edge one.
// They run different models against different ceilings, and each tick rewrites
// its own file by atomic rename — which replaces the WHOLE file. One shared map
// would have each session restore its own snapshot and silently drop the
// other's; this asserts the per-session split that prevents it.
func TestClaudeContextSpoolIsPerSession(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	a := "aaaaaaaa-0000-0000-0000-000000000001"
	b := "bbbbbbbb-0000-0000-0000-000000000002"
	if err := writeClaudeContextSpool(a, claudeContextSpool{ContextWindowTokens: 1_000_000, ObservedAt: 10}); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeContextSpool(b, claudeContextSpool{ContextWindowTokens: 200_000, ObservedAt: 11}); err != nil {
		t.Fatal(err)
	}
	got, ok := readClaudeContextSpool(a)
	if !ok || got.ContextWindowTokens != 1_000_000 {
		t.Fatalf("session a = %d (ok=%v); a second session's tick overwrote it", got.ContextWindowTokens, ok)
	}
	got, ok = readClaudeContextSpool(b)
	if !ok || got.ContextWindowTokens != 200_000 {
		t.Fatalf("session b = %d (ok=%v)", got.ContextWindowTokens, ok)
	}
}

// The OPPOSITE of readClaudeWindowSpool's drain semantics, and the difference is
// the whole reason this is a second spool. A rate-limit reading becomes one
// event and is consumed; a context window is a standing property read once per
// ai_response for the life of the session. Draining it would leave every turn
// after the first without a denominator.
func TestReadClaudeContextSpoolDoesNotDrain(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	id := "cccccccc-0000-0000-0000-000000000003"
	if err := writeClaudeContextSpool(id, claudeContextSpool{ContextWindowTokens: 200_000, ObservedAt: 10}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, ok := readClaudeContextSpool(id); !ok {
			t.Fatalf("read %d drained the spool — every later turn loses its denominator", i+1)
		}
	}
}

// Latest-wins: a mid-session model switch moves the window, and the newest tick
// is the one that describes the turn about to run.
func TestWriteClaudeContextSpoolLatestWins(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	id := "dddddddd-0000-0000-0000-000000000004"
	_ = writeClaudeContextSpool(id, claudeContextSpool{ContextWindowTokens: 1_000_000, ObservedAt: 10})
	_ = writeClaudeContextSpool(id, claudeContextSpool{ContextWindowTokens: 200_000, ObservedAt: 20})
	got, ok := readClaudeContextSpool(id)
	if !ok || got.ContextWindowTokens != 200_000 || got.ObservedAt != 20 {
		t.Fatalf("latest-wins failed: %+v (ok=%v)", got, ok)
	}
}

// Nothing tells the shim that a session ENDED — Claude Code simply stops calling
// it — so the files are aged out rather than closed. Without this the directory
// grows one file per session forever.
func TestPruneClaudeContextSpools(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	fresh := "eeeeeeee-0000-0000-0000-000000000005"
	stale := "ffffffff-0000-0000-0000-000000000006"
	now := time.Now()
	for _, id := range []string{fresh, stale} {
		if err := writeClaudeContextSpool(id, claudeContextSpool{ContextWindowTokens: 200_000, ObservedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	stalePath := filepath.Join(claudeContextSpoolDir(), stale+".json")
	old := now.Add(-claudeContextSpoolTTL - time.Hour)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}
	pruneClaudeContextSpools(now)
	if _, ok := readClaudeContextSpool(stale); ok {
		t.Error("a spool untouched past the TTL must be pruned")
	}
	if _, ok := readClaudeContextSpool(fresh); !ok {
		t.Error("prune removed a live session's spool")
	}
}

// The shim is the only writer, and it must spool the window on the same tick it
// spools the rate limits — asserted through RunStatuslineShim rather than the
// parser alone, because the parser being right is worth nothing if the shim
// never calls it.
func TestStatuslineShimSpoolsContextWindow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.Write([]byte(cwBlob))
		_ = w.Close()
	}()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = r, devNull
	code := RunStatuslineShim()
	os.Stdin, os.Stdout = oldIn, oldOut
	_ = r.Close()

	if code != 0 {
		t.Fatalf("the shim must always exit 0 (Claude Code renders its stdout); got %d", code)
	}
	got, ok := readClaudeContextSpool("60d0d35e-ca8a-490b-a816-e0b05b29b202")
	if !ok {
		t.Fatal("the shim did not spool the context window")
	}
	if got.ContextWindowTokens != 1_000_000 {
		t.Fatalf("spooled window = %d, want 1000000", got.ContextWindowTokens)
	}
	// Same tick, both spools — the rate-limit handoff must not have regressed.
	if _, ok := readClaudeWindowSpool(); !ok {
		t.Fatal("the shim stopped spooling the rate-limit reading")
	}
}
