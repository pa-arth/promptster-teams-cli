package capture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Durability follows AI-authored lines FORWARD through default-branch history —
// entirely on-device, emitting metadata-only verdicts. It never ships bytes,
// diffs, or file contents: the interval math runs against the SAME single
// `git show --unified=0` diff the commit-attribution path already spawns (one
// process per commit, never per file/line), and only integer line ranges leave.
//
// A living-AI-line ledger holds, per (rootKey, path), the AI-authored line
// spans currently believed live, each stamped with when it was born. Every new
// default-branch commit REMAPS those spans by the commit's hunks: a span whose
// old-side line a hunk rewrites is CHURN (emitted at commit time, dropped from
// the ledger); a surviving span shifts by the cumulative line delta of the
// hunks above it. Once a span outlives the durability window it is harvested as
// DURABLE (emitted once, then dropped so it can never re-emit).
//
// SEEDING IS FIRST-TOUCH-ONLY (deliberate, honest-by-construction): a path's AI
// ranges are seeded only the first time the path enters the ledger. The
// AI-paths ledger is path-scoped with a 7-day TTL, so it cannot tell "the AI
// rewrote its own line" from "a human rewrote an AI line" on a LATER commit to
// the same file. Re-seeding on every commit would therefore re-attribute human
// rewrites as fresh AI — inflating AI% exactly where the privacy/honesty rules
// forbid it (unknown is NEVER promoted to AI). So later commits to a tracked
// path only remap/churn what is already there. The cost is a conservative
// UNDERCOUNT (AI appends in a follow-up commit are missed), never inflation.

// durabilityWindowMs is how long an AI line must survive untouched to count as
// durable. A var (not const) so tests can drive time via the injected nowMs;
// the 30-day product window is the default.
var durabilityWindowMs int64 = 30 * 24 * 60 * 60 * 1000

const durabilityLedgerVersion = 1

// durHunkRe captures BOTH sides of a unified-diff hunk header
// `@@ -oldStart,oldLen +newStart,newLen @@`: group 1 oldStart, 2 oldLen
// (optional → 1), 3 newStart, 4 newLen (optional → 1). oldLen 0 = pure
// insertion; newLen 0 = pure deletion.
var durHunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// diffHunk is one hunk's line geometry — old side and new side. Ints only.
type diffHunk struct {
	OldStart, OldLen, NewStart, NewLen int
}

// durTrackedRange is a contiguous span of AI-authored lines currently tracked
// on a path, in that path's CURRENT (new-side) line space, stamped with the
// lineage it belongs to and when it was born. Content-free.
type durTrackedRange struct {
	Start     int    `json:"start"`
	End       int    `json:"end"`
	LineageID string `json:"lineageId"`
	BornTsMs  int64  `json:"bornTsMs"`
}

// durabilityLedger is the on-disk living-AI-line state: rootKey → path → spans,
// plus the per-root default-branch cursor (rootKey → last-processed default-tip
// SHA). Cursor and ranges live in ONE file under ONE lock so a poll advances
// both atomically.
//
// ⚠️ DO NOT BUMP durabilityLedgerVersion TO ADD A FIELD. A version mismatch makes
// readDurabilityLedgerUnlocked discard the whole file, which would wipe every
// tracked AI span on the fleet the moment the new build rolls out — every
// in-flight lineage loses its birth timestamp and restarts its 30-day clock, i.e.
// exactly the "durability is always 0%" failure this change exists to fix,
// inflicted deliberately. New fields must be ADDITIVE: an older ledger reads them as
// nil/zero, which every reader below already tolerates.
type durabilityLedger struct {
	V       int                                     `json:"v"`
	Roots   map[string]map[string][]durTrackedRange `json:"roots"`
	Cursors map[string]string                       `json:"cursors"`
	// Inventoried is rootKey → the last time a living inventory was emitted for
	// that root (ms). Absent/zero means "never" and emits on the next poll, so a
	// fresh install reports survival immediately rather than a day later.
	Inventoried map[string]int64 `json:"inventoried,omitempty"`
}

func durabilityLedgerPath() string {
	return filepath.Join(state.StateDir(), "durability.json")
}

