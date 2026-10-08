package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// usageLimitHit detection (design.md §4). One event per exhausted window:
// (provider, accountRef, window, resetsAt), however many requests were refused.

const (
	evidenceRejectedRequest = "rejected_request"
	evidenceWindowFull      = "window_full"
)

var limitWindowPattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

type limitHit struct {
	Provider      string
	Window        string
	ResetsAt      int64
	FirstSeenAt   int64
	Evidence      string
	OverageStatus string
	SessionID     string
}

func (h limitHit) key() string { return fmt.Sprintf("%s|%s|%d", h.Provider, h.Window, h.ResetsAt) }

// mergeHit keeps one hit per window, with the earliest firstSeenAt.
func mergeHit(m map[string]limitHit, h limitHit) {
	if prev, ok := m[h.key()]; ok && prev.FirstSeenAt <= h.FirstSeenAt {
		return
	}
	m[h.key()] = h
}

func sortedHits(m map[string]limitHit) []limitHit {
	out := make([]limitHit, 0, len(m))
	for _, h := range m {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeenAt < out[j].FirstSeenAt })
	return out
}

// --- Claude: rejected requests in transcripts --------------------------------

// claudeRateLimitRow names only what the detector needs. `error` is raw so a
// row whose error is an object still parses.
type claudeRateLimitRow struct {
	Timestamp         string          `json:"timestamp"`
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
	Error             json.RawMessage `json:"error"`
	QuotaLimits       *struct {
		Status         string  `json:"status"`
		RateLimitType  string  `json:"rateLimitType"`
		ResetsAt       float64 `json:"resetsAt"`
		OverageStatus  string  `json:"overageStatus"`
		IsUsingOverage bool    `json:"isUsingOverage"`
	} `json:"quotaLimits"`
}

// claudeLimitHitFromLine returns a hit for a transcript row that is a refused
// request: isApiErrorMessage, error "rate_limit", quotaLimits.status "rejected",
// and not running on overage.
func claudeLimitHitFromLine(line []byte) (limitHit, bool) {
	if !bytes.Contains(line, []byte(`"rate_limit"`)) || !bytes.Contains(line, []byte(`quotaLimits`)) {
		return limitHit{}, false
	}
	var row claudeRateLimitRow
	if json.Unmarshal(line, &row) != nil {
		return limitHit{}, false
	}
	q := row.QuotaLimits
	if !row.IsAPIErrorMessage || string(bytes.TrimSpace(row.Error)) != `"rate_limit"` || q == nil ||
		q.Status != "rejected" || q.IsUsingOverage {
		return limitHit{}, false
	}
	window := strings.ToLower(q.RateLimitType)
	if !limitWindowPattern.MatchString(window) || math.IsNaN(q.ResetsAt) || q.ResetsAt <= 0 {
		return limitHit{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, row.Timestamp)
	if err != nil {
		return limitHit{}, false
	}
	return limitHit{
		Provider: providerClaudeCode, Window: window, ResetsAt: int64(q.ResetsAt),
		FirstSeenAt: ts.Unix(), Evidence: evidenceRejectedRequest,
		OverageStatus: vendorToken(q.OverageStatus, vendorTokenPattern),
	}, true
}

// scanClaudeLimitHits walks every transcript (all workspaces: a limit is the
// account's, not the project's) modified at/after modifiedAfter.
func scanClaudeLimitHits(projectsDir string, modifiedAfter time.Time) []limitHit {
	hits := map[string]limitHit{}
	_ = filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") || info.ModTime().Before(modifiedAfter) {
			return nil
		}
		forEachSmallLine(path, func(line []byte) {
			if h, ok := claudeLimitHitFromLine(line); ok {
				h.SessionID = claudeSessionIDFromPath(path)
				mergeHit(hits, h)
			}
		})
		return nil
	})
	return sortedHits(hits)
}

// --- Codex: every token_count, any window at 100% ----------------------------

func codexWindowName(minutes float64) string {
	switch classifyWindowMinutes(minutes) {
	case windowFiveHour:
		return "five_hour"
	case windowWeekly:
		return "weekly"
	}
	return fmt.Sprintf("other_%d", int64(minutes))
}

