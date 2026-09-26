package capture

import (
	"fmt"
	"os"
	"path/filepath"
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

func TestKeepaliveTickPingsOnlyDueMainSessionsOnce(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	proj := filepath.Join(cfg, "projects", "-work")
	if err := os.MkdirAll(filepath.Join(proj, "s1", "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	line := assistantLine(now.Add(-50*time.Minute), "claude-opus-5-5", false, 170_000)
	for _, p := range []string{filepath.Join(proj, "s1.jsonl"), filepath.Join(proj, "s1", "subagents", "agent-a.jsonl")} {
		if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var pings []string
	fake := func(t claudeLastTurn) (keepalivePing, error) {
		pings = append(pings, t.SessionID)
		return keepalivePing{Read: t.Context}, nil
	}
	pinged := map[string]time.Time{}
	keepaliveTick(now, pinged, fake)
	keepaliveTick(now.Add(10*time.Minute), pinged, fake) // the ping just refreshed it
	if len(pings) != 1 || pings[0] != "s1" {
		t.Fatalf("want exactly one ping of s1, got %v", pings)
	}
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
