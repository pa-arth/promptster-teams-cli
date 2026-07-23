package capture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Rate-limit *window* capture — the provider-agnostic `windowUsage` event.
//
// This is the "usage currency" signal: how much of an engineer's own 5-hour and
// weekly subscription windows are burned, and when each resets. It is emitted as
// the `windowUsage` event and keyed latest-wins per (engineer, provider) on the
// backend — never summed or averaged across engineers.
//
// FROZEN CONTRACT: ~/repos/openspec/changes/usage-window-currency/contract.md is
// the single authority for field names/types across all three repos. All times
// on the wire are ABSOLUTE Unix epoch seconds — the CLI normalizes Codex's
// countdown at capture (see mapCodexRateLimits) so the wire format is uniform.
//
// ABSENT != ZERO: a window field the provider did not report is OMITTED from the
// event Data (nil pointer below). A provider-reported genuine 0 is emitted as 0.
// A NaN/Inf/negative percentage is treated as malformed and OMITTED, never 0.

const (
	providerClaudeCode = "claude_code"
	providerCodex      = "codex"
)

// windowRole classifies a rate-limit window by its DURATION. Codex's
// primary/secondary key names are NOT a stable proxy for 5h-vs-weekly: which
// window a plan reports as "primary" varies (a "team" plan reports a single
// ~monthly window as primary), so contract.md §3 mandates keying on
// window_minutes, never key order.
type windowRole int

const (
	windowUnknown windowRole = iota
	windowFiveHour
	windowWeekly
)

// classifyWindowMinutes maps a window length (minutes) to 5h/weekly by duration.
// The bands are generous around the nominal values (5h ~= 299/300, weekly ~=
// 10079/10080) so a small vendor drift still classifies, while a genuinely
// different window (a ~monthly 43800-minute window seen on team plans) lands in
// windowUnknown and is dropped rather than mislabeled as one of the two the
// contract carries.
func classifyWindowMinutes(m float64) windowRole {
	switch {
	case m >= 60 && m <= 600: // ~5h (299/300)
		return windowFiveHour
	case m >= 6000 && m <= 20000: // ~weekly (10079/10080 = 7d)
		return windowWeekly
	default:
		return windowUnknown
	}
}

// windowReading is the provider-agnostic snapshot lifted on-device. Pointers so
// ABSENT (nil) is distinct from a real 0 — the contract's absent-!=-zero rule
// lives in this type: a nil field is never emitted, a *0.0 field is emitted as 0.
type windowReading struct {
	FiveHourPct      *float64
	WeeklyPct        *float64
	FiveHourResetsAt *int64
	WeeklyResetsAt   *int64
	// ObservedAt is the provider's reading time (Codex token_count ts; Claude
	// tick time), epoch seconds. Always set — it is the freshness anchor.
	ObservedAt int64
}

// empty reports whether the reading carries no window field at all — both pcts
// and both resets absent. An empty reading still has a valid ObservedAt, but
// there is nothing to render, so callers skip emitting it.
func (r windowReading) empty() bool {
	return r.FiveHourPct == nil && r.WeeklyPct == nil &&
		r.FiveHourResetsAt == nil && r.WeeklyResetsAt == nil
}