// readDurabilityLedgerUnlocked reads the ledger WITHOUT taking the buffer lock —
// the caller must already hold it. A missing or version-mismatched file yields
// an empty ledger (never an error).
func readDurabilityLedgerUnlocked() durabilityLedger {
	led := durabilityLedger{V: durabilityLedgerVersion, Roots: map[string]map[string][]durTrackedRange{}}
	data, err := os.ReadFile(durabilityLedgerPath())
	if err != nil {
		return led
	}
	var onDisk durabilityLedger
	if json.Unmarshal(data, &onDisk) == nil && onDisk.V == durabilityLedgerVersion && onDisk.Roots != nil {
		led = onDisk
	}
	return led
}

// writeDurabilityLedgerUnlocked writes the ledger atomically (tmp + rename)
// WITHOUT taking the buffer lock — the caller must already hold it. Best-effort:
// I/O failure never blocks the caller.
func writeDurabilityLedgerUnlocked(led durabilityLedger) {
	led.V = durabilityLedgerVersion
	data, err := json.Marshal(led)
	if err != nil {
		return
	}
	tmp := durabilityLedgerPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, durabilityLedgerPath())
}

// loadDurabilityLedger reads the ledger under the buffer lock. For read-only
// callers; a mutating caller MUST use mutateDurabilityLedger so its
// read-modify-write is a single locked section.
func loadDurabilityLedger() durabilityLedger {
	var led durabilityLedger
	_ = sign.WithBufferLock(durabilityLedgerPath()+".lock", func() error {
		led = readDurabilityLedgerUnlocked()
		return nil
	})
	return led
}

// mutateDurabilityLedger runs load -> fn -> save as ONE locked read-modify-write.
// Separately locking the load and the save (as load+save above do) is not atomic
// across processes: with several CLI sessions sharing the state dir, a range
// update written between another writer's load and save is silently lost while
// its cursor still advances past the commit — permanently omitting that AI range.
// Every mutating durability path funnels through here so the whole RMW is atomic.
func mutateDurabilityLedger(fn func(led *durabilityLedger)) {
	_ = sign.WithBufferLock(durabilityLedgerPath()+".lock", func() error {
		led := readDurabilityLedgerUnlocked()
		fn(&led)
		writeDurabilityLedgerUnlocked(led)
		return nil
	})
}

// atoiDef parses s, returning def for an empty/invalid string. Used for the
// optional hunk length groups (missing → single line).
func atoiDef(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// parseDiffOldPath reduces a `--- ` header's target to a repo-relative POSIX
// path (git prefixes the old side with `a/`; `/dev/null` — a new file — yields
// ""). Mirrors parseDiffNewPath for the new side.
func parseDiffOldPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, "a/")
}

// parseUnifiedDiffHunks extracts every hunk's full geometry (old AND new side)
// per changed file. It reads ONLY the `diff --git` anchor, the `--- `/`+++ `
// file headers (before the file's first `@@`, so an added/removed body line
// that begins `+++ `/`--- ` can never be mistaken for a header), and the `@@`
// hunk headers — every +/- body line is ignored, so no content is retained.
// The path is the new side (`+++ b/…`), falling back to the old side for a
// whole-file deletion (`+++ /dev/null`) so a deleted file's churn still keys to
// the tracked path.
func parseUnifiedDiffHunks(diff string) map[string][]diffHunk {
	out := map[string][]diffHunk{}
	oldPath, newPath := "", ""
	inBody := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			oldPath, newPath, inBody = "", "", false
		case !inBody && strings.HasPrefix(line, "--- "):
			oldPath = parseDiffOldPath(line[len("--- "):])
		case !inBody && strings.HasPrefix(line, "+++ "):
			newPath = parseDiffNewPath(line[len("+++ "):])
		case strings.HasPrefix(line, "@@ "):
			inBody = true
			path := newPath
			if path == "" {
				path = oldPath
			}
			if path == "" {
				continue
			}
			m := durHunkRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			out[path] = append(out[path], diffHunk{
				OldStart: atoiDef(m[1], 0),
				OldLen:   atoiDef(m[2], 1),
				NewStart: atoiDef(m[3], 0),
				NewLen:   atoiDef(m[4], 1),
			})
		}
	}
	return out
}

