package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Claude Code CONTEXT-WINDOW capture — the denominator for the Claude rail.
//
// The Codex rail carries its own window (`token_count.info.model_context_window`,
// lifted in normalize_codex.go). The Claude transcript carries NONE: it records
// per-request token usage and the model id, and nothing anywhere in it says how
// big the window those tokens were spent against is. Verified against a live
// transcript — the only channel that reports it is the status-line stdin blob,
// documented in Claude Code 2.1.237 itself as:
//
//	"context_window": {
//	  "total_input_tokens": number,   // tokens currently in the window
//	  "context_window_size": number,  // window size for the CURRENT model
//	  "used_percentage": number | null
//	}
//
// So the shim that already runs on every status-line tick spools the window per
// SESSION, and the claude watcher stamps it onto that session's ai_response
// events. The value is the VENDOR's and stays that way — never a model-id
// lookup table. That is not a style preference: this very machine reports a
// 1,000,000-token window for model id `claude-opus-5`, the same id that reports
// 200,000 elsewhere, so the id is not even a key that a table could be built on.
//
// WHY A SEPARATE SPOOL from claudeWindowSpoolPath (the 5h/weekly rate-limit
// handoff), when both are written by the same shim on the same tick:
//
//   - Different KEY. Rate limits are an ACCOUNT fact — one latest-wins reading
//     per (engineer, provider). A context window is a SESSION fact; two Claude
//     Code windows open side by side run different models against different
//     ceilings, and one file per engineer would have them overwrite each other.
//   - Different LIFECYCLE. The rate-limit spool is DRAIN-ONCE: the watcher emits
//     the reading and deletes it, so one reading becomes one event. A window is
//     a standing property read many times — once per ai_response for as long as
//     the session lives — so this spool is read WITHOUT removing, and aged out
//     by prune instead.
//
// One file per session id rather than one map file for all of them, because the
// write is an atomic rename and a rename replaces the WHOLE file: with a shared
// map, two concurrently-ticking sessions would each restore their own snapshot
// and silently drop the other's entry. Per-session files cannot collide.

// claudeContextSpoolDir holds one <session-id>.json per active Claude session.
func claudeContextSpoolDir() string {
	return filepath.Join(state.StateDir(), "claude-context")
}

// claudeContextSpool is the on-disk handoff shape.
//
// ContextWindowTokens is > 0 whenever the file exists: a window of 0 is not a
// small window, it is a division by zero or a session drawn as 100% full, so an
// unreported window is never spooled at all rather than spooled as 0.
type claudeContextSpool struct {
	ContextWindowTokens int64 `json:"contextWindowTokens"`
	// ObservedAt is the tick time, epoch seconds — the freshness anchor the
	// watcher pairs against a turn's own timestamp.
	ObservedAt int64 `json:"observedAt"`
}

// Two integers, and deliberately nothing else. The blob this is lifted from also
// carries a transcript path, a cwd, a workspace, a running cost total and the
// model id — none of which is written here, so the spool cannot become a second
// copy of any of them. The model id in particular was considered and dropped: it
// would have been a stored value with no reader, and it is not even usable as a
// window gate, since `claude-opus-5` reports 1,000,000 on a 1M session and
// 200,000 elsewhere. The session id keys the file and is already the id every
// captured event carries, so it discloses nothing new.

