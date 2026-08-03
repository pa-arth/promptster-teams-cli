package capture

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// Regression tests for the two ways the durability ledger could FABRICATE data
// — invent AI attribution, or invent survival — as opposed to merely losing it.
// Both were reproduced against the shipped code before the fix; each test below
// failed then and passes now.
//
//  1. A path whose every tracked span churned was DELETED from the ledger, which
//     re-armed the first-touch seeding branch. The next purely-human commit to
//     that path was then seeded as fresh AI — the exact re-attribution the file
//     header says first-touch-only exists to prevent ("unknown is NEVER promoted
//     to AI").
//  2. A rename produced no hunks under the tracked path, so its spans were
//     neither remapped nor churned. They sat at a path that no longer existed
//     and matured into a `durable` verdict — the strongest positive signal this
//     product emits, manufactured for a dead path.
//
// The sibling PRE-MERGE rework ledger states the same first-touch discipline in
// its own header and reached (1) through the identical door, so its regression
// lives here too rather than beside the rework unit tests: the invariant is
// shared, and splitting them is how one side gets fixed and the other does not.

// ledgerRanges reads a path's tracked spans straight out of the on-disk ledger.
func ledgerRanges(t *testing.T, rootKey, path string) []durTrackedRange {
	t.Helper()
	return loadDurabilityLedger().Roots[rootKey][path]
}

// trackedLineCount totals the lines a path currently has tracked.
func trackedLineCount(t *testing.T, rootKey, path string) int {
	t.Helper()
	n := 0
	for _, r := range ledgerRanges(t, rootKey, path) {
		n += r.End - r.Start + 1
	}
	return n
}

// firstAgeDays pulls ageDays off the first element of a verdict range array.
func firstAgeDays(t *testing.T, data map[string]interface{}, field string) int {
	t.Helper()
	arr, ok := data[field].([]interface{})
	if !ok || len(arr) == 0 {
		t.Fatalf("no ranges in %s: %+v", field, data[field])
	}
	age, _ := arr[0].(map[string]interface{})["ageDays"].(float64)
	return int(age)
}

// ── Defect 1: human code re-attributed as fresh AI after full churn ──────────

// TestDurabilityFullChurnDoesNotReseedHumanCodeAsAi reproduces §B3. AI writes 3
// lines; a human rewrites all 3 (full churn, path leaves the ledger); the SAME
// human then appends 20 more lines with no AI involvement. Before the fix those
// 20 purely-human lines entered the ledger as fresh AI, because deleting the
// path re-armed first-touch seeding and the day-0 path evidence was still inside
// its 7-day TTL.
func TestDurabilityFullChurnDoesNotReseedHumanCodeAsAi(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	// Day 0: the AI writes app.go.
	recordAiTouchedPath("sess-b3", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)
	if got := trackedLineCount(t, key, "app.go"); got != 3 {
		t.Fatalf("seeded lines = %d, want 3", got)
	}

	// Day 1: a human rewrites every AI line. All three churn, and the path drops
	// out of the ledger.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites all of app.go")
	churn := pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+dayMs)
	if len(churn) == 0 {
		t.Fatal("a full rewrite must emit churn")
	}
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Fatalf("after full churn tracked = %d, want 0", got)
	}

	// Day 2: the same human appends 20 more lines. No AI touched this commit.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n"+strings.Repeat("human\n", 20))
	git("add", "-A")
	git("commit", "-m", "human appends 20 lines")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+2*dayMs)

	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Errorf("a purely HUMAN commit was seeded as AI: %d lines tracked, want 0 (ranges %+v)",
			got, ledgerRanges(t, key, "app.go"))
	}
}

// TestDurabilityHarvestedPathDoesNotReseedHumanCodeAsAi covers the SECOND door
// into the same hole: harvestDurable also deletes a path once its every span has
// matured. Left untombstoned, that re-arms first-touch exactly as a full churn
// does, and the next human commit to the path is seeded as AI.
func TestDurabilityHarvestedPathDoesNotReseedHumanCodeAsAi(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	recordAiTouchedPath("sess-harvest", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)

	// The spans mature and are dropped (a durable range is emitted exactly once).
	if v := harvestDurable(sess, ws, key, t0+31*dayMs); len(v) == 0 {
		t.Fatal("expected a durable harvest at 31d")
	}
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Fatalf("harvest must drop matured spans, %d still tracked", got)
	}

	// A purely human commit to the same path must not re-seed it.
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n"+strings.Repeat("human\n", 10))
	git("add", "-A")
	git("commit", "-m", "human appends after the harvest")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+32*dayMs)

	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Errorf("a harvested path re-seeded human code as AI: %d lines tracked, want 0 (ranges %+v)",
			got, ledgerRanges(t, key, "app.go"))
	}
}

// TestDurabilitySeedTombstoneBlocksInferenceButNotAFreshAiWrite pins BOTH
// directions of the tombstone's boundary, which is the whole design question the
// fix turns on and is the rule the sibling rework ledger already states.
//
// Too permissive fabricates: a human commit to a path the AI-paths ledger still
// carries is not a write, and re-seeding on that presence relabels the human's
// lines AI. Too restrictive silences the agent loop: the file the AI keeps
// rewriting is exactly the one that churns out, and blocking it outright made
// every iteration after the first invisible for good. Only forward motion of the
// per-path write stamp separates the two, and it must be paid for once — the
// same write cannot re-authorize a second re-entry.
func TestDurabilitySeedTombstoneBlocksInferenceButNotAFreshAiWrite(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	recordAiTouchedPath("sess-ev", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)

	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites all of app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+dayMs)
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Fatalf("setup: the full churn must empty the path, got %d lines", got)
	}

	// A human commit with the path still sitting in the 7-day presence cache. No
	// agent has written it since the seed we already spent, so nothing re-enters —
	// however many such commits land, and however long the path stays present.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\nh4\n")
	git("add", "-A")
	git("commit", "-m", "human appends")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+2*dayMs)
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Fatalf("path presence re-seeded human code as AI: %d lines tracked", got)
	}

	// The agent writes the file again. That is direct per-path evidence, strictly
	// newer than the stamp already spent, so the path re-enters — with a FRESH
	// lineage and a FRESH birth stamp, because this is new work and not a
	// continuation of the span that churned out.
	recordAiTouchedPath("sess-ev", key, "app.go")
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\nh4\nb1\nb2\n")
	git("add", "-A")
	git("commit", "-m", "ai writes app.go again")
	sha := gitOut("rev-parse", "HEAD")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+3*dayMs)
	spans := ledgerRanges(t, key, "app.go")
	if len(spans) == 0 {
		t.Fatal("a fresh agent write was silenced: the path must re-enter the ledger")
	}
	if got, want := spans[0].LineageID, durLineageID(sha, "app.go"); got != want {
		t.Errorf("lineage = %q, want a FRESH lineage %q", got, want)
	}
	if got := spans[0].BornTsMs; got != t0+3*dayMs {
		t.Errorf("BornTsMs = %d, want a FRESH stamp %d", got, t0+3*dayMs)
	}

	// That write is now spent. Churn the re-entered spans back out and commit
	// human lines again: the SAME stamp must not buy a second re-entry.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\nh4\nc1\nc2\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites the new AI lines")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+4*dayMs)
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\nh4\nc1\nc2\nc3\n")
	git("add", "-A")
	git("commit", "-m", "human appends again")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+5*dayMs)
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Errorf("a spent write authorized a second re-entry: %d lines tracked, want 0", got)
	}
}