// gitCommitRawDiff returns the raw unified-diff text for one commit — the SAME
// single spawn as gitCommitDiffRanges, kept as text so durability can parse
// both sides of each hunk. Best-effort: an error yields ok=false.
//
// `-m --first-parent` makes a merge commit diff against its FIRST parent (the
// previous default-branch tip) instead of emitting a combined `@@@` diff that
// our `@@` hunk parser cannot read. Without it, a merge that lands AI lines on
// the default branch produces no parseable hunks, so those lines are neither
// seeded nor churned — a real miss for merge-based (non-squash) flows. For a
// single-parent commit the flags are a verified no-op (byte-identical output).
func gitCommitRawDiff(root, sha string) (string, bool) {
	// #nosec G204 -- constant argv; root is a discovered workspace dir and sha comes from git rev-list output, not user input. Read-only.
	out, err := exec.Command("git", "-C", root,
		"-c", "core.quotePath=false",
		"show", "--root", "--no-color", "--unified=0", "--format=",
		"-m", "--first-parent", sha).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// churnedByHunk reports whether any hunk rewrites/deletes old-side line oldLine
// (i.e. oldLine falls inside a hunk's replaced old span). Insertions (OldLen 0)
// replace nothing and never churn.
func churnedByHunk(oldLine int, hunks []diffHunk) bool {
	for _, h := range hunks {
		if h.OldLen > 0 && oldLine >= h.OldStart && oldLine < h.OldStart+h.OldLen {
			return true
		}
	}
	return false
}

// shiftFor returns the net line delta applied to a SURVIVING old-side line by
// all hunks strictly above it. A hunk touches lines at/after
// oldStart+max(oldLen,1); anything at or beyond that is pushed by newLen-oldLen.
func shiftFor(oldLine int, hunks []diffHunk) int {
	shift := 0
	for _, h := range hunks {
		span := h.OldLen
		if span < 1 {
			span = 1
		}
		if oldLine >= h.OldStart+span {
			shift += h.NewLen - h.OldLen
		}
	}
	return shift
}

// expandRanges flattens spans into a per-line lineage/born map.
func expandRanges(ranges []durTrackedRange) map[int]durTrackedRange {
	m := map[int]durTrackedRange{}
	for _, r := range ranges {
		for ln := r.Start; ln <= r.End; ln++ {
			m[ln] = durTrackedRange{LineageID: r.LineageID, BornTsMs: r.BornTsMs}
		}
	}
	return m
}

// coalesceLines groups a per-line map back into contiguous spans that share a
// lineage and birth time, keyed by line number (new-side for survivors,
// old-side for churn). Sorted, so output is stable and reviewable.
func coalesceLines(m map[int]durTrackedRange) []durTrackedRange {
	if len(m) == 0 {
		return nil
	}
	lines := make([]int, 0, len(m))
	for ln := range m {
		lines = append(lines, ln)
	}
	sort.Ints(lines)
	var out []durTrackedRange
	for i, ln := range lines {
		meta := m[ln]
		if i > 0 && ln == lines[i-1]+1 {
			prev := &out[len(out)-1]
			if prev.LineageID == meta.LineageID && prev.BornTsMs == meta.BornTsMs {
				prev.End = ln
				continue
			}
		}
		out = append(out, durTrackedRange{Start: ln, End: ln, LineageID: meta.LineageID, BornTsMs: meta.BornTsMs})
	}
	return out
}

// remapTrackedRanges applies one commit's hunks to a path's tracked spans,
// returning the survivors (in the new-side line space) and the churned spans
// (in the old-side line space, for the churn verdict). Pure interval math — no
// git, no per-line spawn.
func remapTrackedRanges(ranges []durTrackedRange, hunks []diffHunk) (survivors, churned []durTrackedRange) {
	old := expandRanges(ranges)
	surv := map[int]durTrackedRange{}
	chn := map[int]durTrackedRange{}
	for oldLine, meta := range old {
		if churnedByHunk(oldLine, hunks) {
			chn[oldLine] = meta
			continue
		}
		surv[oldLine+shiftFor(oldLine, hunks)] = meta
	}
	return coalesceLines(surv), coalesceLines(chn)
}

// newSideAiRanges derives a path's new-side changed spans (for first-touch
// seeding) straight from its hunks — reusing the already-parsed diff instead of
// re-spawning git. Pure deletions (NewLen 0) contribute nothing.
func newSideAiRanges(hunks []diffHunk) []durTrackedRange {
	var out []durTrackedRange
	for _, h := range hunks {
		if h.NewLen <= 0 {
			continue
		}
		out = append(out, durTrackedRange{Start: h.NewStart, End: h.NewStart + h.NewLen - 1})
	}
	return out
}

// durLineageID is the stable identity for a seeded AI span: the seeding commit
// + path. It survives shifts/transfers so the backend can follow a line across
// history without ever seeing its content.
func durLineageID(sha, path string) string {
	return sha + ":" + path
}

// pollDurabilityCommit folds one new default-branch commit into the ledger:
// (1) remap every tracked path this commit touched — churn what it rewrites,
// shift what survives; (2) FIRST-TOUCH seed the AI-authored paths this commit
// introduces (see file header for why re-seeding is unsafe). Returns (and
// emits) a churn verdict per path with churned spans. One git spawn total.
func pollDurabilityCommit(root, rootKey string, session Session, sha string, nowMs int64) []event.Event {
	diff, ok := gitCommitRawDiff(root, sha)
	if !ok {
		// Inconclusive (git failed): do NOT advance the cursor, so this commit is
		// retried next poll rather than silently skipped.
		return nil
	}
	hunks := parseUnifiedDiffHunks(diff)
	diffNewLines := parseUnifiedDiffNewLines(diff)
	// The ai-paths ledger is anchored to the workspace (session.TaskRoot), not this
	// polled repo — in the daemon a discovered sub-repo's paths are stored
	// HOME-relative under the HOME key. Read/look up through the scope so seeding
	// sees the AI evidence; when root == taskRoot the scope is the identity. (The
	// per-root rootKey still keys fingerprints and the durability cursor below —
	// those are git-watch's own on-device state, correctly per-repo.)
	scope := resolveLedgerScope(root, session.TaskRoot)
	aiPaths := readAiTouchedPaths(scope.aiKey)

	// Fingerprint lookups (a separate locked file) are resolved BEFORE taking the
	// ledger lock, so the ledger's read-modify-write never nests another lock.
	fpsByPath := map[string]map[string]string{}
	for path := range hunks {
		if fps := fingerprintsForPath(rootKey, path, nowMs); fps != nil {
			fpsByPath[path] = fps
		}
	}

	var verdicts []event.Event
	mutateDurabilityLedger(func(led *durabilityLedger) {
		// Advance the cursor to this commit in the SAME transaction as its range
		// changes. A separate advance could crash in between, leaving the ranges
		// applied but the cursor behind — reprocessing would then remap the commit
		// twice (remapTrackedRanges is not idempotent). A zero-hunk commit still
		// advances: it is genuinely behind us and changed no tracked ranges.
		if led.Cursors == nil {
			led.Cursors = map[string]string{}
		}
		led.Cursors[rootKey] = sha
		files := led.Roots[rootKey]
		if files == nil {
			files = map[string][]durTrackedRange{}
		}
		for path, hs := range hunks {
			if existing := files[path]; len(existing) > 0 {
				// Already tracked: remap only — never re-seed (header rationale).
				surv, churned := remapTrackedRanges(existing, hs)
				if len(surv) > 0 {
					files[path] = surv
				} else {
					delete(files, path)
				}
				if len(churned) > 0 {
					verdicts = append(verdicts, buildDurabilityVerdict(session, root, sha, path, nil, churned, nil, nowMs))
				}
				continue
			}
			// First touch. Prefer fingerprint transfer (precise, and the ONLY thing
			// that survives a squash-merge — it carries lineage across the new SHA).
			// Fall back to path-level seeding only when there is NO fingerprint
			// evidence for this path (e.g. a cold ledger), preserving the simpler
			// same-branch behavior.
			var seeded []durTrackedRange
			if fps := fpsByPath[path]; fps != nil {
				for _, r := range matchedAiRuns(diffNewLines[path], fps, durabilityMinTransferRun) {
					r.BornTsMs = nowMs
					seeded = append(seeded, r)
				}
			} else if _, isAI := aiPaths[scope.ledgerPath(path)]; isAI {
				lineage := durLineageID(sha, path)
				for _, r := range newSideAiRanges(hs) {
					r.LineageID = lineage
					r.BornTsMs = nowMs
					seeded = append(seeded, r)
				}
			}
			if len(seeded) > 0 {
				files[path] = seeded
			}
		}

		if len(files) > 0 {
			if led.Roots == nil {
				led.Roots = map[string]map[string][]durTrackedRange{}
			}
			led.Roots[rootKey] = files
		} else {
			delete(led.Roots, rootKey)
		}
	})

	for i := range verdicts {
		emitDurabilityVerdict(verdicts[i])
	}
	return verdicts
}

// harvestDurable emits a durable verdict for every tracked span that has
// outlived the durability window, then DROPS those spans so a durable range is
// reported exactly once. Returns (and emits) one verdict per path with matured
// spans. No git spawn on the hot path — pure age check over the ledger.
func harvestDurable(session Session, root, rootKey string, nowMs int64) []event.Event {
	// Matured spans dropped from the ledger under the lock; the verdict (which
	// needs a git HEAD read) is built afterwards so no git spawn runs while the
	// ledger lock is held, and only when something actually matured.
	type harvested struct {
		path    string
		durable []durTrackedRange
	}
	var matured []harvested
	mutateDurabilityLedger(func(led *durabilityLedger) {
		files := led.Roots[rootKey]
		if len(files) == 0 {
			return
		}
		for path, ranges := range files {
			var durable, remaining []durTrackedRange
			for _, r := range ranges {
				if nowMs-r.BornTsMs >= durabilityWindowMs {
					durable = append(durable, r)
				} else {
					remaining = append(remaining, r)
				}
			}
			if len(durable) == 0 {
				continue
			}
			matured = append(matured, harvested{path: path, durable: durable})
			if len(remaining) > 0 {
				files[path] = remaining
			} else {
				delete(files, path)
			}
		}
		if len(files) > 0 {
			led.Roots[rootKey] = files
		} else {
			delete(led.Roots, rootKey)
		}
	})
	if len(matured) == 0 {
		return nil
	}

	measureSha, _ := gitHead(root)
	var verdicts []event.Event
	for _, h := range matured {
		verdicts = append(verdicts, buildDurabilityVerdict(session, root, measureSha, h.path, h.durable, nil, nil, nowMs))
	}
	for i := range verdicts {
		emitDurabilityVerdict(verdicts[i])
	}
	return verdicts
}

// durabilityInventoryIntervalMs is how often a root re-reports its living spans.
// A var (not const) so tests can drive it. Daily: the payload is a few integers
// per tracked span and ageDays only changes at day granularity anyway, so a
// tighter cadence would multiply event volume for no extra signal.
var durabilityInventoryIntervalMs int64 = 24 * 60 * 60 * 1000

// inventoryLiving emits, at most once per root per day, the AI spans still
// tracked and NOT yet decided — each stamped with its current ageDays. It does
// NOT mutate the spans: they stay in the ledger and are still churned or
// harvested normally.
//
// WHY THIS EXISTS. Durable verdicts are terminal and gated on surviving the full
// 30-day window, while churn is emitted the moment a commit rewrites a line. So
// before an install is 30 days old the ONLY verdict that can exist is churn, and
// every org reads "0% durable / 100% churned" — a statement about how long we
// have been watching, dressed up as a statement about the AI. Worse, BornTsMs is
// stamped when the ledger first SAW a span, not when it was authored, so the
// 30-day clock restarts at install for the whole repo. This inventory is what
// makes survival measurable in the meantime: the backend can compute survival at
// any horizon up to the oldest age observed.
//
// The tempting alternative — shrinking durabilityWindowMs so spans harvest early
// — is a TRAP: harvestDurable DROPS a span when it emits, so an early harvest
// permanently forfeits that lineage's real 30-day verdict. Reporting alongside
// costs nothing and destroys nothing.
func inventoryLiving(session Session, root, rootKey string, nowMs int64) []event.Event {
	type snapshot struct {
		path   string
		living []durTrackedRange
	}
	var due []snapshot
	mutateDurabilityLedger(func(led *durabilityLedger) {
		if nowMs-led.Inventoried[rootKey] < durabilityInventoryIntervalMs {
			return // throttled; a zero/missing entry means "never" and always fires
		}
		files := led.Roots[rootKey]
		if len(files) == 0 {
			return // nothing tracked — don't burn the throttle on an empty root
		}
		for path, ranges := range files {
			if len(ranges) == 0 {
				continue
			}
			// Copy: the slice stays owned by the ledger, and the verdict is built
			// after the lock is released.
			cp := make([]durTrackedRange, len(ranges))
			copy(cp, ranges)
			due = append(due, snapshot{path: path, living: cp})
		}
		if len(due) == 0 {
			return
		}
		if led.Inventoried == nil {
			led.Inventoried = map[string]int64{}
		}
		led.Inventoried[rootKey] = nowMs
	})
	if len(due) == 0 {
		return nil
	}

	// gitHead is deliberately outside the ledger lock (matches harvestDurable):
	// no git spawn ever runs while the lock is held.
	measureSha, _ := gitHead(root)
	var verdicts []event.Event
	for _, s := range due {
		verdicts = append(verdicts, buildDurabilityVerdict(session, root, measureSha, s.path, nil, nil, s.living, nowMs))
	}
	for i := range verdicts {
		emitDurabilityVerdict(verdicts[i])
	}
	return verdicts
}

// durVerdictRange is one span in a verdict payload: line numbers + age + lineage.
// Scalars only — no content ever rides here.
type durVerdictRange struct {
	Start     int    `json:"start"`
	End       int    `json:"end"`
	AgeDays   int    `json:"ageDays"`
	LineageID string `json:"lineageId"`
}

// durabilityVerdictData is the CLOSED payload of a durability_verdict event.
//
// The three range arrays are mutually exclusive per event: churn is emitted at
// commit time, durable at harvest, living on the daily inventory. LivingRanges is
// the PROVISIONAL one — those spans are still tracked and can still churn or
// mature; the other two are terminal.
type durabilityVerdictData struct {
	CommitSha     string            `json:"commitSha"`
	WorkspaceKey  string            `json:"workspaceKey"`
	Path          string            `json:"path"`
	DurableRanges []durVerdictRange `json:"durableRanges,omitempty"`
	ChurnedRanges []durVerdictRange `json:"churnedRanges,omitempty"`
	LivingRanges  []durVerdictRange `json:"livingRanges,omitempty"`
	MeasuredTsMs  int64             `json:"measuredTsMs"`
}

// toVerdictRanges converts tracked spans into content-free verdict ranges,
// stamping each with its age in days at nowMs. Shared by the durability and
// rework verdict builders (both age a durTrackedRange the same way).
func toVerdictRanges(rs []durTrackedRange, nowMs int64) []durVerdictRange {
	var out []durVerdictRange
	for _, r := range rs {
		out = append(out, durVerdictRange{
			Start:     r.Start,
			End:       r.End,
			AgeDays:   int((nowMs - r.BornTsMs) / (24 * 60 * 60 * 1000)),
			LineageID: r.LineageID,
		})
	}
	return out
}

// buildDurabilityVerdict assembles a durability_verdict for one path. Data goes
// through eventDataMap (JSON round-trip) so nested arrays land as
// []interface{} of map — the only shape the redaction projector's element
// allowlist can walk (assigning the struct straight to Data ships {}).
func buildDurabilityVerdict(session Session, root, sha, path string, durable, churned, living []durTrackedRange, nowMs int64) event.Event {
	e := event.NewEvent("durability_verdict", session.DeviceID)
	e.Source = presenceSource
	e.DeviceID = session.DeviceID
	e.Actor = event.SystemActor()
	e.Data = eventDataMap(durabilityVerdictData{
		CommitSha:     sha,
		WorkspaceKey:  workspaceKey(root),
		Path:          path,
		DurableRanges: toVerdictRanges(durable, nowMs),
		ChurnedRanges: toVerdictRanges(churned, nowMs),
		LivingRanges:  toVerdictRanges(living, nowMs),
		MeasuredTsMs:  nowMs,
	})
	return e
}

// emitDurabilityVerdict funnels a verdict through the SAME sign/redact/queue
// path as every captured event, reusing the shared outbox drain singleton (it
// never starts its own). Best-effort — matches emitCommitAttribution.
func emitDurabilityVerdict(ev event.Event) {
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		state.HookDebugf("durability verdict buffer error: %v", err)
	}
	if err := outbox.Append(ev); err != nil {
		state.HookDebugf("durability verdict queue error: %v", err)
	}
}
