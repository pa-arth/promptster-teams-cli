package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// The generation -> model join, on device.
//
// WHY THERE IS STATE HERE AT ALL. Cursor delivers one turn's facts across two
// hook invocations: `afterAgentThought` carries the resolved model id and no
// tokens, `stop` carries the tokens and reports the routing sentinel
// "default" on both model spellings. Each invocation is a fresh short-lived
// process reading one JSON payload from stdin, so nothing links them in memory.
//
// WHY NOT SERVER-SIDE. Superficially cleaner — no file, no eviction, survives
// reordering. It fails on the shape of the consumer: the backend's readUsage
// returns null unless `model` is a non-empty string ON THE SAME ROW, so a
// server-side join would need a new projector stage materialising a joined row
// before the analyzer runs. The device already holds both payloads, seconds
// apart.
//
// WHY NOT INHERIT THE SESSION'S LAST MODEL. Because Cursor auto-routes by
// default and genuinely switches models mid-conversation — on the live external
// org the Cursor rail reports grok, composer-2.5 and claude-opus-5 across one
// fleet — so "the model this session was using" is not a fact about this turn.
// A generation-keyed entry costs the same and cannot be wrong in that way.

// cursorGenerationsPath holds the join cache and the local counters that keep
// its coverage observable after the probe that measured it is gone.
func cursorGenerationsPath() string {
	return filepath.Join(state.StateDir(), "cursor-generations.json")
}

type cursorGenerations struct {
	V       int                         `json:"v"`
	Entries map[string]cursorGeneration `json:"entries"`
	// Counters are cumulative for this machine's lifetime and are read by
	// `doctor`. They exist because §0.1's model-coverage number was measured by
	// a probe that is torn down afterwards: a measurement recorded only as a
	// number in a spec has no expiry and no observable moment of failure, so the
	// same quantity is counted here forever, cheaply.
	UsageRows     int64 `json:"usageRows"`
	ModellessRows int64 `json:"modellessRows"`
	// NonCumulative* are the owning observation for the premise that this rail's
	// counts are NOT cumulative (see usageEvent's `usageScope: "request"`). A
	// cumulative counter cannot decrease, so every observed decrease is positive
	// evidence for the premise and a run of zero decreases across many
	// comparisons is the shape that would refute it.
	//
	// ⚠ THIS COUNTER PROVES NON-CUMULATIVE AND NOTHING MORE. It was named
	// PerRequest* until 2026-08-22 and read as though a decrease established
	// that a row IS one request. It does not, and the row is not: Cursor emits
	// a per-TURN sum across the turn's model calls. That reading is a separate
	// premise living on the backend's TURN_SUM_INTEGRATIONS, and nothing here
	// can check it — the two are numerically identical on a single-call turn,
	// which is exactly the shape the calibration below happened to use.
	//
	// The JSON keys deliberately KEEP the old spelling. They are the on-disk
	// names on every enrolled machine, and these counters are worth having only
	// because they accumulate over a machine's lifetime; renaming the key would
	// silently reset the fleet's evidence to zero to fix a word.
	//
	// Calibrated 2026-08-18 on Cursor 3.12.17: output fell 902 -> 525 across
	// consecutive generations of one conversation. REFUTED IF this counter
	// reports many comparisons and zero decreases on a fleet whose Cursor
	// version has moved — that is the signal to re-probe before trusting any
	// per-turn figure, not a reason to keep asserting the tag.
	NonCumulativeComparisons int64 `json:"perRequestComparisons"`
	NonCumulativeDecreases   int64 `json:"perRequestDecreases"`
	// LastOutput is the previous generation's output count per conversation,
	// kept only to make the comparison above possible. It is a counter input,
	// never an event field.
	LastOutput map[string]cursorLastOutput `json:"lastOutput"`
}

type cursorGeneration struct {
	Model string `json:"model"`
	TsMs  int64  `json:"tsMs"`
}

type cursorLastOutput struct {
	Tokens int64 `json:"tokens"`
	TsMs   int64 `json:"tsMs"`
}