// TestDurabilityHarvestedPathStaysVisibleToLaterAiWork is the concrete sequence
// the tombstone's first shape got wrong, and it is the common one rather than an
// edge: the AI creates a file, it seeds, thirty days later harvestDurable emits
// `durable` and drops the path, and the AI goes on working in that same file.
//
// Every rail is gone by then except the path fallback — the fingerprints from the
// original commit expired at 14 days, well short of the 30-day maturation — and
// that fallback was blocked, while refreshing its own mark on every blocked
// attempt. Live AI work was therefore the thing keeping the file buried, so the
// ledger reported nothing further from a file the agent never stopped writing.
func TestDurabilityHarvestedPathStaysVisibleToLaterAiWork(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	recordAiTouchedPath("sess-mature", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)

	if v := harvestDurable(sess, ws, key, t0+31*dayMs); len(v) == 0 {
		t.Fatal("setup: expected a durable harvest at 31d")
	}
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Fatalf("setup: harvest must drop matured spans, %d still tracked", got)
	}

	// The agent writes the same file again, a month into its life.
	recordAiTouchedPath("sess-mature", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\nb1\nb2\n")
	git("add", "-A")
	git("commit", "-m", "ai extends app.go a month later")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+32*dayMs)

	if got := trackedLineCount(t, key, "app.go"); got == 0 {
		t.Errorf("AI work on a harvested path is invisible: 0 lines tracked, want the new lines (ranges %+v)",
			ledgerRanges(t, key, "app.go"))
	}
}

// TestDurabilityTombstoneDoesNotBlockFingerprintTransfer guards the fix's blast
// radius. The tombstone gates ONLY the path-level fallback, which is the
// imprecise evidence the honesty invariant is about. Fingerprint transfer is
// line-precise (it matches content hashes), and it is the ONLY thing that
// carries lineage across a squash-merge — blocking it would trade a fabrication
// for a silent loss of every squashed lineage on a previously-churned path.
func TestDurabilityTombstoneDoesNotBlockFingerprintTransfer(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	// Seed app.go, then churn it entirely so the path is tombstoned.
	recordAiTouchedPath("sess-fp", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)
	writeCommitFile(t, ws, "app.go", "h1\nh2\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+dayMs)
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Fatalf("full churn should have emptied app.go, got %d", got)
	}

	// The attribution watcher captures fingerprints for AI lines written on a
	// feature branch, as it does on every likely_ai commit.
	branchDiff := strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -2,0 +3,2 @@",
		"+func A() {",
		"+}",
	}, "\n")
	recordAiFingerprints(key, "sha-branch", branchDiff,
		[]attrFile{{Path: "app.go", LineRanges: []attrLineRange{{Start: 3, End: 4, Attribution: attributionLikelyAI}}}}, t0+dayMs)

	// Those exact lines then land on the default branch under a new sha.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nfunc A() {\n}\n")
	git("add", "-A")
	git("commit", "-m", "squash: land the branch")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+2*dayMs)

	if got := trackedLineCount(t, key, "app.go"); got != 2 {
		t.Errorf("fingerprint transfer was blocked by the tombstone: %d lines tracked, want 2 (ranges %+v)",
			got, ledgerRanges(t, key, "app.go"))
	}
}

// ── Defect 2: a rename manufactures a phantom durable verdict ────────────────

// TestDurabilityPureRenameCarriesTheSpan reproduces §B4. A pure rename yields no
// `@@` hunks under the tracked path, so before the fix the span was neither
// remapped nor churned: it stayed stranded at the old path and, 31 days later,
// was emitted as `durable` for a path that had not existed for 28 days.
func TestDurabilityPureRenameCarriesTheSpan(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	recordAiTouchedPath("sess-b4", key, "old.go")
	writeCommitFile(t, ws, "old.go", "l1\nl2\nl3\nl4\nl5\nl6\n")
	git("add", "-A")
	git("commit", "-m", "ai adds old.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)
	if got := trackedLineCount(t, key, "old.go"); got != 6 {
		t.Fatalf("seeded = %d lines, want 6", got)
	}

	// Day 3: a pure rename, no content change.
	git("mv", "old.go", "new.go")
	git("commit", "-m", "rename old.go -> new.go")
	if churn := pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+3*dayMs); len(churn) != 0 {
		t.Errorf("a pure rename churns nothing, got %+v", churn)
	}

	// The span must have MOVED, keeping its birth stamp and lineage.
	if got := trackedLineCount(t, key, "old.go"); got != 0 {
		t.Errorf("%d lines still tracked at the dead path old.go, want 0", got)
	}
	moved := ledgerRanges(t, key, "new.go")
	if len(moved) != 1 || moved[0].Start != 1 || moved[0].End != 6 {
		t.Fatalf("new.go ranges = %+v, want a single 1..6 span", moved)
	}
	if moved[0].BornTsMs != t0 {
		t.Errorf("BornTsMs = %d, want %d — a rename must not restart the clock", moved[0].BornTsMs, t0)
	}

	// Day 31: durable is emitted for the path that EXISTS, never for the dead one.
	v := harvestDurable(sess, ws, key, t0+31*dayMs)
	for _, ev := range v {
		if data, _ := ev.Data.(map[string]interface{}); data["path"] == "old.go" {
			t.Errorf("phantom durable verdict for the dead path old.go: %+v", data)
		}
	}
	data := durVerdictFor(t, v, "new.go")
	if durable := rangeSet(t, data, "durableRanges"); !durable["1..6"] {
		t.Errorf("durableRanges = %+v, want 1..6 at new.go", durable)
	}
	if age := firstAgeDays(t, data, "durableRanges"); age != 31 {
		t.Errorf("ageDays = %d, want 31 — the rename must carry the original birth stamp", age)
	}
}