// codexLimitHits maps one token_count's rate_limits to hits: each window with
// used_percent >= 100. A non-null rate_limit_reached_type with no window at
// 100 marks the fullest window (vocabulary unverified; it was null on every
// observed row, even at 100%).
func codexLimitHits(rl map[string]interface{}, observedAt int64) []limitHit {
	type win struct {
		name   string
		pct    float64
		reset  int64
		hasPct bool
	}
	var wins []win
	for _, key := range []string{"primary", "secondary"} {
		w, ok := rl[key].(map[string]interface{})
		if !ok {
			continue
		}
		wm, ok := w["window_minutes"].(float64)
		if !ok || wm <= 0 {
			continue
		}
		reset, ok := codexResetsAbsolute(w, observedAt)
		if !ok {
			continue
		}
		pct, pctOK := sanePct(w, "used_percent")
		wins = append(wins, win{codexWindowName(wm), pct, reset, pctOK})
	}
	var out []limitHit
	fullest := -1
	for i, w := range wins {
		if w.hasPct && w.pct >= 100 {
			out = append(out, limitHit{Provider: providerCodex, Window: w.name, ResetsAt: w.reset, FirstSeenAt: observedAt, Evidence: evidenceWindowFull})
		}
		if w.hasPct && (fullest < 0 || w.pct > wins[fullest].pct) {
			fullest = i
		}
	}
	if reached, _ := rl["rate_limit_reached_type"].(string); len(out) == 0 && reached != "" && fullest >= 0 {
		w := wins[fullest]
		out = append(out, limitHit{Provider: providerCodex, Window: w.name, ResetsAt: w.reset, FirstSeenAt: observedAt, Evidence: evidenceWindowFull})
	}
	return out
}

// scanCodexLimitHits reads EVERY token_count line (not the latest per file), so
// a window that hit 100 and reset between watcher scans is still seen.
func scanCodexLimitHits(sessionsDir string, modifiedAfter time.Time) []limitHit {
	hits := map[string]limitHit{}
	walkCodexRollouts(sessionsDir, modifiedAfter, func(path string) {
		sid := codexSessionIDFromPath(path)
		forEachCodexTokenCount(path, func(ts time.Time, rl map[string]interface{}) {
			for _, h := range codexLimitHits(rl, ts.Unix()) {
				h.SessionID = sid
				mergeHit(hits, h)
			}
		})
	})
	return sortedHits(hits)
}

func buildUsageLimitHitEvent(h limitHit, accountRef string, capturedAt int64, deviceID string) event.Event {
	e := event.NewEvent("usageLimitHit", h.SessionID)
	e.Source = "cli"
	e.Actor = event.SystemActor()
	e.DeviceID = deviceID
	data := map[string]interface{}{
		"provider":    h.Provider,
		"window":      h.Window,
		"resetsAt":    h.ResetsAt,
		"firstSeenAt": h.FirstSeenAt,
		"evidence":    h.Evidence,
		"capturedAt":  capturedAt,
	}
	if h.OverageStatus != "" {
		data["overageStatus"] = h.OverageStatus
	}
	if accountRef != "" {
		data["accountRef"] = accountRef
	}
	e.Data = data
	e.ID = event.DeterministicUUID(fmt.Sprintf("usageLimitHit:%s:%s:%s:%d", h.Provider, accountRef, h.Window, h.ResetsAt))
	return e
}

// --- emitter: cadence + persisted de-dup --------------------------------------

const (
	planTierReadInterval = 6 * time.Hour
	limitHitScanInterval = 5 * time.Minute
	// First scan on a machine reaches back over the backend's 30 d retention.
	limitHitFirstLookback = 30 * 24 * time.Hour
)

// planLimitState is persisted per provider (one watcher owns each provider, so
// files never race). It makes the daily heartbeat and one-event-per-window
// hold across watcher restarts: the backend de-dups on content, and
// capturedAt differs on every re-emit.
type planLimitState struct {
	TierKey  string           `json:"tierKey,omitempty"`
	TierDate string           `json:"tierDate,omitempty"`
	HitScan  int64            `json:"hitScan,omitempty"`
	Hits     map[string]int64 `json:"hits,omitempty"` // hit key -> resetsAt
}

func planLimitStatePath(provider string) string {
	return filepath.Join(state.StateDir(), "plan-limits-"+provider+".json")
}

func loadPlanLimitState(provider string) planLimitState {
	var s planLimitState
	data, err := os.ReadFile(planLimitStatePath(provider)) // #nosec G304 -- fixed path under the state dir.
	if err == nil {
		_ = json.Unmarshal(data, &s)
	}
	if s.Hits == nil {
		s.Hits = map[string]int64{}
	}
	return s
}