// sanePct reads a percentage field and returns it only if finite and
// non-negative. NaN/Inf (JSON `null` round-trips to NaN through some encoders)
// and a negative value are malformed → dropped-by-omission, never coerced to 0.
func sanePct(m map[string]interface{}, key string) (float64, bool) {
	v, ok := m[key].(float64)
	if !ok {
		return 0, false
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}

// codexResetsAbsolute resolves a Codex window's reset instant to an ABSOLUTE
// epoch, tolerating both shapes seen in the wild:
//   - `resets_at`: already an absolute epoch (the shape current Codex emits) —
//     used directly.
//   - `resets_in_seconds`: a COUNTDOWN (the shape contract.md §3 documents) —
//     converted on-device to observedAt + seconds so the wire is uniformly
//     absolute.
//
// A missing/malformed value yields ok=false → the reset field is omitted.
func codexResetsAbsolute(w map[string]interface{}, observedAt int64) (int64, bool) {
	if v, ok := w["resets_at"].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 {
		return int64(v), true
	}
	if v, ok := w["resets_in_seconds"].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 {
		return observedAt + int64(v), true
	}
	return 0, false
}

// mapCodexRateLimits maps a Codex `payload.rate_limits` object to a provider-
// agnostic windowReading. It classifies EACH window by window_minutes (never key
// order, contract.md §3), lifts ONLY the scalars (used_percent + reset), and
// drops any window whose length matches neither 5h nor weekly.
func mapCodexRateLimits(rl map[string]interface{}, observedAt int64) windowReading {
	r := windowReading{ObservedAt: observedAt}
	for _, key := range []string{"primary", "secondary"} {
		w, ok := rl[key].(map[string]interface{})
		if !ok {
			continue
		}
		wm, ok := w["window_minutes"].(float64)
		if !ok {
			continue
		}
		pct, pctOK := sanePct(w, "used_percent")
		reset, resetOK := codexResetsAbsolute(w, observedAt)
		switch classifyWindowMinutes(wm) {
		case windowFiveHour:
			if pctOK {
				p := pct
				r.FiveHourPct = &p
			}
			if resetOK {
				t := reset
				r.FiveHourResetsAt = &t
			}
		case windowWeekly:
			if pctOK {
				p := pct
				r.WeeklyPct = &p
			}
			if resetOK {
				t := reset
				r.WeeklyResetsAt = &t
			}
		case windowUnknown:
			// A window whose length is neither 5h nor weekly (e.g. a ~monthly
			// team-plan window) has no slot in the frozen contract — drop it
			// rather than mislabel it.
		}
	}
	return r
}

// codexTokenCountLine is the MINIMAL projection of a rollout line we parse when
// scanning for window readings. Deliberately narrow: it names only the
// timestamp, the payload type discriminator, and the rate_limits scalars — so a
// token_count line's surrounding usage counts, and every non-token_count line's
// prompt/patch/output content, are never lifted into memory beyond the raw line
// (which is discarded per-iteration and never logged or emitted).
type codexTokenCountLine struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type       string                 `json:"type"`
		RateLimits map[string]interface{} `json:"rate_limits"`
	} `json:"payload"`
}

// latestCodexWindowReading scans rollout files under sessionsDir for the most
// recent `token_count` event (by its OWN timestamp) that carries a rate_limits
// object, maps it, and returns the reading plus the rollout's session id.
//
// Account-level rate limits are GLOBAL to the engineer, not workspace-scoped, so
// this looks across ALL rollouts, not just the workspace-matched ones the
// capture watcher tails. Only files modified at/after modifiedAfter are read,
// which bounds the per-tick cost to the handful of recently-active sessions.
//
// Privacy: only the fields in codexTokenCountLine are ever parsed out; the raw
// line bytes are local to the loop and are never retained, logged, or emitted —
// including on the error path (a parse failure is silently skipped).
func latestCodexWindowReading(sessionsDir string, modifiedAfter time.Time) (windowReading, string, bool) {
	var best windowReading
	var bestSession string
	var bestTs time.Time
	found := false

	_ = filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, ".jsonl") {
			return nil
		}
		if info.ModTime().Before(modifiedAfter) {
			return nil
		}
		ts, rl, ok := scanRolloutLatestRateLimits(path)
		if !ok {
			return nil
		}
		if found && !ts.After(bestTs) {
			return nil
		}
		reading := mapCodexRateLimits(rl, ts.Unix())
		if reading.empty() {
			// The latest rate_limits in this file mapped to nothing we carry
			// (e.g. only a monthly window). Don't let it shadow an earlier file's
			// usable-but-older reading — skip it.
			return nil
		}
		best = reading
		bestSession = codexSessionIDFromPath(path)
		bestTs = ts
		found = true
		return nil
	})

	return best, bestSession, found
}

// scanRolloutLatestRateLimits returns the timestamp + rate_limits map of the
// latest token_count-with-rate_limits line in a single rollout file. Read-only,
// line-by-line; only the minimal projection is parsed.
func scanRolloutLatestRateLimits(path string) (time.Time, map[string]interface{}, bool) {
	// #nosec G304 -- path is a Codex rollout discovered under the sessions dir, not user input; opened read-only and only the timestamp + rate_limits scalars are read.
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, nil, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	var bestTs time.Time
	var bestRL map[string]interface{}
	found := false
	for scanner.Scan() {
		line := scanner.Bytes()
		// Cheap pre-filter so we only JSON-parse plausible lines.
		if !containsToken(line, "token_count") || !containsToken(line, "rate_limits") {
			continue
		}
		var rec codexTokenCountLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Payload.Type != "token_count" || rec.Payload.RateLimits == nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, rec.Timestamp)
		if err != nil {
			// RFC3339Nano (fractional seconds) — the shape Codex actually writes.
			ts, err = time.Parse(time.RFC3339Nano, rec.Timestamp)
			if err != nil {
				continue
			}
		}
		if found && !ts.After(bestTs) {
			continue
		}
		bestTs = ts
		bestRL = rec.Payload.RateLimits
		found = true
	}
	return bestTs, bestRL, found
}