// TestDurabilityRenameWithEditRemapsAndChurns: a rename and an edit in the SAME
// commit. Git keys the hunks under the NEW path while their old-side line
// numbers still address the OLD file, so the span must be carried across first
// and only then remapped — otherwise the edited line never churns and the
// survivors are stranded.
func TestDurabilityRenameWithEditRemapsAndChurns(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	recordAiTouchedPath("sess-re", key, "old.go")
	writeCommitFile(t, ws, "old.go", "l1\nl2\nl3\nl4\nl5\nl6\n")
	git("add", "-A")
	git("commit", "-m", "ai adds old.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)

	// Rename AND rewrite line 2 in one commit.
	git("mv", "old.go", "new.go")
	writeCommitFile(t, ws, "new.go", "l1\nCHANGED\nl3\nl4\nl5\nl6\n")
	git("add", "-A")
	git("commit", "-m", "rename + edit line 2")
	churn := pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+3*dayMs)

	cdata := durVerdictFor(t, churn, "new.go")
	if got := rangeSet(t, cdata, "churnedRanges"); !got["2..2"] {
		t.Errorf("churnedRanges = %+v, want line 2 churned under the new path", got)
	}
	if got := trackedLineCount(t, key, "old.go"); got != 0 {
		t.Errorf("%d lines still tracked at the dead path old.go, want 0", got)
	}
	if got := trackedLineCount(t, key, "new.go"); got != 5 {
		t.Fatalf("new.go tracked = %d lines, want 5 (6 seeded, 1 churned): %+v",
			got, ledgerRanges(t, key, "new.go"))
	}

	v := harvestDurable(sess, ws, key, t0+31*dayMs)
	data := durVerdictFor(t, v, "new.go")
	durable := rangeSet(t, data, "durableRanges")
	if !durable["1..1"] || !durable["3..6"] {
		t.Errorf("durableRanges = %+v, want 1..1 and 3..6 at new.go", durable)
	}
	if age := firstAgeDays(t, data, "durableRanges"); age != 31 {
		t.Errorf("ageDays = %d, want 31 — the birth stamp must survive rename+edit", age)
	}
}

// TestDurabilityRenameOfPartiallyChurnedPath: the span set that crosses a rename
// is the one left AFTER earlier churn, with each surviving run keeping its own
// birth stamp. A rename must move exactly what is still tracked — no more (the
// churned lines must not come back) and no less.
func TestDurabilityRenameOfPartiallyChurnedPath(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	recordAiTouchedPath("sess-rp", key, "old.go")
	writeCommitFile(t, ws, "old.go", "l1\nl2\nl3\nl4\nl5\nl6\n")
	git("add", "-A")
	git("commit", "-m", "ai adds old.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)

	// Day 1: a human rewrites line 3 — that line churns, 1..2 and 4..6 survive.
	writeCommitFile(t, ws, "old.go", "l1\nl2\nHUMAN\nl4\nl5\nl6\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites line 3")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+dayMs)
	if got := trackedLineCount(t, key, "old.go"); got != 5 {
		t.Fatalf("after partial churn tracked = %d, want 5", got)
	}

	// Day 3: pure rename.
	git("mv", "old.go", "new.go")
	git("commit", "-m", "rename old.go -> new.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+3*dayMs)

	if got := trackedLineCount(t, key, "old.go"); got != 0 {
		t.Errorf("%d lines still tracked at the dead path old.go, want 0", got)
	}
	if got := trackedLineCount(t, key, "new.go"); got != 5 {
		t.Fatalf("new.go tracked = %d lines, want the 5 survivors: %+v",
			got, ledgerRanges(t, key, "new.go"))
	}
	for _, r := range ledgerRanges(t, key, "new.go") {
		if r.BornTsMs != t0 {
			t.Errorf("span %+v lost its birth stamp across the rename", r)
		}
	}

	v := harvestDurable(sess, ws, key, t0+31*dayMs)
	data := durVerdictFor(t, v, "new.go")
	durable := rangeSet(t, data, "durableRanges")
	if !durable["1..2"] || !durable["4..6"] {
		t.Errorf("durableRanges = %+v, want 1..2 and 4..6 (line 3 stayed churned)", durable)
	}
}

// TestParseUnifiedDiffRenames pins the rename-header parser against the exact
// shapes `git show --unified=0` emits: a pure rename (no ---/+++ , no @@), a
// rename carrying an edit, and a C-quoted path.
func TestParseUnifiedDiffRenames(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/old.go b/new.go",
		"similarity index 100%",
		"rename from old.go",
		"rename to new.go",
		"diff --git a/edited.go b/moved.go",
		"similarity index 65%",
		"rename from edited.go",
		"rename to moved.go",
		"index 0970e47..e8803f4 100644",
		"--- a/edited.go",
		"+++ b/moved.go",
		"@@ -2 +2 @@ l1",
		"-l2",
		"+rename from plus.go", // a `+` body line can never match the prefix…
		"rename from decoy.go", // …but a BARE one after the hunk header could
		"rename to decoyto.go",
		`diff --git "a/q\"uote.go" "b/r\"quote.go"`,
		`rename from "q\"uote.go"`,
		`rename to "r\"quote.go"`,
	}, "\n")

	got := parseUnifiedDiffRenames(diff)
	if got["old.go"] != "new.go" {
		t.Errorf("pure rename: got %q, want new.go", got["old.go"])
	}
	if got["edited.go"] != "moved.go" {
		t.Errorf("rename+edit: got %q, want moved.go", got["edited.go"])
	}
	if got[`q"uote.go`] != `r"quote.go` {
		t.Errorf("quoted rename: got %q, want r\"quote.go", got[`q"uote.go`])
	}
	if _, bogus := got["decoy.go"]; bogus {
		t.Errorf("a diff BODY line was read as a rename header: %+v", got)
	}
	if len(got) != 3 {
		t.Errorf("renames = %+v, want exactly 3", got)
	}
}