const (
	cursorGenerationsVersion = 1
	// cursorGenerationTTL bounds by AGE. A generation is one agent turn: the
	// thought and the stop that follows it are seconds apart, and a turn that
	// has not ended within this window is not going to be joined by a longer
	// wait. Generous enough for a genuinely long tool-using turn, short enough
	// that a machine left running for a month carries nothing from last week.
	cursorGenerationTTL = 6 * time.Hour
	// cursorGenerationsMax bounds by COUNT, because age alone is not a bound: a
	// busy machine with several conversations open can mint generations far
	// faster than the TTL retires them. Oldest-first eviction, so the entries
	// dropped are the ones whose `stop` is least likely to still be coming.
	cursorGenerationsMax = 512
	// cursorLastOutputMax bounds the monotonicity inputs the same way. One entry
	// per live conversation; a handful is the real number.
	cursorLastOutputMax = 64
	// cursorGenerationUUIDLen is the length of a canonical 8-4-4-4-12 UUID, which
	// is the prefix Cursor suffixes with `-<n>-<slug>` to identify a sub-request
	// within one turn. See cursorGenerationBaseID.
	cursorGenerationUUIDLen = 36
)

func loadCursorGenerations() cursorGenerations {
	c := cursorGenerations{
		V:          cursorGenerationsVersion,
		Entries:    map[string]cursorGeneration{},
		LastOutput: map[string]cursorLastOutput{},
	}
	data, err := os.ReadFile(cursorGenerationsPath()) // #nosec G304 -- state dir path.
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	if c.Entries == nil {
		c.Entries = map[string]cursorGeneration{}
	}
	if c.LastOutput == nil {
		c.LastOutput = map[string]cursorLastOutput{}
	}
	return c
}

// saveCursorGenerations writes through a temp file and a rename, so a hook
// killed mid-write cannot leave the next invocation reading a truncated file.
// Errors are swallowed on purpose: this runs inside the engineer's agent loop
// and a cache miss costs a modelless usage row, which is an outcome this design
// already handles honestly.
func saveCursorGenerations(c cursorGenerations) {
	c.V = cursorGenerationsVersion
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cursorGenerationsPath()), 0o700); err != nil {
		return
	}
	tmp := cursorGenerationsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, cursorGenerationsPath())
}

// pruneCursorGenerations enforces both bounds. Called on every write rather
// than on a timer: only the hook process touches this file, and a machine that
// stops hooking is a machine whose file stops growing.
func pruneCursorGenerations(c *cursorGenerations, now time.Time) {
	for k, v := range c.Entries {
		if now.Sub(time.UnixMilli(v.TsMs)) > cursorGenerationTTL {
			delete(c.Entries, k)
		}
	}
	if len(c.Entries) > cursorGenerationsMax {
		keys := make([]string, 0, len(c.Entries))
		for k := range c.Entries {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return c.Entries[keys[i]].TsMs < c.Entries[keys[j]].TsMs })
		for _, k := range keys[:len(c.Entries)-cursorGenerationsMax] {
			delete(c.Entries, k)
		}
	}
	for k, v := range c.LastOutput {
		if now.Sub(time.UnixMilli(v.TsMs)) > cursorGenerationTTL {
			delete(c.LastOutput, k)
		}
	}
	if len(c.LastOutput) > cursorLastOutputMax {
		keys := make([]string, 0, len(c.LastOutput))
		for k := range c.LastOutput {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return c.LastOutput[keys[i]].TsMs < c.LastOutput[keys[j]].TsMs })
		for _, k := range keys[:len(c.LastOutput)-cursorLastOutputMax] {
			delete(c.LastOutput, k)
		}
	}
}

// cursorGenerationBaseID reduces a generation id to the TURN it belongs to.
//
// MEASURED 2026-08-22, Cursor IDE 3.17.8, model picker on Auto. One turn emits
// afterAgentThought TWICE per thought, under two different ids:
//
//	gen 3a8b6e45-…-617844887809         model_id "default"       <- routing sentinel
//	gen 3a8b6e45-…-617844887809-3-1v0i  model_id "composer-2.5"  <- the resolved model
//
// and `stop` carries the BARE id with the tokens. So the two halves of this join
// were keyed differently and could never meet. The cache only ever learned
// SUFFIXED ids — the bare thought reports the sentinel, which modelLabel
// correctly rejects, so it records nothing — while the lookup only ever asked
// for BARE ones.
//
// WHY NOBODY SAW IT. On a PINNED model `stop` resolves its own model and never
// consults the cache, so the join is dead code on exactly the turns that work.
// It is consulted only under Auto, where it missed every time. The failure was
// therefore invisible in aggregate and perfectly correlated with the one case it
// existed to serve: Auto turns are precisely the turns that arrived unpriced.
//
// The suffix is `-<n>-<slug>` appended to a full UUID, so the base is the first
// 36 characters and nothing needs parsing. The UUID shape is VERIFIED rather
// than assumed: an id that is not a UUID keeps its full value, because
// truncating an unrecognised shape would mint a key that silently joins two
// unrelated turns — a quieter and worse failure than the one being fixed.
//
// Normalising on BOTH sides is what makes the entry findable; doing it on one
// side only moves the miss rather than closing it. Entries written by an older
// build under a suffixed key simply go unread and expire on the TTL, so no
// migration is needed.
func cursorGenerationBaseID(id string) string {
	if len(id) <= cursorGenerationUUIDLen || id[cursorGenerationUUIDLen] != '-' {
		return id
	}
	if !isCursorGenerationUUID(id[:cursorGenerationUUIDLen]) {
		return id
	}
	return id[:cursorGenerationUUIDLen]
}

