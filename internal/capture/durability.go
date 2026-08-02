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
//
// "FIRST TOUCH" IS A PROPERTY OF THE ROOT, NOT OF THE LEDGER'S CURRENT CONTENTS.
// A path LEAVES the ledger by three routes — every span churned, every span
// harvested as durable, or the file renamed away — and each of those deletions
// used to re-arm the branch above, so the next purely-human commit to that path
// was seeded as fresh AI. That is precisely the promotion the paragraph above
// exists to forbid, reached through the back door. Every deletion therefore
// leaves a TOMBSTONE (durabilityLedger.Seeded) recording that this root has
// already first-touched this path, and the path-level fallback refuses to fire
// while one stands. The tombstone gates ONLY that fallback: fingerprint transfer
// is line-precise and is the sole carrier of lineage through a squash-merge, so
// blocking it would trade a fabrication for a silent loss.
//
// RENAMES ARE CARRIED, NOT STRANDED. A rename emits no `@@` hunks under the
// tracked path, so its spans were neither remapped nor churned: they sat at a
// path that no longer existed and matured into a `durable` verdict — fabricated
// survival, the worst error this code can make. pollDurabilityCommit now moves
// the spans to the new path with BornTsMs and lineage intact, before the hunk
// remap, so lineage and age both survive the move.

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
	// Seeded is rootKey → path → ms: the SEED TOMBSTONES. An entry means "this
	// root has already first-touched this path", and blocks the path-level
	// seeding fallback so a path that left the ledger cannot be re-seeded from
	// stale path evidence (see the file header). Additive — an older ledger reads
	// it as nil and every reader below tolerates that.
	Seeded map[string]map[string]int64 `json:"seeded,omitempty"`
}

// durabilitySeedTombstoneTTLms is how long a seed tombstone stands after the
// last commit that would otherwise have re-seeded the path. A var so tests can
// drive it.
//
// The lifetime is a real tradeoff and neither extreme is right: a permanent set
// grows without bound (one repo-wide reformat churns every tracked path at once
// and would tombstone all of them forever), while too short a one re-opens the
// hole the tombstone exists to close.
//
// 30 days — the durability window — with the stamp REFRESHED on every blocked
// attempt, so the clock runs from the last touch rather than from the deletion.
// An actively-worked path therefore stays protected indefinitely and only a path
// nobody has committed to for a full window ages out. That is safe because a
// bogus re-seed needs LIVE path evidence, and the AI-paths ledger expires an
// entry 7 days after the AI last touched it (aiPathsTTL): by the time a
// tombstone lapses, the evidence that could misfire against it is long dead, and
// any evidence that IS live by then describes genuinely recent AI work — the
// same guess the fallback makes on any true first touch.
//
// Deliberately NOT keyed to aiPathsTTL's 7 days: that TTL is refreshed by AI
// activity, so a 7-day tombstone can lapse while the very evidence that would
// re-fire the seed is still alive.
var durabilitySeedTombstoneTTLms int64 = 30 * 24 * 60 * 60 * 1000

// tombstoneSeededPath records (or refreshes) the mark saying this root has
// already first-touched path. Called on EVERY route a path takes out of the
// ledger — full churn, durable harvest, rename-away — and on every blocked
// re-seed attempt.
func tombstoneSeededPath(led *durabilityLedger, rootKey, path string, nowMs int64) {
	if led.Seeded == nil {
		led.Seeded = map[string]map[string]int64{}
	}
	if led.Seeded[rootKey] == nil {
		led.Seeded[rootKey] = map[string]int64{}
	}
	led.Seeded[rootKey][path] = nowMs
}

// seededPathTombstoned reports whether a live tombstone blocks seeding path.
// A FUTURE stamp (host clock moved backwards) reads as blocked, which is the
// safe direction: the cost is a conservative undercount, never inflation.
func seededPathTombstoned(led *durabilityLedger, rootKey, path string, nowMs int64) bool {
	ts, ok := led.Seeded[rootKey][path]
	return ok && nowMs-ts < durabilitySeedTombstoneTTLms
}