// TestDurabilityTombstonesPrunedOnAQuietRoot pins the tombstone set's growth
// bound, which the block alone does not give: a mark only leaves the ledger when
// something walks it. Pruning from the commit loop is not enough, because a root
// whose default branch stops advancing (a repo-wide reformat churns every tracked
// path, then the work moves elsewhere) processes no further commits — its marks
// would sit in durability.json forever. The per-poll harvest prunes BEFORE its
// empty-root early return, which is exactly the state that root is left in.
//
// What makes dropping the mark safe is that it is bounded by EVIDENCE, not by
// time, and on the seed gate's OWN predicate: the path fallback cannot fire for a
// path the AI-paths ledger no longer carries, so a mark for such a path is
// unreachable. Here the whole ledger is gone, which is that state at its limit.
func TestDurabilityTombstonesPrunedOnAQuietRoot(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	recordAiTouchedPath("sess-prune", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)

	// A repo-wide reformat churns every tracked line: the path is tombstoned and
	// the root's tracked map disappears with it.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "reformat rewrites all of app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+dayMs)
	if len(loadDurabilityLedger().Seeded[key]) == 0 {
		t.Fatal("a full churn must leave a tombstone")
	}
	if len(loadDurabilityLedger().Roots[key]) != 0 {
		t.Fatal("a fully churned root should have no tracked paths left")
	}

	// The AI evidence for the path ages out of the 7-day ai-paths ledger, which is
	// what makes the mark inert: nothing can seed this path from presence again.
	if err := os.Remove(aiPathsLedgerPath()); err != nil {
		t.Fatalf("expire the ai-paths evidence: %v", err)
	}

	// The default branch never moves again, so no commit is ever processed for
	// this root. Only the per-poll harvest runs — and it must still collect the
	// inert mark.
	harvestDurable(sess, ws, key, t0+2*dayMs)

	if got := loadDurabilityLedger().Seeded[key]; len(got) != 0 {
		t.Errorf("inert tombstones survive on a root whose branch stopped advancing: %+v", got)
	}
}

// ── The same first-touch hole in the sibling pre-merge rework ledger ─────────

// TestReworkFullChurnDoesNotReseedHumanCodeAsAi is defect 1 against rework.go,
// which states the identical invariant in its own header and reached the identical
// hole: a fully churned path was deleted, `remapped` only guards re-seeding within
// the SAME commit, and a LATER commit found nothing tracked and re-seeded from the
// path-granular 7-day AI evidence. Human lines then entered as AI, and any later
// rewrite of them emitted a rework_verdict about code the AI never wrote.
func TestReworkFullChurnDoesNotReseedHumanCodeAsAi(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	// Feature branch (rework is pre-merge only). The AI writes app.go.
	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-rwb3", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	sha1 := gitOut("rev-parse", "HEAD")
	diff1, files1 := commitDiffFiles(t, ws, sha1)
	pollReworkCommit(sess, ws, sha1, diff1, files1, t0)
	if c := reworkCovered(key, "app.go"); !c[1] || !c[2] || !c[3] {
		t.Fatalf("rework ledger = %+v, want 1..3 seeded", c)
	}

	// A human rewrites every AI line: all three churn and the path leaves.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites all of app.go")
	sha2 := gitOut("rev-parse", "HEAD")
	diff2, files2 := commitDiffFiles(t, ws, sha2)
	if v := pollReworkCommit(sess, ws, sha2, diff2, files2, t0+dayMs); len(v) == 0 {
		t.Fatal("a full rewrite must emit a rework verdict")
	}
	if c := reworkCovered(key, "app.go"); len(c) != 0 {
		t.Fatalf("after full churn tracked = %+v, want nothing", c)
	}

	// The same human appends 20 lines. No AI touched this commit — but the day-0
	// path evidence is still inside its 7-day TTL, so attribution still reports
	// likely_ai and the seeding branch must refuse it.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n"+strings.Repeat("human\n", 20))
	git("add", "-A")
	git("commit", "-m", "human appends 20 lines")
	sha3 := gitOut("rev-parse", "HEAD")
	diff3, files3 := commitDiffFiles(t, ws, sha3)
	pollReworkCommit(sess, ws, sha3, diff3, files3, t0+2*dayMs)

	if c := reworkCovered(key, "app.go"); len(c) != 0 {
		t.Fatalf("a purely HUMAN commit was seeded as AI into the rework ledger: %+v", c)
	}

	// And therefore rewriting those human lines reports no rework at all.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n"+strings.Repeat("rewritten\n", 20))
	git("add", "-A")
	git("commit", "-m", "human rewrites their own 20 lines")
	sha4 := gitOut("rev-parse", "HEAD")
	diff4, files4 := commitDiffFiles(t, ws, sha4)
	if v := pollReworkCommit(sess, ws, sha4, diff4, files4, t0+3*dayMs); len(v) != 0 {
		t.Errorf("fabricated rework_verdict over human-written lines: %+v", v)
	}
}

// reworkLineage returns the lineage id recorded for a path's first tracked span.
func reworkLineage(t *testing.T, rootKey, path string) string {
	t.Helper()
	rs := loadReworkLedger().Roots[rootKey][path]
	if len(rs) == 0 {
		t.Fatalf("no tracked rework spans at %s", path)
	}
	return rs[0].LineageID
}

// reworkBorn returns the birth stamp recorded for a path's first tracked span.
func reworkBorn(t *testing.T, rootKey, path string) int64 {
	t.Helper()
	rs := loadReworkLedger().Roots[rootKey][path]
	if len(rs) == 0 {
		t.Fatalf("no tracked rework spans at %s", path)
	}
	return rs[0].BornTsMs
}

// seedReworkCommit stages the worktree, commits, and folds the commit into the
// rework ledger through the real attribution path (same diff, same reconciled
// files as the watcher feeds it).
func seedReworkCommit(t *testing.T, ws string, git func(...string), gitOut func(...string) string, msg string, nowMs int64) (string, []event.Event) {
	t.Helper()
	git("add", "-A")
	git("commit", "-m", msg)
	sha := gitOut("rev-parse", "HEAD")
	// Mirrors attributeAndReworkCommit, including its !ok branch: a pure rename
	// has a real diff but no attributable files, and rework must still see it.
	diff, files, _, ok := commitAttributionFromDiff(ws, ws, sha)
	if diff == "" {
		t.Fatalf("no diff for %s", sha)
	}
	if !ok {
		files = nil
	}
	return sha, pollReworkCommit(Session{DeviceID: "dev", TaskRoot: ws}, ws, sha, diff, files, nowMs)
}