// claudeContextSessionIDOK reports whether a session id is safe to use as a
// filename. Claude session ids are UUIDs; anything else is refused outright
// rather than sanitised, because the id arrives on stdin from another process
// and a sanitiser that turns a traversal into a plausible name is worse than a
// refusal. No separators, no dots, bounded length, hex-and-dash only.
func claudeContextSessionIDOK(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

func claudeContextSpoolPath(sessionID string) (string, bool) {
	if !claudeContextSessionIDOK(sessionID) {
		return "", false
	}
	return filepath.Join(claudeContextSpoolDir(), sessionID+".json"), true
}

// writeClaudeContextSpool atomically overwrites one session's spool with the
// latest reading (latest-wins — a mid-session model switch moves the window and
// the newest tick is the one that describes the turn about to run).
//
// Best-effort: the shim must never block or fail a status-line render over it.
func writeClaudeContextSpool(sessionID string, s claudeContextSpool) error {
	path, ok := claudeContextSpoolPath(sessionID)
	if !ok {
		return nil
	}
	if s.ContextWindowTokens <= 0 {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(claudeContextSpoolDir(), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(claudeContextSpoolDir(), path, data)
}

// writeFileAtomic writes data to a UNIQUE temporary file in dir and renames it
// over path.
//
// The uniqueness is the point. A fixed `<path>.tmp` is shared by every process
// that writes it, and the shim is not a singleton: Claude Code invokes it per
// tick, and our own step (3) runs the engineer's prior status-line command under
// a 2.5s timeout, so a slow one leaves tick N still resident when tick N+1
// fires. Two processes writing one tmp file interleave into a file that is
// neither reading — the rename then publishes corrupt JSON, not a stale value.
//
// What remains, and why it is acceptable: two renames microseconds apart may
// land in either order, so an older reading can still win. Both readings are
// then from the same second of the same session and carry the same window, so
// the value is identical and only `observedAt` differs. Closing that would need
// a read-compare-rename, which is a TOCTOU race dressed up as a fix — worse
// than the thing it replaces.
func writeFileAtomic(dir, path string, data []byte) error {
	return writeFileAtomicMode(dir, path, data, 0o600)
}

// writeFileAtomicMode is writeFileAtomic with an explicit final mode, for the
// files that are deliberately not 0600 — ~/.claude/settings.json is user config
// and world-readable by design.
func writeFileAtomicMode(dir, path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// Leaving the temp behind would accumulate one file per failed tick.
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readClaudeContextSpool loads one session's reading WITHOUT removing it — the
// opposite of readClaudeWindowSpool's drain semantics, and deliberately so: a
// window is read once per ai_response for the life of the session, so draining
// it would leave every turn after the first without a denominator.
//
// ok=false when absent, unparseable, or carrying a non-positive window.
func readClaudeContextSpool(sessionID string) (claudeContextSpool, bool) {
	path, ok := claudeContextSpoolPath(sessionID)
	if !ok {
		return claudeContextSpool{}, false
	}
	data, err := os.ReadFile(path) // #nosec G304 -- a validated uuid-shaped basename under the state dir.
	if err != nil {
		return claudeContextSpool{}, false
	}
	var s claudeContextSpool
	if err := json.Unmarshal(data, &s); err != nil {
		_ = os.Remove(path)
		return claudeContextSpool{}, false
	}
	if s.ContextWindowTokens <= 0 {
		return claudeContextSpool{}, false
	}
	return s, true
}

// claudeContextSpoolTTL bounds how long a session's spool survives after its
// last tick. Nothing tells the shim that a session ENDED — Claude Code simply
// stops calling it — so the files are aged out rather than closed. A day is far
// past the point where a reading could still describe a live turn (the watcher's
// own freshness bound is minutes), and keeps the directory to the sessions an
// engineer actually ran recently.
const claudeContextSpoolTTL = 24 * time.Hour

// claudeContextPruneInterval throttles the prune sweep. The spool is tiny and
// the directory shallow, so hourly is ample.
const claudeContextPruneInterval = time.Hour

// pruneClaudeContextSpools removes readings whose file has not been rewritten
// within claudeContextSpoolTTL. Best-effort: any error is skipped, never fatal.
// A stale spool that survives a failed prune is still refused downstream by the
// freshness bound, so this is disk hygiene rather than a correctness guard.
func pruneClaudeContextSpools(now time.Time) {
	entries, err := os.ReadDir(claudeContextSpoolDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Both the published readings and any temp file orphaned by a crash
		// between CreateTemp and Rename. The temp prefix is swept here because
		// nothing else ever will: writeFileAtomic only cleans up the paths it
		// can still see fail.
		if !strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".tmp-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > claudeContextSpoolTTL {
			_ = os.Remove(filepath.Join(claudeContextSpoolDir(), name))
		}
	}
}

// claudeContextPruner holds the prune throttle for one watcher. Zero value is
// ready to use and prunes on its first tick.
type claudeContextPruner struct {
	lastPrune time.Time
}

func (c *claudeContextPruner) maybe(now time.Time) {
	if !c.lastPrune.IsZero() && now.Sub(c.lastPrune) < claudeContextPruneInterval {
		return
	}
	c.lastPrune = now
	pruneClaudeContextSpools(now)
}

// claudeContextDoctorLine reports whether a context window has actually been
// OBSERVED, as opposed to whether the shim is configured.
//
// The two failure modes it separates look identical from the config alone: a
// correctly-wrapped statusline that Claude Code has simply not redrawn yet, and
// one whose readings are not landing. Downstream, both present as a headroom
// tile with no ceiling — and a tile that says "unknown" cannot tell an engineer
// which of the two they are looking at.
//
// Newest spool across all sessions: this answers "is this machine producing
// readings at all", not "is this particular session". A never-observed window is
// a WARN rather than an error, because it is the correct state for the first
// minute after enable and for a machine that has not opened Claude Code since.
func claudeContextDoctorLine(now time.Time) StatuslineDoctorLine {
	tokens, at, ok := newestClaudeContextSpool()
	if !ok {
		return StatuslineDoctorLine{
			Warn: true,
			Text: "Context window not observed yet — it arrives on the next Claude Code status-line redraw; until then context figures have no ceiling",
		}
	}
	return StatuslineDoctorLine{
		OK: true,
		Text: fmt.Sprintf("Context window observed %s ago — %s tokens",
			now.Sub(at).Round(time.Second), humanTokens(tokens)),
	}
}

// newestClaudeContextSpool returns the most recently written reading across all
// sessions, by file mtime.
func newestClaudeContextSpool() (int64, time.Time, bool) {
	entries, err := os.ReadDir(claudeContextSpoolDir())
	if err != nil {
		return 0, time.Time{}, false
	}
	var bestTokens int64
	var bestAt time.Time
	found := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if found && !info.ModTime().After(bestAt) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		s, ok := readClaudeContextSpool(id)
		if !ok {
			continue
		}
		bestTokens = s.ContextWindowTokens
		bestAt = info.ModTime()
		found = true
	}
	return bestTokens, bestAt, found
}

// humanTokens renders a token count the way the engineer sees it on the
// dashboard ("200k", "1.0M") rather than as a bare seven-digit integer.
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