// pruneSeedTombstones drops a root's expired tombstones, and the empty maps
// behind them, so the ledger cannot grow without bound. It costs one map walk
// over one root's marks.
//
// It runs once per processed commit AND once per poll from harvestDurable — the
// second call site is what makes the bound real. A root whose default branch
// stops advancing (repo abandoned, work moved elsewhere) processes no further
// commits, so a commit-only pruner would never walk its marks again and a
// repo-wide reformat's tombstones would sit in durability.json forever.
// harvestDurable runs every poll regardless of whether the tip moved, and prunes
// BEFORE its empty-root early return, which is exactly the state a fully churned
// root is left in.
func pruneSeedTombstones(led *durabilityLedger, rootKey string, nowMs int64) {
	marks := led.Seeded[rootKey]
	for path, ts := range marks {
		if nowMs-ts >= durabilitySeedTombstoneTTLms {
			delete(marks, path)
		}
	}
	if len(marks) == 0 {
		delete(led.Seeded, rootKey)
	}
	if len(led.Seeded) == 0 {
		led.Seeded = nil
	}
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

// unquoteDiffPath undoes git's C-style quoting. Git quotes a diff path whenever
// it contains a `"`, a backslash or a control character — even under
// `core.quotePath=false`, which only stops it escaping non-ASCII. Left quoted,
// the same file reads as two different ledger keys depending on which header it
// came from (`"b/a\"b.go"` from `+++`, `"a\"b.go"` from `rename from`), and a
// rename could never be matched to the span it moves. A string that is not
// quoted, or that fails to parse, is returned unchanged.
func unquoteDiffPath(s string) string {
	if len(s) < 2 || s[0] != '"' {
		return s
	}
	if out, err := strconv.Unquote(s); err == nil {
		return out
	}
	return s
}

// parseDiffOldPath reduces a `--- ` header's target to a repo-relative POSIX
// path (git prefixes the old side with `a/`; `/dev/null` — a new file — yields
// ""). Mirrors parseDiffNewPath for the new side.
func parseDiffOldPath(s string) string {
	s = unquoteDiffPath(strings.TrimSpace(s))
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

// parseUnifiedDiffRenames extracts oldPath → newPath for every rename in the
// diff, reading ONLY the `rename from` / `rename to` headers git already emits
// (rename detection is on by default since git 2.9). Nothing else is read, so
// no content is retained.
//
// It deliberately ignores `copy from` / `copy to`: a copy leaves the source file
// in place, so its tracked spans must stay exactly where they are. Migrating
// them would delete a live path's lineage to chase a duplicate.
//
// Headers are read only BEFORE a file's first `@@` — with the same inBody
// discipline parseUnifiedDiffHunks uses — so an added line whose text happens to
// begin `rename from ` can never be mistaken for a header.
func parseUnifiedDiffRenames(diff string) map[string]string {
	out := map[string]string{}
	from := ""
	inBody := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			from, inBody = "", false
		case strings.HasPrefix(line, "@@ "):
			inBody = true
		case !inBody && strings.HasPrefix(line, "rename from "):
			from = unquoteDiffPath(strings.TrimSpace(line[len("rename from "):]))
		case !inBody && strings.HasPrefix(line, "rename to "):
			to := unquoteDiffPath(strings.TrimSpace(line[len("rename to "):]))
			if from != "" && to != "" && from != to {
				out[from] = to
			}
			from = ""
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

// applyDurabilityRenames carries a commit's renamed paths across in the ledger,
// mutating files in place, BEFORE the hunk remap runs.
//
// Order matters and is the whole point. Git keys a rename's hunks under the NEW
// path while their old-side line numbers still address the OLD file, so a span
// moved here lands already in the correct line space for the ordinary
// remapTrackedRanges pass — a rename+edit churns exactly the edited lines and
// shifts the rest, with no rename-specific interval math. A pure rename has no
// hunks at all, which is what stranded the span before this existed.
//
// BornTsMs and LineageID ride along untouched: a rename is not a rewrite, so
// neither the 30-day clock nor the lineage may restart.
func applyDurabilityRenames(led *durabilityLedger, rootKey string, files map[string][]durTrackedRange, renames map[string]string, nowMs int64) {
	if len(renames) == 0 {
		return
	}
	type move struct {
		to    string
		spans []durTrackedRange
	}
	// Every source is READ (and removed) before any destination is written, so a
	// chain landing in one commit (a→b, b→c) cannot clobber itself on Go's random
	// map order.
	var moves []move
	for from, to := range renames {
		// The tombstone follows the file. Otherwise renaming a fully-churned path
		// re-arms first-touch seeding under its new name — the same hole, moved.
		if seededPathTombstoned(led, rootKey, from, nowMs) {
			tombstoneSeededPath(led, rootKey, to, nowMs)
		}
		spans := files[from]
		if len(spans) == 0 {
			continue
		}
		moves = append(moves, move{to: to, spans: spans})
		delete(files, from)
		tombstoneSeededPath(led, rootKey, from, nowMs)
	}
	for _, m := range moves {
		existing := files[m.to]
		if len(existing) == 0 {
			files[m.to] = m.spans
			continue
		}
		// Git emits modify+delete rather than a rename when the destination already
		// exists, so this is unreachable through real git output — it is here so an
		// unexpected shape degrades to a merge rather than to a dropped span, since
		// a dropped span is the stranded-lineage fabrication all over again. The
		// destination wins any overlapping line.
		merged := expandRanges(m.spans)
		for ln, meta := range expandRanges(existing) {
			merged[ln] = meta
		}
		files[m.to] = coalesceLines(merged)
	}
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
	renames := parseUnifiedDiffRenames(diff)
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
		pruneSeedTombstones(led, rootKey, nowMs)
		applyDurabilityRenames(led, rootKey, files, renames, nowMs)
		for path, hs := range hunks {
			if existing := files[path]; len(existing) > 0 {
				// Already tracked: remap only — never re-seed (header rationale).
				surv, churned := remapTrackedRanges(existing, hs)
				if len(surv) > 0 {
					files[path] = surv
				} else {
					// The path leaves the ledger. Tombstone it, or the first-touch
					// branch below re-arms and the next purely-human commit to this
					// path is seeded as fresh AI.
					delete(files, path)
					tombstoneSeededPath(led, rootKey, path, nowMs)
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
			//
			// The path-level fallback additionally refuses to fire while a SEED
			// TOMBSTONE stands: "first touch" is a property of the root, not of the
			// ledger's current contents, and a path that churned out / matured out /
			// was renamed away has already been seeded once. Without that check the
			// deletion re-arms this branch and a purely-human commit enters the
			// ledger as fresh AI. The tombstone does NOT gate fingerprint transfer
			// above — that evidence is line-precise and is the only thing carrying
			// lineage through a squash-merge.
			var seeded []durTrackedRange
			if fps := fpsByPath[path]; fps != nil {
				for _, r := range matchedAiRuns(diffNewLines[path], fps, durabilityMinTransferRun) {
					r.BornTsMs = nowMs
					seeded = append(seeded, r)
				}
			} else if _, isAI := aiPaths[scope.ledgerPath(path)]; isAI {
				if seededPathTombstoned(led, rootKey, path, nowMs) {
					// Refresh so the tombstone's clock runs from the last touch: an
					// actively-worked path stays protected, a quiet one ages out.
					tombstoneSeededPath(led, rootKey, path, nowMs)
					continue
				}
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
		// Before the early return, deliberately: an emptied root is precisely the
		// state whose tombstones nothing else would ever walk again.
		pruneSeedTombstones(led, rootKey, nowMs)
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
				// Second route out of the ledger, same hole: an emptied path re-arms
				// first-touch seeding, so the next purely-human commit to it would be
				// seeded as fresh AI. Tombstone it exactly as the churn path does.
				delete(files, path)
				tombstoneSeededPath(led, rootKey, path, nowMs)
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
		last := led.Inventoried[rootKey]
		// A zero/missing entry means "never inventoried" and always fires. A FUTURE
		// entry means the host clock moved BACKWARDS (an NTP correction, a laptop
		// resumed from suspend, a VM snapshot): nowMs-last goes negative, which is
		// trivially < the interval, so a naive check would suppress every inventory
		// until wall time caught back up — a multi-day hole in the survival series
		// caused by a clock, not by the code. Treat a future stamp as due; the emit
		// below overwrites it with a sane one.
		if last != 0 && nowMs >= last && nowMs-last < durabilityInventoryIntervalMs {
			return // throttled
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
	delivered := false
	for i := range verdicts {
		if err := emitDurabilityVerdict(verdicts[i]); err == nil {
			delivered = true
		}
	}
	// The throttle is stamped AFTER delivery, deliberately. Stamping it in the read
	// pass above would burn the daily slot even when the outbox was unwritable
	// (disk full, permissions, a half-migrated state dir) — the inventory would be
	// silently skipped for 24h and that day would be missing from survival forever.
	// Re-emitting instead is cheap and safe: living verdicts are PROVISIONAL, and
	// the backend folds a lineage's living entries by max measuredTsMs, so a
	// duplicate inventory of the same spans collapses to one.
	if !delivered {
		return verdicts
	}
	mutateDurabilityLedger(func(led *durabilityLedger) {
		if led.Inventoried == nil {
			led.Inventoried = map[string]int64{}
		}
		led.Inventoried[rootKey] = nowMs
	})
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
// never starts its own). Best-effort — matches emitCommitAttribution — but it
// RETURNS the outbox error so a caller that gates state on delivery can see it.
// The outbox is the leg that reaches the backend; the local buffer is the signed
// audit copy, so a buffer-only failure is not a lost verdict. Callers that don't
// care (churn at commit time, the durable harvest) discard the return.
func emitDurabilityVerdict(ev event.Event) error {
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		state.HookDebugf("durability verdict buffer error: %v", err)
	}
	if err := outbox.Append(ev); err != nil {
		state.HookDebugf("durability verdict queue error: %v", err)
		return err
	}
	return nil
}