// TestReworkRepeatIterationStaysObserved is the TOO-RESTRICTIVE direction, and
// it is a failure mode in its own right: under-reporting presented as
// measurement. The canonical agent loop is write, rewrite, rewrite again on one
// branch, and repeat iteration on a single file is the core thing rework exists
// to measure. A tombstone that blocked re-seeding outright made every iteration
// after the first invisible for the rest of the branch.
func TestReworkRepeatIterationStaysObserved(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	const t0 int64 = 1_000_000_000_000

	git("checkout", "-b", "feature")

	// Iteration 1: the agent writes helper.go.
	recordAiTouchedPath("sess-loop", key, "helper.go")
	writeCommitFile(t, ws, "helper.go", "a1\na2\na3\n")
	seedReworkCommit(t, ws, git, gitOut, "ai writes helper.go", t0)
	if c := reworkCovered(key, "helper.go"); !c[1] || !c[2] || !c[3] {
		t.Fatalf("rework ledger = %+v, want 1..3 seeded", c)
	}

	// Iteration 2: the agent rewrites it in full. Churn fires and the path leaves.
	recordAiTouchedPath("sess-loop", key, "helper.go")
	writeCommitFile(t, ws, "helper.go", "b1\nb2\nb3\n")
	if _, v := seedReworkCommit(t, ws, git, gitOut, "ai rewrites helper.go", t0+dayMs); len(v) == 0 {
		t.Fatal("the first full rewrite must emit a rework verdict")
	}

	// Iteration 3: the agent writes it AGAIN. This is a genuine agent write, so it
	// must re-enter the ledger — with a fresh lineage and a fresh birth stamp.
	recordAiTouchedPath("sess-loop", key, "helper.go")
	writeCommitFile(t, ws, "helper.go", "c1\nc2\nc3\n")
	sha3, _ := seedReworkCommit(t, ws, git, gitOut, "ai rewrites helper.go again", t0+2*dayMs)
	if c := reworkCovered(key, "helper.go"); len(c) == 0 {
		t.Fatal("iteration 3 was silenced: a fresh agent write must re-enter the ledger")
	}
	if got, want := reworkLineage(t, key, "helper.go"), durLineageID(sha3, "helper.go"); got != want {
		t.Errorf("lineage = %q, want a FRESH lineage %q", got, want)
	}
	if got := reworkBorn(t, key, "helper.go"); got != t0+2*dayMs {
		t.Errorf("BornTsMs = %d, want a FRESH stamp %d", got, t0+2*dayMs)
	}

	// Iteration 4: rewriting it once more is therefore still observed.
	recordAiTouchedPath("sess-loop", key, "helper.go")
	writeCommitFile(t, ws, "helper.go", "d1\nd2\nd3\n")
	_, v := seedReworkCommit(t, ws, git, gitOut, "ai rewrites helper.go a third time", t0+3*dayMs)
	if len(v) == 0 {
		t.Fatal("iteration 4 emitted nothing — repeat iteration is still being dropped")
	}
	if reworked := rangeSet(t, reworkVerdictFor(t, v, "helper.go"), "reworkedRanges"); !reworked["1..3"] {
		t.Errorf("reworkedRanges = %+v, want 1..3", reworked)
	}
}

// TestReworkTombstoneBlocksInferenceWhileAgentWorksElsewhere is the
// TOO-PERMISSIVE direction, and it is the one that must remain impossible.
// Re-entry is authorized by evidence that an agent wrote THIS path again — not
// by the agent merely being alive. An agent busy in the same repo, on other
// files, while a human edits a churned-out path proves nothing about that path,
// and treating session liveness as evidence would relabel the human's lines AI.
func TestReworkTombstoneBlocksInferenceWhileAgentWorksElsewhere(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	const t0 int64 = 1_000_000_000_000

	git("checkout", "-b", "feature")

	recordAiTouchedPath("sess-else", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	seedReworkCommit(t, ws, git, gitOut, "ai adds app.go", t0)

	// A human rewrites every AI line: full churn, the path is tombstoned.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	if _, v := seedReworkCommit(t, ws, git, gitOut, "human rewrites app.go", t0+dayMs); len(v) == 0 {
		t.Fatal("a full rewrite must emit a rework verdict")
	}

	// The agent keeps working — on a DIFFERENT file. That refreshes the session's
	// activity, but says nothing about app.go.
	recordAiTouchedPath("sess-else", key, "other.go")
	writeCommitFile(t, ws, "other.go", "o1\n")
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n"+strings.Repeat("human\n", 20))
	seedReworkCommit(t, ws, git, gitOut, "agent edits other.go, human appends to app.go", t0+2*dayMs)

	if c := reworkCovered(key, "app.go"); len(c) != 0 {
		t.Fatalf("session liveness was taken as evidence: %d human lines seeded as AI at app.go (%+v)", len(c), c)
	}

	// And so rewriting those human lines reports no rework at all.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n"+strings.Repeat("rewritten\n", 20))
	if _, v := seedReworkCommit(t, ws, git, gitOut, "human rewrites their own lines", t0+3*dayMs); len(v) != 0 {
		t.Errorf("fabricated rework_verdict over human-written lines: %+v", v)
	}
}

// writeAiPathsLedger plants an ai-paths.json verbatim, so a test can build ledger
// shapes recordAiTouchedPath no longer produces — notably an entry with Paths
// populated and no per-path stamps, which is what every machine's ledger looks
// like across a fleet upgrade (aiPathsLedgerVersion is deliberately not bumped).
func writeAiPathsLedger(t *testing.T, led aiPathsLedger) {
	t.Helper()
	data, err := json.Marshal(led)
	if err != nil {
		t.Fatalf("marshal ai-paths ledger: %v", err)
	}
	if err := os.WriteFile(aiPathsLedgerPath(), data, 0o600); err != nil {
		t.Fatalf("write ai-paths ledger: %v", err)
	}
}