func savePlanLimitState(provider string, s planLimitState) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	dir := state.StateDir()
	if os.MkdirAll(dir, 0o700) == nil {
		_ = writeFileAtomic(dir, planLimitStatePath(provider), data)
	}
}

// planLimitEmitter runs inside one provider's watcher loop. readTier returns
// ok=false to emit nothing (Cursor with no store anywhere); scanHits is nil for
// providers with no local hit source.
type planLimitEmitter struct {
	provider string
	readTier func(now time.Time) (planTierReading, bool)
	scanHits func(modifiedAfter time.Time) []limitHit
	lastTier time.Time
	lastScan time.Time
	tier     planTierReading
	tierOK   bool
	tierDate string // UTC date the heartbeat was last evaluated
}

func (p *planLimitEmitter) maybe(session Session, now time.Time, captureProse bool) {
	date := now.UTC().Format("2006-01-02")
	doTier := p.lastTier.IsZero() || now.Sub(p.lastTier) >= planTierReadInterval
	doHits := p.scanHits != nil && (p.lastScan.IsZero() || now.Sub(p.lastScan) >= limitHitScanInterval)
	if !doTier && !doHits && date == p.tierDate {
		return
	}
	st := loadPlanLimitState(p.provider)
	dirty := false
	if doTier {
		p.lastTier = now
		p.tier, p.tierOK = p.readTier(now)
	}
	p.tierDate = date
	if p.tierOK {
		// Emit on change, plus one heartbeat per UTC day.
		if key := p.tier.AccountRef + "#" + p.tier.tupleKey(); key != st.TierKey || date != st.TierDate {
			if queuePlanEvent(buildPlanTierEvent(p.tier, now.Unix(), session.DeviceID), captureProse) == nil {
				st.TierKey, st.TierDate, dirty = key, date, true
			}
		}
	}
	if doHits {
		p.lastScan = now
		after := now.Add(-limitHitFirstLookback)
		if st.HitScan > 0 {
			// Overlap a little: a file can be written during the previous scan.
			after = time.Unix(st.HitScan, 0).Add(-10 * time.Minute)
		}
		queueFailed := false
		for _, h := range p.scanHits(after) {
			if _, seen := st.Hits[h.key()]; seen {
				continue
			}
			if queuePlanEvent(buildUsageLimitHitEvent(h, p.tier.AccountRef, now.Unix(), session.DeviceID), captureProse) != nil {
				queueFailed = true
				continue
			}
			st.Hits[h.key()] = h.ResetsAt
			if verboseWatch() {
				fmt.Fprintf(os.Stderr, "%s: emitted usageLimitHit (window=%s resetsAt=%d)\n", p.provider, h.Window, h.ResetsAt)
			}
		}
		for k, reset := range st.Hits {
			if reset < now.Add(-2*limitHitFirstLookback).Unix() {
				delete(st.Hits, k)
			}
		}
		// Hold the scan position while any hit failed to queue, so the next scan
		// still reaches it; queued hits are deduped by st.Hits.
		if !queueFailed {
			st.HitScan = now.Unix()
		}
		dirty = true
	}
	if dirty {
		savePlanLimitState(p.provider, st)
	}
}

func newClaudePlanLimitEmitter() *planLimitEmitter {
	return &planLimitEmitter{
		provider: providerClaudeCode,
		readTier: func(time.Time) (planTierReading, bool) { return readClaudePlanTier(claudeProfilePath()), true },
		scanHits: func(after time.Time) []limitHit { return scanClaudeLimitHits(ClaudeProjectsDir(), after) },
	}
}

func newCodexPlanLimitEmitter() *planLimitEmitter {
	return &planLimitEmitter{
		provider: providerCodex,
		readTier: func(now time.Time) (planTierReading, bool) {
			return readCodexPlanTier(codexSessionsDir(), filepath.Join(codexHome(), "auth.json"), now), true
		},
		scanHits: func(after time.Time) []limitHit { return scanCodexLimitHits(codexSessionsDir(), after) },
	}
}

// Cursor has no local hit source; its cycle_cap hit is derived server-side
// from cursorVendorSnapshot rows (design §4).
func newCursorPlanLimitEmitter() *planLimitEmitter {
	return &planLimitEmitter{
		provider: providerCursor,
		readTier: func(time.Time) (planTierReading, bool) { return readCursorPlanTier(cursorPlanStateDBCandidates()) },
	}
}
