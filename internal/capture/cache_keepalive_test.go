package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func assistantLine(ts time.Time, model string, sidechain bool, read int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/work","sessionId":"s1","isSidechain":%t,"message":{"model":%q,"usage":{"input_tokens":2,"cache_read_input_tokens":%d,"cache_creation_input_tokens":100}}}`,
		ts.Format(time.RFC3339Nano), sidechain, model, read)
}

func TestReadClaudeLastTurnSkipsNonMainThreadAndSynthetic(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	lines := []string{
		assistantLine(t0, "claude-opus-5-5", false, 170_000),
		assistantLine(t0.Add(time.Minute), "claude-haiku-4-5", true, 5), // sidechain
		assistantLine(t0.Add(2*time.Minute), "<synthetic>", false, 0),   // not a real turn
		`{"type":"user","message":{"content":"thanks"}}`,
		`{"type":"assistant", torn`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := readClaudeLastTurn(path)
	if !ok {
		t.Fatal("no turn found")
	}
	if !got.At.Equal(t0) || got.Model != "claude-opus-5-5" || got.Context != 170_102 || got.Cwd != "/work" || got.SessionID != "s1" {
		t.Fatalf("wrong turn: %+v", got)
	}
}

func TestKeepaliveDue(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	turn := func(idle time.Duration, ctx int) claudeLastTurn {
		return claudeLastTurn{At: now.Add(-idle), Model: "claude-opus-5-5", Cwd: "/w", SessionID: "s", Context: ctx}
	}
	cases := []struct {
		name     string
		t        claudeLastTurn
		lastPing time.Time
		want     bool
	}{
		{"still warm", turn(30*time.Minute, 170_000), time.Time{}, false},
		{"due", turn(50*time.Minute, 170_000), time.Time{}, true},
		{"already expired, a ping would pay the rebuild", turn(70*time.Minute, 170_000), time.Time{}, false},
		{"kept alive by a recent ping", turn(2*time.Hour, 170_000), now.Add(-20 * time.Minute), false},
		{"previous ping ageing out", turn(2*time.Hour, 170_000), now.Add(-50 * time.Minute), true},
		{"past the cap", turn(4*time.Hour+10*time.Minute, 170_000), now.Add(-50 * time.Minute), false},
		{"too small to be worth it", turn(50*time.Minute, 10_000), time.Time{}, false},
	}
	for _, c := range cases {
		if got := keepaliveDue(c.t, c.lastPing, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// writeSession puts one main-thread transcript whose last turn was idle ago
// under a fresh CLAUDE_CONFIG_DIR project.
func writeSession(t *testing.T, proj, id string, lastTurn time.Time, trailer string) string {
	t.Helper()
	line := strings.Replace(assistantLine(lastTurn, "claude-opus-5-5", false, 170_000), `"sessionId":"s1"`, fmt.Sprintf(`"sessionId":%q`, id), 1)
	p := filepath.Join(proj, id+".jsonl")
	if err := os.WriteFile(p, []byte(line+"\n"+trailer), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func claudeProject(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	proj := filepath.Join(cfg, "projects", "-work")
	if err := os.MkdirAll(filepath.Join(proj, "s1", "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	return proj
}

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestKeepaliveTickPingsOnlyDueMainSessionsOnce(t *testing.T) {
	proj := claudeProject(t)
	now := time.Now()
	writeSession(t, proj, "s1", now.Add(-50*time.Minute), "")
	line := assistantLine(now.Add(-50*time.Minute), "claude-opus-5-5", false, 170_000)
	if err := os.WriteFile(filepath.Join(proj, "s1", "subagents", "agent-a.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var pings []string
	fake := func(t claudeLastTurn) (keepalivePing, error) {
		pings = append(pings, t.SessionID)
		return keepalivePing{Read: t.Context}, nil
	}
	pinged := map[string]time.Time{}
	keepaliveTick(fixed(now), pinged, fake)
	keepaliveTick(fixed(now.Add(10*time.Minute)), pinged, fake) // the ping just refreshed it
	if len(pings) != 1 || pings[0] != "s1" {
		t.Fatalf("want exactly one ping of s1, got %v", pings)
	}
}

func TestKeepaliveTickRetriesAFailedPing(t *testing.T) {
	proj := claudeProject(t)
	now := time.Now()
	writeSession(t, proj, "s1", now.Add(-46*time.Minute), "")
	calls := 0
	failOnce := func(claudeLastTurn) (keepalivePing, error) {
		calls++
		if calls == 1 {
			return keepalivePing{}, fmt.Errorf("timed out")
		}
		return keepalivePing{}, nil
	}
	pinged := map[string]time.Time{}
	keepaliveTick(fixed(now), pinged, failOnce)
	keepaliveTick(fixed(now.Add(10*time.Minute)), pinged, failOnce)
	if calls != 2 {
		t.Fatalf("a failed ping must not count as a refresh: want a retry, got %d calls", calls)
	}
}

func TestKeepaliveTickJudgesEachSessionAtItsOwnTime(t *testing.T) {
	proj := claudeProject(t)
	start := time.Now()
	// Both due at the tick's start; b's cache expires 8 min in.
	writeSession(t, proj, "a", start.Add(-50*time.Minute), "")
	writeSession(t, proj, "b", start.Add(-52*time.Minute), "")
	clock := start
	var pinged []string
	slow := func(t claudeLastTurn) (keepalivePing, error) {
		pinged = append(pinged, t.SessionID)
		clock = clock.Add(10 * time.Minute) // each ping takes long enough for the next cache to die
		return keepalivePing{}, nil
	}
	keepaliveTick(func() time.Time { return clock }, map[string]time.Time{}, slow)
	if len(pinged) != 1 {
		t.Fatalf("the second session expired while the first was pinged; want 1 ping, got %v", pinged)
	}
}

func TestReadClaudeLastTurnFindsATurnFarFromTheEnd(t *testing.T) {
	proj := claudeProject(t)
	t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	big := fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","content":%q}]}}`, strings.Repeat("x", 3*keepaliveTailBytes))
	p := writeSession(t, proj, "s1", t0, big+"\n")
	got, ok := readClaudeLastTurn(p)
	if !ok || !got.At.Equal(t0) {
		t.Fatalf("turn behind a %d-byte tool result not found: %+v %v", 3*keepaliveTailBytes, got, ok)
	}
}

// A stand-in `claude` records its argv, cwd and stdin, and answers like
// `claude -p --output-format json`.
func TestReadClaudeLastTurnStopsAtTheScanLimit(t *testing.T) {
	proj := claudeProject(t)
	t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	old := keepaliveMaxScanBytes
	keepaliveMaxScanBytes = 2 * keepaliveTailBytes
	t.Cleanup(func() { keepaliveMaxScanBytes = old })
	big := fmt.Sprintf(`{"type":"user","message":{"content":%q}}`, strings.Repeat("x", 3*keepaliveTailBytes))
	p := writeSession(t, proj, "s1", t0, big+"\n")
	if got, ok := readClaudeLastTurn(p); ok {
		t.Fatalf("a turn beyond the scan limit must not be found (bounded work), got %+v", got)
	}
}

func TestReadClaudeLastTurnAcrossChunkBoundaries(t *testing.T) {
	// The tail after the assistant record is 50 bytes short of one chunk, so the
	// first chunk boundary falls INSIDE that record: it only parses if the
	// partial line is carried into the next (earlier) chunk.
	proj := claudeProject(t)
	t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	const shape = `{"type":"user","message":{"content":"%s"}}` + "\n"
	pad := keepaliveTailBytes - 50 - (len(shape) - 2)
	tail := fmt.Sprintf(shape, strings.Repeat("y", pad))
	if len(tail) != keepaliveTailBytes-50 {
		t.Fatalf("tail is %d bytes, want %d", len(tail), keepaliveTailBytes-50)
	}
	p := writeSession(t, proj, "s1", t0, tail)
	got, ok := readClaudeLastTurn(p)
	if !ok || !got.At.Equal(t0) || got.Context != 170_102 {
		t.Fatalf("record split across a chunk boundary not recovered: %+v %v", got, ok)
	}
}

func TestPingClaudeSessionArgsCwdAndClosedStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + dir + `/args"
pwd > "` + dir + `/cwd"
cat > "` + dir + `/stdin"
echo '{"result":"ok","usage":{"cache_read_input_tokens":170000,"cache_creation_input_tokens":120,"output_tokens":1}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	res, err := pingClaudeSession(bin, claudeLastTurn{SessionID: "s1", Model: "claude-opus-5-5", Cwd: work})
	if err != nil {
		t.Fatal(err) // an open stdin would hang `cat` until the timeout
	}
	if res != (keepalivePing{Read: 170000, Write: 120, Output: 1}) {
		t.Fatalf("usage not parsed: %+v", res)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"--resume\ns1\n", "--fork-session", "--no-session-persistence", "--model\nclaude-opus-5-5\n", `{"disableAllHooks":true}`} {
		if !strings.Contains(string(args), want) {
			t.Errorf("argv missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "--tools") {
		t.Error("narrowing tools changes the prompt prefix; every ping would miss")
	}
	cwd, _ := os.ReadFile(filepath.Join(dir, "cwd"))
	if got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(cwd))); got != mustEval(t, work) {
		t.Errorf("ran in %q, want the session's cwd %q", got, work)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCacheKeepalivePrefRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if CacheKeepaliveEnabled() {
		t.Fatal("must default to off")
	}
	if err := SetCacheKeepalive(true); err != nil {
		t.Fatal(err)
	}
	if !CacheKeepaliveEnabled() {
		t.Fatal("enable didn't stick")
	}
	if err := SetCacheKeepalive(false); err != nil {
		t.Fatal(err)
	}
	if CacheKeepaliveEnabled() {
		t.Fatal("disable didn't stick")
	}
}