// TestReworkLegacyAiPathsGrantNoReEntry is mechanism (a). On a machine that was
// capturing before per-path write stamps existed, every path in ai-paths.json has
// presence and no stamp. Borrowing the SESSION's activity stamp to stand in for
// the missing per-path one made the evidence move whenever the agent wrote ANY
// file, so a tombstoned path was re-authorized by an agent editing something else
// entirely — B3 again, reached through the migration window that covers the whole
// deployed fleet.
func TestReworkLegacyAiPathsGrantNoReEntry(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	const t0 int64 = 1_000_000_000_000
	nowMs := time.Now().UnixMilli()

	git("checkout", "-b", "feature")

	// A pre-upgrade ledger: app.go is present, with NO per-path stamp anywhere.
	writeAiPathsLedger(t, aiPathsLedger{
		V: aiPathsLedgerVersion,
		Sessions: map[string]aiPathsEntry{
			"sess-legacy": {Paths: map[string]bool{"app.go": true}, TsMs: nowMs - 60_000, RootKey: key},
		},
	})

	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	seedReworkCommit(t, ws, git, gitOut, "ai adds app.go", t0)
	if c := reworkCovered(key, "app.go"); len(c) == 0 {
		t.Fatalf("legacy path presence must still seed a FIRST touch, got %+v", c)
	}

	// A human rewrites every line: full churn, the path is tombstoned.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	if _, v := seedReworkCommit(t, ws, git, gitOut, "human rewrites app.go", t0+dayMs); len(v) == 0 {
		t.Fatal("a full rewrite must emit a rework verdict")
	}

	// The agent writes an UNRELATED file. That bumps the session's activity stamp
	// and nothing about app.go.
	recordAiTouchedPath("sess-legacy", key, "other.go")
	writeCommitFile(t, ws, "other.go", "o1\n")
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n"+strings.Repeat("human\n", 20))
	seedReworkCommit(t, ws, git, gitOut, "agent writes other.go, human appends to app.go", t0+2*dayMs)

	if c := reworkCovered(key, "app.go"); len(c) != 0 {
		t.Fatalf("a legacy entry with no per-path stamp granted re-entry: %d human lines seeded as AI (%+v)", len(c), c)
	}
}

// TestReworkWinnerFlipGrantsNoReEntry is mechanism (b), and unlike (a) it never
// expires: concurrent agent sessions in one workspace are the normal case. The
// owning session is chosen by SESSION activity while the stamp came from that
// winner's own per-path record, so an older session overtaking a newer one on
// activity — by writing a completely different file — handed back its own OLDER
// stamp for this path. The evidence moved BACKWARDS with nobody touching the
// file, and a `!=` comparison read that as a fresh write.
func TestReworkWinnerFlipGrantsNoReEntry(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	const t0 int64 = 1_000_000_000_000
	nowMs := time.Now().UnixMilli()

	git("checkout", "-b", "feature")

	// Two live sessions have both written app.go. s2 wrote it later, so s2 is the
	// session app.go is read from and s2's stamp is the evidence.
	writeAiPathsLedger(t, aiPathsLedger{
		V: aiPathsLedgerVersion,
		Sessions: map[string]aiPathsEntry{
			"sess-a": {
				Paths:   map[string]bool{"app.go": true},
				PathTs:  map[string]int64{"app.go": nowMs - 30_000},
				TsMs:    nowMs - 30_000,
				RootKey: key,
			},
			"sess-b": {
				Paths:   map[string]bool{"app.go": true},
				PathTs:  map[string]int64{"app.go": nowMs - 10_000},
				TsMs:    nowMs - 10_000,
				RootKey: key,
			},
		},
	})

	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	seedReworkCommit(t, ws, git, gitOut, "ai adds app.go", t0)
	if c := reworkCovered(key, "app.go"); len(c) == 0 {
		t.Fatalf("expected app.go seeded, got %+v", c)
	}

	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	if _, v := seedReworkCommit(t, ws, git, gitOut, "human rewrites app.go", t0+dayMs); len(v) == 0 {
		t.Fatal("a full rewrite must emit a rework verdict")
	}

	// The OLDER session writes a different file. Its activity stamp now leads, so
	// it becomes the session app.go is read from — carrying its older app.go stamp.
	recordAiTouchedPath("sess-a", key, "other.go")
	writeCommitFile(t, ws, "other.go", "o1\n")
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n"+strings.Repeat("human\n", 20))
	seedReworkCommit(t, ws, git, gitOut, "older session writes other.go, human appends to app.go", t0+2*dayMs)

	if c := reworkCovered(key, "app.go"); len(c) != 0 {
		t.Fatalf("a session winner flip granted re-entry: %d human lines seeded as AI (%+v)", len(c), c)
	}
}

// TestReworkStaleEvidenceAfterTtlEvictionGrantsNoReEntry pins STRICTLY NEWER as
// distinct from "changed". Reading the stamp as the max across live sessions
// stops it flipping backwards on a winner change, but it can still legitimately
// REGRESS: the session holding the newest write ages out of the 7-day ai-paths
// TTL (or is evicted by the session cap) and an older session's stamp is all that
// is left. That is a smaller number arriving with no new write behind it, so a
// `!=` comparison would read the loss of evidence as fresh evidence.
func TestReworkStaleEvidenceAfterTtlEvictionGrantsNoReEntry(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	const t0 int64 = 1_000_000_000_000
	nowMs := time.Now().UnixMilli()
	live := aiPathsEntry{
		Paths:   map[string]bool{"app.go": true},
		PathTs:  map[string]int64{"app.go": nowMs - 30_000},
		TsMs:    nowMs - 30_000,
		RootKey: key,
	}

	git("checkout", "-b", "feature")
	writeAiPathsLedger(t, aiPathsLedger{V: aiPathsLedgerVersion, Sessions: map[string]aiPathsEntry{
		"sess-old": live,
		"sess-new": {
			Paths:   map[string]bool{"app.go": true},
			PathTs:  map[string]int64{"app.go": nowMs - 5_000},
			TsMs:    nowMs - 5_000,
			RootKey: key,
		},
	}})

	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	seedReworkCommit(t, ws, git, gitOut, "ai adds app.go", t0)
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	if _, v := seedReworkCommit(t, ws, git, gitOut, "human rewrites app.go", t0+dayMs); len(v) == 0 {
		t.Fatal("a full rewrite must emit a rework verdict")
	}

	// The session that held the newest write ages out; only the older one is left,
	// so the evidence for app.go goes BACKWARDS without anyone writing it.
	writeAiPathsLedger(t, aiPathsLedger{V: aiPathsLedgerVersion, Sessions: map[string]aiPathsEntry{
		"sess-old": live,
	}})

	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n"+strings.Repeat("human\n", 20))
	seedReworkCommit(t, ws, git, gitOut, "human appends after the newer session expired", t0+2*dayMs)

	if c := reworkCovered(key, "app.go"); len(c) != 0 {
		t.Fatalf("evidence going BACKWARDS granted re-entry: %d human lines seeded as AI (%+v)", len(c), c)
	}
}