// containsToken is a byte-level substring check used to skip JSON-parsing lines
// that cannot be a token_count-with-rate_limits event.
func containsToken(b []byte, tok string) bool {
	return strings.Contains(string(b), tok)
}

// buildWindowUsageEvent assembles the provider-agnostic `windowUsage` event from
// a reading. Only NON-NIL window fields are placed in Data (absent != zero). The
// id is derived deterministically from the reading's content so an at-least-once
// resend of the SAME logical reading collapses on ingest.
func buildWindowUsageEvent(provider string, r windowReading, capturedAt int64, sessionID, deviceID string) event.Event {
	e := event.NewEvent("windowUsage", sessionID)
	e.Source = "cli"
	e.Actor = event.SystemActor()
	e.DeviceID = deviceID

	data := map[string]interface{}{
		"provider":   provider,
		"observedAt": r.ObservedAt,
		"capturedAt": capturedAt,
	}
	if r.FiveHourPct != nil {
		data["fiveHourPct"] = *r.FiveHourPct
	}
	if r.WeeklyPct != nil {
		data["weeklyPct"] = *r.WeeklyPct
	}
	if r.FiveHourResetsAt != nil {
		data["fiveHourResetsAt"] = *r.FiveHourResetsAt
	}
	if r.WeeklyResetsAt != nil {
		data["weeklyResetsAt"] = *r.WeeklyResetsAt
	}
	e.Data = data

	// Deterministic id keyed on provider + device + observedAt + the four window
	// scalars: the same logical reading always yields the same id (idempotent
	// resend), while a changed reading is a new id (latest-wins on the backend).
	e.ID = event.DeterministicUUID(fmt.Sprintf(
		"windowUsage:%s:%s:%d:%s:%s:%s:%s",
		provider, deviceID, r.ObservedAt,
		ptrFloatKey(r.FiveHourPct), ptrFloatKey(r.WeeklyPct),
		ptrIntKey(r.FiveHourResetsAt), ptrIntKey(r.WeeklyResetsAt),
	))
	return e
}

func ptrFloatKey(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%g", *p)
}

func ptrIntKey(p *int64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *p)
}

// queueWindowUsageEvent projects, signs, and queues a windowUsage event through
// the same durable path every other captured event uses. captureProse is threaded
// only for signature parity — windowUsage carries no prose.
func queueWindowUsageEvent(e event.Event, captureProse bool) {
	ev := e
	if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
		fmt.Fprintf(os.Stderr, "window-usage: buffer error: %v\n", err)
	}
	if err := outbox.Append(ev); err != nil {
		fmt.Fprintf(os.Stderr, "window-usage: queue error: %v\n", err)
	}
}

// --- Codex window emission (wired into the codex watcher poll loop) -----------

// codexWindowScanInterval throttles the (relatively expensive) rollout scan so
// it runs on a slow cadence independent of the 3s capture poll. Rate-limit
// windows move on the order of minutes, so a per-minute scan is ample.
const codexWindowScanInterval = 60 * time.Second

// codexWindowLookback bounds which rollout files the scan reads to those touched
// recently — a window reading older than this is stale for a live signal anyway.
const codexWindowLookback = 6 * time.Hour

// codexWindowEmitter holds the throttle + de-dup state for one watcher's Codex
// window emission. Zero value is ready to use.
type codexWindowEmitter struct {
	lastScan     time.Time
	lastObserved int64
}

