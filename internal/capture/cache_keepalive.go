package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Cache keep-alive keeps an idle Claude Code session's prompt cache warm so that
// coming back to it doesn't pay for a full context rebuild.
//
// Claude Code writes Anthropic's 1-hour cache. Sixty minutes after its last use
// it expires, and the next turn re-writes the WHOLE context at the cache-write
// rate (2× input): ~$1.40 for a 170k-token Opus session, ~$3.30 at 420k. A ping
// 45–60 minutes after the last use costs one cache READ of that context instead
// (~0.1× input, a few cents). Measured on our own sessions 2026-09-26: 0 of
// 1,191 turns within 60 minutes of the previous one rebuilt, 37 of 37 after 75
// minutes did; a ping at 108 minutes idle read 420k tokens from cache and wrote 0.
//
// The ping is a forked, unsaved, hook-free headless turn (--fork-session
// --no-session-persistence, hooks disabled): the engineer's session is not
// touched and this daemon doesn't capture the ping. The anthropic cache is keyed
// by the prompt prefix, not the session id, so the fork refreshes the entry the
// real session will read. The tool list must NOT be narrowed (--tools and
// friends change the prefix and every ping would miss).
//
// Off by default, and Claude Code only: a Codex ping (`codex exec resume
// --ephemeral`) still wrote into the live rollout, is refused while the session
// is open, and missed the cache even back-to-back; Cursor caches server-side.
//
// ponytail: fixed 4h cap after the last real turn; a smarter keep/let-expire
// decision (e.g. how likely the engineer is to come back) when the cost of
// pinging abandoned sessions shows up in practice.
const (
	keepaliveInterval    = 10 * time.Minute
	keepalivePingAfter   = 45 * time.Minute // ping once the cache is this old...
	keepaliveTTL         = 60 * time.Minute // ...and before it expires
	keepaliveCap         = 4 * time.Hour    // stop keeping a session this long after its last real turn
	keepaliveMinContext  = 30_000           // a small context is cheap to rebuild
	keepalivePingTimeout = 3 * time.Minute
	keepaliveTailBytes   = 1 << 20 // enough to hold the last assistant record
	keepalivePrompt      = `Cache keep-alive ping. Reply with exactly "ok" and nothing else. Do not use tools.`
)

// keepalivePref is the engineer's opt-in, stored like the auto-update consent:
// a small JSON file that outlives every cache. Missing or corrupt reads as off.
type keepalivePref struct {
	Enabled   bool      `json:"enabled"`
	DecidedAt time.Time `json:"decidedAt"`
}

func keepalivePrefPath() string {
	return filepath.Join(state.GlobalPromptsterDir(), "cache-keepalive.json")
}

// CacheKeepaliveEnabled reports the engineer's opt-in. The daemon re-reads it
// every tick, so `keepalive enable|disable` takes effect without a restart.
func CacheKeepaliveEnabled() bool {
	b, err := os.ReadFile(keepalivePrefPath())
	if err != nil {
		return false
	}
	var p keepalivePref
	return json.Unmarshal(b, &p) == nil && p.Enabled
}