// TestReworkPureRenameCarriesTheSpan is defect 2 (B4) against the rework ledger:
// a pure rename emits no `@@` hunks under the tracked path, so without the
// rename parser the spans stayed at a path that no longer existed.
func TestReworkPureRenameCarriesTheSpan(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	const t0 int64 = 1_000_000_000_000

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-rwmv", key, "old.go")
	writeCommitFile(t, ws, "old.go", "l1\nl2\nl3\nl4\nl5\nl6\n")
	sha1, _ := seedReworkCommit(t, ws, git, gitOut, "ai adds old.go", t0)

	git("mv", "old.go", "new.go")
	if _, v := seedReworkCommit(t, ws, git, gitOut, "rename old.go -> new.go", t0+dayMs); len(v) != 0 {
		t.Errorf("a pure rename reworks nothing, got %+v", v)
	}

	if c := reworkCovered(key, "old.go"); len(c) != 0 {
		t.Errorf("%d lines still tracked at the dead path old.go, want 0", len(c))
	}
	moved := loadReworkLedger().Roots[key]["new.go"]
	if len(moved) != 1 || moved[0].Start != 1 || moved[0].End != 6 {
		t.Fatalf("new.go ranges = %+v, want a single 1..6 span", moved)
	}
	if moved[0].BornTsMs != t0 {
		t.Errorf("BornTsMs = %d, want %d — a rename is not a rewrite", moved[0].BornTsMs, t0)
	}
	if got, want := moved[0].LineageID, durLineageID(sha1, "old.go"); got != want {
		t.Errorf("lineage = %q, want the ORIGINAL %q carried across the rename", got, want)
	}

	// Rework of the moved lines is now reported at the path that exists.
	recordAiTouchedPath("sess-rwmv", key, "new.go")
	writeCommitFile(t, ws, "new.go", "x1\nx2\nx3\nx4\nx5\nx6\n")
	_, v := seedReworkCommit(t, ws, git, gitOut, "rewrite the moved file", t0+2*dayMs)
	if reworked := rangeSet(t, reworkVerdictFor(t, v, "new.go"), "reworkedRanges"); !reworked["1..6"] {
		t.Errorf("reworkedRanges = %+v, want 1..6 at new.go", reworked)
	}
}

// TestReworkRenameThenRecreateEmitsNoPhantomVerdict is the FABRICATION half of
// defect 2 in rework, reachable through an ordinary split-a-file refactor. A
// stranded span at a dead path is not merely lost: recreating a file at that path
// is a pure insertion, `churnedByHunk` does not churn on OldLen==0, so the stale
// span survives into the NEW file's line space and the next edit reports it as
// reworked AI in a file the agent never touched.
func TestReworkRenameThenRecreateEmitsNoPhantomVerdict(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	const t0 int64 = 1_000_000_000_000

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-split", key, "config.go")
	writeCommitFile(t, ws, "config.go", strings.Repeat("ai\n", 10))
	seedReworkCommit(t, ws, git, gitOut, "ai adds config.go", t0)

	git("mv", "config.go", "config_legacy.go")
	seedReworkCommit(t, ws, git, gitOut, "move config.go aside", t0+dayMs)
	if c := reworkCovered(key, "config_legacy.go"); len(c) != 10 {
		t.Fatalf("the rename must carry all 10 spans, got %+v", c)
	}

	// A brand-new, human-written config.go appears at the freed path.
	writeCommitFile(t, ws, "config.go", strings.Repeat("human\n", 30))
	seedReworkCommit(t, ws, git, gitOut, "new human config.go", t0+2*dayMs)
	if c := reworkCovered(key, "config.go"); len(c) != 0 {
		t.Fatalf("a recreated human file was seeded as AI: %+v", c)
	}

	// Rewriting that human file must report nothing about config.go.
	writeCommitFile(t, ws, "config.go", strings.Repeat("edited\n", 30))
	_, v := seedReworkCommit(t, ws, git, gitOut, "rewrite the human config.go", t0+3*dayMs)
	for _, ev := range v {
		if data, _ := ev.Data.(map[string]interface{}); data["path"] == "config.go" {
			t.Errorf("phantom rework_verdict over a file the AI never wrote: %+v", data)
		}
	}
}

// TestReworkStateClearedWhenTheBranchChanges pins the ledger's actual bound. The
// state describes ONE branch, and the clear used to fire only on standing on the
// default branch — which `git switch -c next-thing` straight off a feature branch
// never does, so the previous branch's ranges and tombstones carried over
// silently into unrelated work.
func TestReworkStateClearedWhenTheBranchChanges(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, _ := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	pollGitWatchWorkspace(sess) // baseline on main

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-branch", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds feature.go")
	pollGitWatchWorkspace(sess)
	if c := reworkCovered(key, "feature.go"); len(c) == 0 {
		t.Fatalf("expected feature.go seeded on the feature branch, got %+v", c)
	}
	// A full churn leaves a tombstone behind as well as emptying the tracked map.
	writeCommitFile(t, ws, "feature.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites feature.go")
	pollGitWatchWorkspace(sess)
	if len(loadReworkLedger().Seeded[key]) == 0 {
		t.Fatal("a full churn must leave a tombstone")
	}

	// Branch straight off the feature branch. main is never checked out, so the
	// scopeDefault clear never runs — the branch change itself must expire it.
	git("checkout", "-b", "next-thing")
	pollGitWatchWorkspace(sess)

	led := loadReworkLedger()
	if len(led.Roots[key]) != 0 {
		t.Errorf("tracked ranges outlived the branch they belong to: %+v", led.Roots[key])
	}
	if len(led.Seeded[key]) != 0 {
		t.Errorf("seed tombstones outlived the branch they belong to: %+v", led.Seeded[key])
	}
	if got := led.Branches[key]; got != "next-thing" {
		t.Errorf("recorded branch = %q, want next-thing", got)
	}
}