// isCursorGenerationUUID reports whether s is exactly a canonical 8-4-4-4-12
// hex UUID. Deliberately hand-rolled and allocation-free: this runs inside the
// engineer's agent loop on every hook invocation.
func isCursorGenerationUUID(s string) bool {
	if len(s) != cursorGenerationUUIDLen {
		return false
	}
	for i := 0; i < cursorGenerationUUIDLen; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// recordCursorGenerationModel remembers which model produced a generation.
//
// afterAgentThought fires many times per turn and every one resolves the same
// model, so an unchanged entry is skipped rather than rewritten — the same churn
// argument the transcript-claim ledger makes, and it matters more here because
// this write is inside the agent loop.
func recordCursorGenerationModel(generationID, model string) {
	if generationID == "" || model == "" {
		return
	}
	generationID = cursorGenerationBaseID(generationID)
	_ = sign.WithBufferLock(cursorGenerationsPath()+".lock", func() error {
		c := loadCursorGenerations()
		if prev, ok := c.Entries[generationID]; ok && prev.Model == model {
			return nil
		}
		now := time.Now()
		c.Entries[generationID] = cursorGeneration{Model: model, TsMs: now.UnixMilli()}
		pruneCursorGenerations(&c, now)
		saveCursorGenerations(c)
		return nil
	})
}

// cursorGenerationModel is the read half of the join: the model observed for
// this generation, or "" when none was. An expired entry answers "" rather than
// a stale model, because inheriting across the TTL is the failure this cache is
// keyed per-generation to avoid.
func cursorGenerationModel(generationID string) string {
	if generationID == "" {
		return ""
	}
	generationID = cursorGenerationBaseID(generationID)
	e, ok := loadCursorGenerations().Entries[generationID]
	if !ok || time.Since(time.UnixMilli(e.TsMs)) > cursorGenerationTTL {
		return ""
	}
	return e.Model
}

// recordCursorUsageObservation counts what §0.1 measured with a probe, so the
// number survives the probe: how many usage rows this machine emitted and how
// many of them had no model to price.
//
// It also feeds the per-request premise its evidence, by comparing this
// generation's output count with the previous generation of the same
// conversation. A decrease is impossible for a cumulative counter, so the
// counter of decreases IS the observation, and `doctor` reports it.
func recordCursorUsageObservations(conversationID, model string, events []event.Event) {
	for _, ev := range events {
		if ev.Kind != "ai_response" {
			continue
		}
		var out *int64
		if d, ok := ev.Data.(map[string]interface{}); ok {
			if v, ok := d["outputTokens"].(int64); ok {
				out = &v
			}
		}
		recordCursorUsageObservation(conversationID, model, out)
	}
}

func recordCursorUsageObservation(conversationID, model string, outputTokens *int64) {
	_ = sign.WithBufferLock(cursorGenerationsPath()+".lock", func() error {
		c := loadCursorGenerations()
		now := time.Now()
		c.UsageRows++
		if model == "" {
			c.ModellessRows++
		}
		if outputTokens != nil && conversationID != "" {
			if prev, ok := c.LastOutput[conversationID]; ok {
				c.NonCumulativeComparisons++
				if *outputTokens < prev.Tokens {
					c.NonCumulativeDecreases++
				}
			}
			c.LastOutput[conversationID] = cursorLastOutput{Tokens: *outputTokens, TsMs: now.UnixMilli()}
		}
		pruneCursorGenerations(&c, now)
		saveCursorGenerations(c)
		return nil
	})
}