// SetCacheKeepalive records the opt-in.
func SetCacheKeepalive(on bool) error {
	b, _ := json.Marshal(keepalivePref{Enabled: on, DecidedAt: time.Now().UTC()})
	if err := os.MkdirAll(state.GlobalPromptsterDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(keepalivePrefPath(), b, 0o600)
}

// claudeLastTurn is the newest main-thread assistant turn of a transcript.
type claudeLastTurn struct {
	At        time.Time
	Model     string
	Cwd       string
	SessionID string
	Context   int // input + cache read + cache write of that turn = the cached prefix size
}

// readClaudeLastTurn scans the transcript's tail backwards for the newest
// main-thread assistant record that carries usage from a claude-* model
// (skipping "<synthetic>" rows, sidechains and a torn first line).
func readClaudeLastTurn(path string) (claudeLastTurn, bool) {
	f, err := os.Open(path) // #nosec G304 -- a transcript under the Claude projects dir we already watch
	if err != nil {
		return claudeLastTurn{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return claudeLastTurn{}, false
	}
	off := info.Size() - keepaliveTailBytes
	if off < 0 {
		off = 0
	}
	buf, err := io.ReadAll(io.NewSectionReader(f, off, info.Size()-off))
	if err != nil {
		return claudeLastTurn{}, false
	}
	lines := bytes.Split(buf, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line may start mid-record
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Type        string    `json:"type"`
			Timestamp   time.Time `json:"timestamp"`
			Cwd         string    `json:"cwd"`
			SessionID   string    `json:"sessionId"`
			IsSidechain bool      `json:"isSidechain"`
			Message     struct {
				Model string `json:"model"`
				Usage *struct {
					Input         int `json:"input_tokens"`
					CacheRead     int `json:"cache_read_input_tokens"`
					CacheCreation int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(lines[i], &rec) != nil || rec.Type != "assistant" || rec.IsSidechain ||
			rec.Message.Usage == nil || !strings.HasPrefix(rec.Message.Model, "claude-") {
			continue
		}
		u := rec.Message.Usage
		return claudeLastTurn{At: rec.Timestamp, Model: rec.Message.Model, Cwd: rec.Cwd,
			SessionID: rec.SessionID, Context: u.Input + u.CacheRead + u.CacheCreation}, true
	}
	return claudeLastTurn{}, false
}

// keepaliveDue decides whether to ping now. lastPing is zero if never pinged.
// The cache's age runs from its last use, real turn or ping; the cap runs from
// the last REAL turn so a session can't keep itself alive forever.
func keepaliveDue(t claudeLastTurn, lastPing, now time.Time) bool {
	if t.SessionID == "" || t.Cwd == "" || t.Context < keepaliveMinContext {
		return false
	}
	touched := t.At
	if lastPing.After(touched) {
		touched = lastPing
	}
	age := now.Sub(touched)
	return age >= keepalivePingAfter && age < keepaliveTTL && now.Sub(t.At) < keepaliveCap
}

type keepalivePing struct{ Read, Write, Output int }

// pingClaudeSession sends one forked, unsaved, hook-free turn on the session's
// model from its cwd and returns what the ping read from and wrote to the cache.
func pingClaudeSession(bin string, t claudeLastTurn) (keepalivePing, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keepalivePingTimeout)
	defer cancel()
	// #nosec G204 -- fixed argv to the engineer's own claude binary; session id and model come from their transcript
	cmd := exec.CommandContext(ctx, bin, "--resume", t.SessionID, "--fork-session", "--no-session-persistence",
		"-p", keepalivePrompt, "--model", t.Model, "--settings", `{"disableAllHooks":true}`, "--output-format", "json")
	cmd.Dir = t.Cwd
	cmd.Stdin = nil // /dev/null: with an open stdin pipe `claude -p` waits on it until the timeout
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return keepalivePing{}, err
	}
	var res struct {
		Usage struct {
			CacheRead     int `json:"cache_read_input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
			Output        int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return keepalivePing{}, fmt.Errorf("unreadable claude output: %w", err)
	}
	return keepalivePing{res.Usage.CacheRead, res.Usage.CacheCreation, res.Usage.Output}, nil
}

// findClaudeBinary resolves `claude` even under launchd/systemd, whose minimal
// PATH usually misses the per-user install locations.
func findClaudeBinary() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		filepath.Join(home, ".npm-global", "bin", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// keepaliveTick pings every due session once. pinged is the daemon's memory of
// its own pings (a restart forgets it: worst case one early extra ping).
func keepaliveTick(now time.Time, pinged map[string]time.Time, ping func(claudeLastTurn) (keepalivePing, error)) {
	for _, path := range candidateClaudeTranscripts(now.Add(-(keepaliveCap + keepaliveTTL))) {
		if isClaudeSidechainFile(path) {
			continue
		}
		t, ok := readClaudeLastTurn(path)
		if !ok || !keepaliveDue(t, pinged[t.SessionID], now) {
			continue
		}
		pinged[t.SessionID] = now
		res, err := ping(t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cache-keepalive: ping %s failed: %v\n", t.SessionID, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "cache-keepalive: pinged %s (%s, idle %s): cache read %d, write %d\n",
			t.SessionID, t.Model, now.Sub(t.At).Round(time.Minute), res.Read, res.Write)
	}
	for id, at := range pinged {
		if now.Sub(at) > keepaliveCap+keepaliveTTL {
			delete(pinged, id)
		}
	}
}

func runCacheKeepalive(stop <-chan struct{}) {
	pinged := map[string]time.Time{}
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if !CacheKeepaliveEnabled() {
				continue
			}
			bin := findClaudeBinary()
			if bin == "" {
				fmt.Fprintln(os.Stderr, "cache-keepalive: enabled but no claude binary found; skipping")
				continue
			}
			keepaliveTick(time.Now(), pinged, func(t claudeLastTurn) (keepalivePing, error) {
				return pingClaudeSession(bin, t)
			})
		}
	}
}

// StartCacheKeepalive launches the keep-alive loop and returns a stop func the
// caller defers. It always runs; each tick checks the opt-in.
func StartCacheKeepalive() (stop func()) {
	done := make(chan struct{})
	go runCacheKeepalive(done)
	return func() { close(done) }
}