// maybe emits a Codex windowUsage event if the scan interval has elapsed and a
// reading newer than the last emitted one is available. Best-effort and
// non-fatal: any failure is logged to stderr (debug) and skipped, never wedging
// the capture loop.
func (c *codexWindowEmitter) maybe(session Session, now time.Time, captureProse bool) {
	if !c.lastScan.IsZero() && now.Sub(c.lastScan) < codexWindowScanInterval {
		return
	}
	c.lastScan = now
	reading, rolloutSession, ok := latestCodexWindowReading(codexSessionsDir(), now.Add(-codexWindowLookback))
	if !ok {
		return
	}
	if reading.ObservedAt <= c.lastObserved {
		return
	}
	c.lastObserved = reading.ObservedAt
	sessionID := rolloutSession
	if sessionID == "" {
		sessionID = session.DeviceID
	}
	e := buildWindowUsageEvent(providerCodex, reading, now.Unix(), sessionID, session.DeviceID)
	queueWindowUsageEvent(e, captureProse)
	if verboseWatch() {
		fmt.Fprintf(os.Stderr, "codex-watcher: emitted windowUsage (observedAt=%d)\n", reading.ObservedAt)
	}
}

// --- Claude window emission (spool written by the statusline shim, drained by
// the claude watcher poll loop) ----------------------------------------------

// claudeWindowSpoolPath is the latest-wins handoff file the statusline shim
// writes (fast, fail-open) and the claude watcher drains (emits + deletes). It
// lives in the state dir alongside the other watcher state.
func claudeWindowSpoolPath() string {
	return filepath.Join(state.StateDir(), "claude-window.json")
}

// claudeWindowSpool is the on-disk handoff shape. Absent fields are absent
// (pointers) so absent != zero survives the spool exactly as it does on the wire.
type claudeWindowSpool struct {
	FiveHourPct      *float64 `json:"fiveHourPct,omitempty"`
	WeeklyPct        *float64 `json:"weeklyPct,omitempty"`
	FiveHourResetsAt *int64   `json:"fiveHourResetsAt,omitempty"`
	WeeklyResetsAt   *int64   `json:"weeklyResetsAt,omitempty"`
	ObservedAt       int64    `json:"observedAt"`
}

// writeClaudeWindowSpool atomically overwrites the spool with the latest reading
// (latest-wins). Best-effort: the shim must never block or fail the statusline
// render on a spool error.
func writeClaudeWindowSpool(r windowReading) error {
	s := claudeWindowSpool{
		FiveHourPct:      r.FiveHourPct,
		WeeklyPct:        r.WeeklyPct,
		FiveHourResetsAt: r.FiveHourResetsAt,
		WeeklyResetsAt:   r.WeeklyResetsAt,
		ObservedAt:       r.ObservedAt,
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(claudeWindowSpoolPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := claudeWindowSpoolPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, claudeWindowSpoolPath())
}

// readClaudeWindowSpool loads and REMOVES the spool (drain semantics): a reading
// is emitted exactly once. Returns ok=false when absent or unparseable.
func readClaudeWindowSpool() (windowReading, bool) {
	path := claudeWindowSpoolPath()
	data, err := os.ReadFile(path) // #nosec G304 -- fixed path under the state dir.
	if err != nil {
		return windowReading{}, false
	}
	var s claudeWindowSpool
	if err := json.Unmarshal(data, &s); err != nil {
		_ = os.Remove(path)
		return windowReading{}, false
	}
	_ = os.Remove(path)
	r := windowReading{
		FiveHourPct:      s.FiveHourPct,
		WeeklyPct:        s.WeeklyPct,
		FiveHourResetsAt: s.FiveHourResetsAt,
		WeeklyResetsAt:   s.WeeklyResetsAt,
		ObservedAt:       s.ObservedAt,
	}
	if r.empty() {
		return windowReading{}, false
	}
	return r, true
}

// claudeWindowEmitter drains the statusline spool and emits a claude_code
// windowUsage event. De-dups on observedAt so a spool that was not rewritten
// between polls is not re-emitted.
type claudeWindowEmitter struct {
	lastObserved int64
}

func (c *claudeWindowEmitter) maybe(session Session, now time.Time, captureProse bool) {
	reading, ok := readClaudeWindowSpool()
	if !ok {
		return
	}
	if reading.ObservedAt <= c.lastObserved {
		return
	}
	c.lastObserved = reading.ObservedAt
	e := buildWindowUsageEvent(providerClaudeCode, reading, now.Unix(), session.DeviceID, session.DeviceID)
	queueWindowUsageEvent(e, captureProse)
	if verboseWatch() {
		fmt.Fprintf(os.Stderr, "claude-watcher: emitted windowUsage (observedAt=%d)\n", reading.ObservedAt)
	}
}