// TestReworkSeedTombstonesClearedOnMerge pins the rework tombstones' lifetime.
// They carry no TTL because the merge clears them — so the clear must actually
// reach them. The guard on that clear used to test only the tracked map, which a
// fully churned root no longer has: the marks would then outlive the branch and
// block seeding on every future one, converting a fabrication fix into permanent
// silence.
func TestReworkSeedTombstonesClearedOnMerge(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, _ := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	pollGitWatchWorkspace(sess) // baseline on main

	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-rwtomb", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	pollGitWatchWorkspace(sess)
	if c := reworkCovered(key, "app.go"); !c[1] {
		t.Fatalf("expected app.go seeded on the feature branch, got %+v", c)
	}

	// Full churn: the tracked map for this root empties and the mark is left.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites all of app.go")
	pollGitWatchWorkspace(sess)
	if len(loadReworkLedger().Roots[key]) != 0 {
		t.Fatal("a fully churned root should have no tracked paths left")
	}
	if len(loadReworkLedger().Seeded[key]) == 0 {
		t.Fatal("a full churn must leave a rework tombstone")
	}

	// Merge back. The clear must reach the marks even though Roots[key] is gone.
	git("checkout", "main")
	git("merge", "--squash", "feature")
	git("commit", "-m", "squash: feature")
	pollGitWatchWorkspace(sess)
	if got := loadReworkLedger().Seeded[key]; len(got) != 0 {
		t.Fatalf("rework tombstones outlived the merge: %+v", got)
	}

	// A fresh branch may seed the same path again — the tombstone was scoped to
	// the branch, not to the repo.
	git("checkout", "-b", "feature2")
	recordAiTouchedPath("sess-rwtomb", key, "app.go")
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\nnew1\nnew2\n")
	git("add", "-A")
	git("commit", "-m", "ai extends app.go on a new branch")
	pollGitWatchWorkspace(sess)
	if c := reworkCovered(key, "app.go"); len(c) == 0 {
		t.Error("a merged tombstone permanently blocked seeding on the next branch")
	}
}

// TestDurabilityRenameCarriesTheTombstone: the seed tombstone must follow a file
// across a rename. A path that fully churned is tombstoned; renaming it must not
// hand the same content a fresh, unmarked name under which first-touch seeding
// re-arms — that is the identical hole, relocated.
func TestDurabilityRenameCarriesTheTombstone(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	// The AI evidence covers BOTH names, as it would if the AI did the rename.
	recordAiTouchedPath("sess-rt", key, "old.go")
	recordAiTouchedPath("sess-rt", key, "new.go")
	writeCommitFile(t, ws, "old.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds old.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)

	// Full churn → old.go leaves the ledger and is tombstoned.
	writeCommitFile(t, ws, "old.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites all of old.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+dayMs)
	if got := trackedLineCount(t, key, "old.go"); got != 0 {
		t.Fatalf("full churn should have emptied old.go, got %d", got)
	}

	// Rename it. Nothing is tracked, so nothing moves — but the MARK must.
	git("mv", "old.go", "new.go")
	git("commit", "-m", "rename old.go -> new.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+2*dayMs)

	// A purely human commit under the new name must still be refused.
	writeCommitFile(t, ws, "new.go", "h1\nh2\nh3\n"+strings.Repeat("human\n", 8))
	git("add", "-A")
	git("commit", "-m", "human appends under the new name")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+3*dayMs)

	if got := trackedLineCount(t, key, "new.go"); got != 0 {
		t.Errorf("the tombstone did not follow the rename: %d human lines seeded as AI at new.go (ranges %+v)",
			got, ledgerRanges(t, key, "new.go"))
	}
}

// TestDurabilityTombstoneSurvivesAStamplessAiPathsLedger runs the sequence every
// ALREADY-DEPLOYED install is in, which no other test here can reach: they all
// seed evidence through recordAiTouchedPath, which always writes a per-path
// stamp. An ai-paths.json written before PathTs existed has Paths populated and
// no stamps at all, and its version is deliberately not bumped, so it is read for
// its full 7-day TTL with every WriteMs reading 0.
//
// A tombstone stamped from that evidence therefore holds 0 — correctly, since 0
// means "no per-write evidence, only presence". Pruning on the stamp read that as
// a dead mark and deleted it on the very next poll, one statement before the seed
// gate consulted the path it was still carrying, and the human commit that
// followed re-entered the ledger as fresh AI. The gate and the pruner must be the
// same predicate for that reason and not merely compatible ones.
func TestDurabilityTombstoneSurvivesAStamplessAiPathsLedger(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	// Written by hand, NOT through recordAiTouchedPath: the point is the shape a
	// pre-PathTs daemon left on disk. TsMs is real time because the reader's TTL
	// check is against the wall clock, not the ledger's simulated one.
	legacy := aiPathsLedger{V: aiPathsLedgerVersion, Sessions: map[string]aiPathsEntry{
		"legacy-session": {
			Paths:   map[string]bool{"app.go": true},
			TsMs:    time.Now().UnixMilli(),
			RootKey: key,
		},
	}}
	blob, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aiPathsLedgerPath(), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readAiPathMarks(key)["app.go"]; got.WriteMs != 0 {
		t.Fatalf("setup: a stampless entry must read WriteMs 0, got %d", got.WriteMs)
	}

	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)
	if got := trackedLineCount(t, key, "app.go"); got != 3 {
		t.Fatalf("setup: presence must seed the path, got %d lines", got)
	}

	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites all of app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+dayMs)
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Fatalf("setup: the full churn must empty the path, got %d lines", got)
	}
	if _, tombstoned := loadDurabilityLedger().Seeded[key]["app.go"]; !tombstoned {
		t.Fatal("setup: the churn must leave a tombstone")
	}

	// The next poll prunes before it seeds. The path is still in the ledger the
	// gate reads, so the mark is still live and this human commit stays human.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n"+strings.Repeat("human\n", 8))
	git("add", "-A")
	git("commit", "-m", "human appends")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+2*dayMs)

	if _, tombstoned := loadDurabilityLedger().Seeded[key]["app.go"]; !tombstoned {
		t.Error("a 0-stamp tombstone was pruned while its path was still seedable")
	}
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Errorf("human code re-seeded as AI on a stampless ledger: %d lines tracked, want 0 (ranges %+v)",
			got, ledgerRanges(t, key, "app.go"))
	}
}
