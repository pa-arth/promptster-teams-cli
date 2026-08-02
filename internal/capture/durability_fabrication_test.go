package capture

import (
	"strings"
	"testing"
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

// TestDurabilitySeedTombstoneExpiresWhenThePathGoesQuiet pins the tombstone's
// LIFETIME, which is the whole design question the fix turns on. A permanent
// tombstone would grow the ledger without bound; too short a one re-opens the
// hole. The tombstone lives durabilitySeedTombstoneTTLms past the LAST commit to
// the path, so an actively-worked path is protected forever while a path nobody
// has touched for a full window drops out — by which time the 7-day AI path
// evidence that could re-fire the seed is long dead anyway.
func TestDurabilitySeedTombstoneExpiresWhenThePathGoesQuiet(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000

	recordAiTouchedPath("sess-ttl", key, "app.go")
	writeCommitFile(t, ws, "app.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0)

	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites all of app.go")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), t0+dayMs)

	// A commit inside the window is blocked (defect 1) and REFRESHES the stamp.
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\nh4\n")
	git("add", "-A")
	git("commit", "-m", "human appends inside the window")
	inWindow := t0 + dayMs + durabilitySeedTombstoneTTLms - dayMs
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), inWindow)
	if got := trackedLineCount(t, key, "app.go"); got != 0 {
		t.Fatalf("inside the tombstone window the seed must stay blocked, got %d lines", got)
	}

	// One full TTL after the LAST touch the path is quiet enough to be treated as
	// unseen again. (This is the deliberate ceiling on tombstone growth.)
	writeCommitFile(t, ws, "app.go", "h1\nh2\nh3\nh4\nh5\n")
	git("add", "-A")
	git("commit", "-m", "much later")
	pollDurabilityCommit(ws, key, sess, gitOut("rev-parse", "HEAD"), inWindow+durabilitySeedTombstoneTTLms+dayMs)
	if got := trackedLineCount(t, key, "app.go"); got == 0 {
		t.Error("the tombstone never expires — the seeded-path set grows without bound")
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
