package capture

import (
	"strings"
	"testing"
)

// firstLineage pulls the lineageId off the first element of a verdict range array.
func firstLineage(t *testing.T, data map[string]interface{}, field string) string {
	t.Helper()
	arr, ok := data[field].([]interface{})
	if !ok || len(arr) == 0 {
		t.Fatalf("no ranges in %s: %+v", field, data[field])
	}
	lin, _ := arr[0].(map[string]interface{})["lineageId"].(string)
	return lin
}

// TestParseUnifiedDiffNewLines pins the body parser: new-file numbering starts at
// the hunk's NewStart, `-` lines never advance the new-side counter, and every
// `+` line is fingerprinted (not stored raw).
func TestParseUnifiedDiffNewLines(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -5 +5 @@ ctx", // replace old line 5 with new line 5
		"-old5",
		"+new5",
		"@@ -9,0 +10,2 @@ ctx", // insert 2 lines at new 10..11
		"+ins10",
		"+ins11",
	}, "\n")

	got := parseUnifiedDiffNewLines(diff)
	app := got["app.go"]
	if len(app) != 3 {
		t.Fatalf("app.go new lines = %+v, want 3 (5,10,11)", app)
	}
	if app[5] != lineFingerprint("new5") {
		t.Errorf("line 5 fp = %q, want fp(new5)", app[5])
	}
	if app[10] != lineFingerprint("ins10") || app[11] != lineFingerprint("ins11") {
		t.Errorf("insert lines mis-numbered: %+v", app)
	}
	if _, present := app[6]; present {
		t.Errorf("a `-` line must not advance the new-side counter: %+v", app)
	}
}

// TestMatchedAiRunsMinRunGuard: only contiguous runs of >= minRun matched lines
// transfer. An isolated line that happens to match an AI fingerprint is dropped
// (the false-positive guard), and a transferred run carries its lineage.
func TestMatchedAiRunsMinRunGuard(t *testing.T) {
	fps := map[string]string{"h1": "lin-A", "h2": "lin-A", "h3": "lin-A"}
	// Lines 1..3 are a contiguous match; line 5 matches h1 but is isolated.
	newLines := map[int]string{1: "h1", 2: "h2", 3: "h3", 4: "nomatch", 5: "h1"}

	runs := matchedAiRuns(newLines, fps, 2)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want exactly 1 (the 1..3 block; line 5 dropped)", runs)
	}
	if runs[0].Start != 1 || runs[0].End != 3 {
		t.Errorf("run = %+v, want 1..3", runs[0])
	}
	if runs[0].LineageID != "lin-A" {
		t.Errorf("lineage = %q, want lin-A", runs[0].LineageID)
	}

	// With minRun=1, the isolated line 5 would also transfer — proving the guard
	// is what suppresses it, not the matching.
	if loose := matchedAiRuns(newLines, fps, 1); len(loose) != 2 {
		t.Errorf("minRun=1 runs = %+v, want 2 (1..3 and 5..5)", loose)
	}
}

// TestDurabilityFingerprintsFileScoped: fingerprints are keyed by path, so
// identical boilerplate in a different file has NO fingerprint evidence and can
// never transfer (the cross-file false-positive guard).
func TestDurabilityFingerprintsFileScoped(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	const t0 int64 = 1_000_000_000_000
	key := "rk-filescope"
	diff := strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- /dev/null",
		"+++ b/app.go",
		"@@ -0,0 +1,3 @@",
		"+func A() {",
		"+  return 1",
		"+}",
	}, "\n")
	files := []attrFile{{Path: "app.go", LineRanges: []attrLineRange{{Start: 1, End: 3, Attribution: attributionLikelyAI}}}}

	recordAiFingerprints(key, "sha-app", diff, files, t0)

	if fingerprintsForPath(key, "app.go", t0) == nil {
		t.Fatal("app.go should have captured fingerprints")
	}
	// boiler.go containing an identical `}` gets NOTHING — fps are file-scoped.
	if fps := fingerprintsForPath(key, "boiler.go", t0); fps != nil {
		t.Errorf("boiler.go must have no fingerprints (file-scoped), got %+v", fps)
	}
	// A non-likely_ai range is never captured.
	if fingerprintsForPath("rk-other", "app.go", t0) != nil {
		t.Error("a different root key must not see app.go fingerprints")
	}
}

// TestDurabilitySquashMergeTransfersAttribution (the long pole): AI lines written
// on a feature branch, then SQUASH-merged to the default branch under a brand-new
// SHA with no ancestry, are still attributed as AI — transferred by content
// fingerprint — and tracked to durability on default, carrying the feature
// commit's lineage.
func TestDurabilitySquashMergeTransfersAttribution(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	roots := []string{ws}
	const t0 int64 = 1_000_000_000_000

	pollDurability(sess, roots, t0) // baseline main

	// Feature branch: the AI writes feature.go.
	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-sq", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "func A() {\n\treturn 1\n}\n")
	git("add", "-A")
	git("commit", "-m", "ai adds feature.go")
	featureSha := gitOut("rev-parse", "HEAD")
	// Capture fingerprints exactly as the attribution watcher would on the branch.
	attributeCommit(sess, ws, featureSha, t0+dayMs, siblingLineage{})

	// Squash-merge feature into main: one NEW commit, no ancestry to featureSha.
	git("checkout", "main")
	git("merge", "--squash", "feature")
	git("commit", "-m", "squash: feature")

	// Durability polls main, sees the squash commit, transfers via fingerprints.
	land := t0 + 2*dayMs
	pollDurability(sess, roots, land)

	// 30d after LANDING on main, feature.go's AI lines are durable, lineage kept.
	v := harvestDurable(sess, ws, key, land+31*dayMs)
	data := durVerdictFor(t, v, "feature.go")
	durable := rangeSet(t, data, "durableRanges")
	if !durable["1..3"] {
		t.Errorf("durableRanges = %+v, want 1..3 transferred through squash", durable)
	}
	if lin := firstLineage(t, data, "durableRanges"); lin != featureSha+":feature.go" {
		t.Errorf("lineageId = %q, want %q", lin, featureSha+":feature.go")
	}
}

// TestDurabilityCherryPickFollowsLineage (§3.4): a cherry-pick replays a feature
// commit onto the default branch under a BRAND-NEW sha (distinct from the feature
// commit, unlike a squash it stays a single commit). The AI lines must still
// transfer by content fingerprint AND carry the ORIGINAL feature commit's
// lineage — not the cherry-pick's sha — so the backend follows the line to the
// commit that actually authored it across the rewrite.
func TestDurabilityCherryPickFollowsLineage(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	roots := []string{ws}
	const t0 int64 = 1_000_000_000_000

	pollDurability(sess, roots, t0) // baseline main

	// Feature branch: the AI writes feature.go, then we capture fingerprints just
	// as the attribution watcher would on the branch.
	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-cp", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "func A() {\n\treturn 1\n}\n")
	git("add", "-A")
	git("commit", "-m", "ai adds feature.go")
	featureSha := gitOut("rev-parse", "HEAD")
	attributeCommit(sess, ws, featureSha, t0+dayMs, siblingLineage{})

	// main advances so the cherry-pick lands on a different parent — otherwise git
	// recreates a byte-identical commit (same tree+parent) and reuses the sha.
	git("checkout", "main")
	writeCommitFile(t, ws, "other.txt", "unrelated\n")
	git("add", "-A")
	git("commit", "-m", "main advances")

	// Cherry-pick feature's commit onto main: a NEW sha, single commit, no squash.
	git("cherry-pick", "feature")
	cherrySha := gitOut("rev-parse", "HEAD")
	if cherrySha == featureSha {
		t.Fatalf("cherry-pick must produce a new sha; got %q == featureSha", cherrySha)
	}

	// Durability polls main, sees the cherry-picked commit, transfers by fingerprint.
	land := t0 + 2*dayMs
	pollDurability(sess, roots, land)

	// 30d after landing, feature.go's AI lines are durable and lineage points at
	// the ORIGINAL feature commit, not the cherry-pick sha.
	v := harvestDurable(sess, ws, key, land+31*dayMs)
	data := durVerdictFor(t, v, "feature.go")
	durable := rangeSet(t, data, "durableRanges")
	if !durable["1..3"] {
		t.Errorf("durableRanges = %+v, want 1..3 transferred through cherry-pick", durable)
	}
	if lin := firstLineage(t, data, "durableRanges"); lin != featureSha+":feature.go" {
		t.Errorf("lineageId = %q, want %q (original feature commit, not cherry sha)", lin, featureSha+":feature.go")
	}
}

// TestDurabilityRebaseFollowsLineage (§3.4): a rebase replays a feature commit
// onto an ADVANCED default-branch tip under a new sha, then fast-forwards main
// onto it. The AI lines transfer by fingerprint and keep the original feature
// commit's lineage across the rebase rewrite (same mechanism as cherry-pick;
// pinned separately because the spec enumerates rebase and cherry-pick).
func TestDurabilityRebaseFollowsLineage(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	roots := []string{ws}
	const t0 int64 = 1_000_000_000_000

	pollDurability(sess, roots, t0) // baseline main

	// Feature branch off base: the AI writes feature.go; capture fingerprints.
	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-rb", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "func A() {\n\treturn 1\n}\n")
	git("add", "-A")
	git("commit", "-m", "ai adds feature.go")
	featureSha := gitOut("rev-parse", "HEAD")
	attributeCommit(sess, ws, featureSha, t0+dayMs, siblingLineage{})

	// main advances independently, so the feature commit cannot fast-forward as-is.
	git("checkout", "main")
	writeCommitFile(t, ws, "other.txt", "unrelated\n")
	git("add", "-A")
	git("commit", "-m", "main advances")

	// Rebase feature onto the new main tip (new sha), then fast-forward main onto it.
	git("checkout", "feature")
	git("rebase", "main")
	rebasedSha := gitOut("rev-parse", "HEAD")
	if rebasedSha == featureSha {
		t.Fatalf("rebase must produce a new sha; got %q == featureSha", rebasedSha)
	}
	git("checkout", "main")
	git("merge", "--ff-only", "feature")

	// Durability polls main, sees the rebased commit, transfers by fingerprint.
	land := t0 + 2*dayMs
	pollDurability(sess, roots, land)

	v := harvestDurable(sess, ws, key, land+31*dayMs)
	data := durVerdictFor(t, v, "feature.go")
	durable := rangeSet(t, data, "durableRanges")
	if !durable["1..3"] {
		t.Errorf("durableRanges = %+v, want 1..3 transferred through rebase", durable)
	}
	if lin := firstLineage(t, data, "durableRanges"); lin != featureSha+":feature.go" {
		t.Errorf("lineageId = %q, want %q (original feature commit, not rebased sha)", lin, featureSha+":feature.go")
	}
}

// TestDurabilitySquashDoesNotTransferHumanLines: a squash whose file mixes the
// AI block with human lines transfers ONLY the AI block — human lines the branch
// never fingerprinted stay unknown (not durable).
func TestDurabilitySquashDoesNotTransferHumanLines(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	const t0 int64 = 1_000_000_000_000
	key := "rk-mixed"

	// The branch fingerprinted ONLY the two AI lines of app.go.
	branchDiff := strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- /dev/null",
		"+++ b/app.go",
		"@@ -0,0 +1,2 @@",
		"+ai_line_one",
		"+ai_line_two",
	}, "\n")
	files := []attrFile{{Path: "app.go", LineRanges: []attrLineRange{{Start: 1, End: 2, Attribution: attributionLikelyAI}}}}
	recordAiFingerprints(key, "branchsha", branchDiff, files, t0)

	// The squash lands app.go with the AI block at 1..2, human lines 3..4, and a
	// stray recurrence of an AI line at 6 (isolated).
	squashNewLines := parseUnifiedDiffNewLines(strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- /dev/null",
		"+++ b/app.go",
		"@@ -0,0 +1,6 @@",
		"+ai_line_one",
		"+ai_line_two",
		"+human_three",
		"+human_four",
		"+human_five",
		"+ai_line_one",
	}, "\n"))["app.go"]

	fps := fingerprintsForPath(key, "app.go", t0)
	runs := matchedAiRuns(squashNewLines, fps, durabilityMinTransferRun)
	if len(runs) != 1 || runs[0].Start != 1 || runs[0].End != 2 {
		t.Fatalf("runs = %+v, want only the 1..2 AI block (human 3..5 and stray 6 excluded)", runs)
	}
}

// TestMatchedAiRunsSplitsOnLineage: two fingerprint-matched lines that are
// adjacent but belong to DIFFERENT feature commits must NOT be merged into one
// run under the first line's lineage — each keeps its own lineageId so the
// backend attributes each line to the commit that actually wrote it.
func TestMatchedAiRunsSplitsOnLineage(t *testing.T) {
	fps := map[string]string{"h1": "lin-A", "h2": "lin-B"}
	newLines := map[int]string{1: "h1", 2: "h2"} // adjacent, different lineages

	// minRun=1 so each singleton survives and we can inspect the split directly.
	runs := matchedAiRuns(newLines, fps, 1)
	if len(runs) != 2 {
		t.Fatalf("runs = %+v, want 2 (a lineage change must break the run)", runs)
	}
	byLineage := map[string]durTrackedRange{}
	for _, r := range runs {
		byLineage[r.LineageID] = r
	}
	if r, ok := byLineage["lin-A"]; !ok || r.Start != 1 || r.End != 1 {
		t.Errorf("lin-A run = %+v, want 1..1", r)
	}
	if r, ok := byLineage["lin-B"]; !ok || r.Start != 2 || r.End != 2 {
		t.Errorf("lin-B run = %+v, want 2..2", r)
	}
}

// TestDurabilityFingerprintRecaptureRefreshesTTL: re-capturing identical AI
// content must refresh its bornTs (dedup keeps the freshest, not the oldest),
// so a later squash within the fresh capture's TTL still finds the evidence.
func TestDurabilityFingerprintRecaptureRefreshesTTL(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	const t0 int64 = 1_000_000_000_000
	key := "rk-ttl"
	diff := strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- /dev/null",
		"+++ b/app.go",
		"@@ -0,0 +1,2 @@",
		"+ai_line_one",
		"+ai_line_two",
	}, "\n")
	files := []attrFile{{Path: "app.go", LineRanges: []attrLineRange{{Start: 1, End: 2, Attribution: attributionLikelyAI}}}}

	recordAiFingerprints(key, "sha-old", diff, files, t0)
	// Re-capture the SAME content 10 days later (well before the 14d TTL).
	recordAiFingerprints(key, "sha-new", diff, files, t0+10*dayMs)

	// 16 days after the FIRST capture: the old entry has expired, but the refresh
	// (born at t0+10d, now 6d old) must keep the evidence live.
	if fps := fingerprintsForPath(key, "app.go", t0+16*dayMs); fps == nil {
		t.Fatal("recapture must refresh bornTs so evidence survives past the first entry's TTL")
	}
}

// TestDurabilitySquashSeedsBeforeRecordingOwnFingerprints (ordering guard): the
// squash landing on the default branch is BOTH the new working-HEAD commit and
// the new default-branch commit. Durability seeding must run BEFORE
// attributeCommit records the squash's own fingerprints — otherwise path-level
// attribution marks every changed line of the AI-touched file as likely_ai,
// fingerprints them, and the human lines in the squash transfer as AI. Driving
// the real poll cycle, only the true AI block may land in the ledger.
func TestDurabilitySquashSeedsBeforeRecordingOwnFingerprints(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, _ := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")
	git("branch", "-M", "main")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}

	// Cycle 1: baseline both cursors on main.
	pollGitWatchWorkspace(sess)

	// Feature branch: the AI writes feature.go (3 AI lines only).
	git("checkout", "-b", "feature")
	recordAiTouchedPath("sess-sq", key, "feature.go")
	writeCommitFile(t, ws, "feature.go", "ai_a\nai_b\nai_c\n")
	git("add", "-A")
	git("commit", "-m", "ai adds feature.go")
	// Cycle 2 (working HEAD = feature): attributeCommit records the 3 AI
	// fingerprints; main has not moved, so nothing is seeded yet.
	pollGitWatchWorkspace(sess)

	// Squash-merge into main, and the landed file gains 2 HUMAN lines the branch
	// never fingerprinted. feature.go is AI-touched, so path-level attribution on
	// the squash would tag all 5 lines likely_ai if it ran before seeding.
	git("checkout", "main")
	git("merge", "--squash", "feature")
	writeCommitFile(t, ws, "feature.go", "ai_a\nai_b\nai_c\nhuman_d\nhuman_e\n")
	git("add", "-A")
	git("commit", "-m", "squash: feature (+ human lines)")
	// Cycle 3: pollDurability runs first, seeding the squash against the EARLIER
	// feature fingerprints only.
	pollGitWatchWorkspace(sess)

	tracked := loadDurabilityLedger().Roots[key]["feature.go"]
	covered := map[int]bool{}
	for _, r := range tracked {
		for ln := r.Start; ln <= r.End; ln++ {
			covered[ln] = true
		}
	}
	if !covered[1] || !covered[2] || !covered[3] {
		t.Errorf("tracked = %+v, want the AI block 1..3 seeded", tracked)
	}
	if covered[4] || covered[5] {
		t.Errorf("tracked = %+v, human lines 4..5 must NOT transfer (seed ran before recording own fps)", tracked)
	}
}

// aiDiff builds a one-file diff whose every added line is likely_ai, for the
// prune tests below (which care about entry COUNTS, not diff parsing).
func aiDiff(path string, lines []string) (string, []attrFile) {
	body := []string{
		"diff --git a/" + path + " b/" + path,
		"--- /dev/null",
		"+++ b/" + path,
		"@@ -0,0 +1," + itoa(len(lines)) + " @@",
	}
	for _, l := range lines {
		body = append(body, "+"+l)
	}
	files := []attrFile{{Path: path, LineRanges: []attrLineRange{{Start: 1, End: len(lines), Attribution: attributionLikelyAI}}}}
	return strings.Join(body, "\n"), files
}

// countFingerprints totals the entries actually PERSISTED, not what a read-time
// filter would show — the leak this pins was invisible to fingerprintsForPath.
func countFingerprints(store durabilityFingerprints) int {
	n := 0
	for _, paths := range store.Roots {
		for _, entries := range paths {
			n += len(entries)
		}
	}
	return n
}

// TestDurabilityFingerprintsPruneSweepsUntouchedPaths: expiry must sweep the
// WHOLE store on every write, not just the paths the current commit touched.
// Before the global prune, a repo that stopped receiving commits kept its
// fingerprints on disk forever — 65% of one measured 65 MB store was expired.
func TestDurabilityFingerprintsPruneSweepsUntouchedPaths(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	const t0 int64 = 1_000_000_000_000

	// An abandoned repo: captured once, never committed to again.
	staleDiff, staleFiles := aiDiff("stale.go", []string{"stale_one", "stale_two"})
	recordAiFingerprints("rk-abandoned", "sha-stale", staleDiff, staleFiles, t0)
	if countFingerprints(loadDurabilityFingerprints()) != 2 {
		t.Fatalf("setup: want 2 persisted entries, got %d", countFingerprints(loadDurabilityFingerprints()))
	}

	// 20 days later a DIFFERENT root sees a commit. That write is the only thing
	// that ever runs again — and it must carry the abandoned root out with it.
	liveDiff, liveFiles := aiDiff("live.go", []string{"live_one"})
	recordAiFingerprints("rk-live", "sha-live", liveDiff, liveFiles, t0+20*dayMs)

	store := loadDurabilityFingerprints()
	if _, ok := store.Roots["rk-abandoned"]; ok {
		t.Errorf("expired root must be deleted from the FILE, got %+v", store.Roots["rk-abandoned"])
	}
	if got := countFingerprints(store); got != 1 {
		t.Errorf("persisted entries = %d, want 1 (only the live capture)", got)
	}
	// The live root is untouched by the sweep.
	if fingerprintsForPath("rk-live", "live.go", t0+20*dayMs) == nil {
		t.Error("in-TTL fingerprints must survive the sweep")
	}
}

// TestDurabilityFingerprintsEntryCap: the backstop bounds the file even when
// every entry is inside its TTL, and evicts OLDEST first so the evidence most
// likely to still be awaiting a squash is the evidence kept.
func TestDurabilityFingerprintsEntryCap(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	old := durabilityFingerprintsMaxEntries
	durabilityFingerprintsMaxEntries = 4
	t.Cleanup(func() { durabilityFingerprintsMaxEntries = old })

	const t0 int64 = 1_000_000_000_000
	oldDiff, oldFiles := aiDiff("old.go", []string{"old_a", "old_b", "old_c"})
	recordAiFingerprints("rk-cap", "sha-old", oldDiff, oldFiles, t0)

	// Three more, one hour later and all well inside the TTL: 6 entries against a
	// cap of 4, so the 2 oldest go.
	newDiff, newFiles := aiDiff("new.go", []string{"new_a", "new_b", "new_c"})
	recordAiFingerprints("rk-cap", "sha-new", newDiff, newFiles, t0+3_600_000)

	store := loadDurabilityFingerprints()
	if got := countFingerprints(store); got != 4 {
		t.Fatalf("persisted entries = %d, want the cap of 4", got)
	}
	// All three newest survive; the older file is down to its single newest line.
	if got := len(store.Roots["rk-cap"]["new.go"]); got != 3 {
		t.Errorf("new.go entries = %d, want 3 (newest are never evicted first)", got)
	}
	if got := len(store.Roots["rk-cap"]["old.go"]); got != 1 {
		t.Errorf("old.go entries = %d, want 1 (2 of 3 evicted as oldest)", got)
	}
}

// TestDurabilityFingerprintsPruneDropsEmptyContainers: a path emptied by expiry
// must not linger as an empty map, or the store keeps paying marshal and load
// cost for roots that hold nothing.
func TestDurabilityFingerprintsPruneDropsEmptyContainers(t *testing.T) {
	const t0 int64 = 1_000_000_000_000
	store := durabilityFingerprints{V: durabilityFingerprintsVersion, Roots: map[string]map[string][]durAiFingerprint{
		"rk": {
			"gone.go": {{FP: "a", LineageID: "lin", BornTsMs: t0}},
			"kept.go": {{FP: "b", LineageID: "lin", BornTsMs: t0 + 19*dayMs}},
		},
	}}
	pruneDurabilityFingerprints(&store, t0+20*dayMs)

	if _, ok := store.Roots["rk"]["gone.go"]; ok {
		t.Error("expired path must be deleted, not left empty")
	}
	if len(store.Roots["rk"]["kept.go"]) != 1 {
		t.Errorf("in-TTL path must survive, got %+v", store.Roots["rk"]["kept.go"])
	}

	// Now expire the survivor too: the root itself goes.
	pruneDurabilityFingerprints(&store, t0+40*dayMs)
	if _, ok := store.Roots["rk"]; ok {
		t.Errorf("emptied root must be deleted, got %+v", store.Roots["rk"])
	}
}

// TestFingerprintsForPathInMatchesLoadingPerPath: the one-load lookup used by
// durability seeding must be behaviourally identical to the per-path convenience
// wrapper — same hits, same misses, same TTL boundary, same lineage. The refactor
// that stopped re-reading the file per path is only safe if these never diverge.
func TestFingerprintsForPathInMatchesLoadingPerPath(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	const t0 int64 = 1_000_000_000_000

	aDiff, aFiles := aiDiff("a.go", []string{"a_one", "a_two"})
	recordAiFingerprints("rk-eq", "sha-a", aDiff, aFiles, t0)
	bDiff, bFiles := aiDiff("b.go", []string{"b_one"})
	recordAiFingerprints("rk-eq", "sha-b", bDiff, bFiles, t0+dayMs)

	store := loadDurabilityFingerprints()
	// Include a path with no evidence and a root that does not exist: nil-vs-nil
	// has to agree too, since nil is what makes the caller fall back to
	// path-level seeding.
	for _, tc := range []struct{ rootKey, path string }{
		{"rk-eq", "a.go"},
		{"rk-eq", "b.go"},
		{"rk-eq", "never-touched.go"},
		{"rk-missing", "a.go"},
	} {
		for _, now := range []int64{t0, t0 + 13*dayMs, t0 + 20*dayMs} {
			want := fingerprintsForPath(tc.rootKey, tc.path, now)
			got := fingerprintsForPathIn(store, tc.rootKey, tc.path, now)
			if len(want) != len(got) {
				t.Fatalf("%s/%s @+%dd: got %d entries, want %d", tc.rootKey, tc.path, (now-t0)/dayMs, len(got), len(want))
			}
			for fp, lineage := range want {
				if got[fp] != lineage {
					t.Errorf("%s/%s @+%dd: fp %s → %q, want %q", tc.rootKey, tc.path, (now-t0)/dayMs, fp, got[fp], lineage)
				}
			}
		}
	}

	// The TTL boundary is where a divergence would actually bite: a.go is live at
	// 13 days and gone at 20, and both variants must agree on that.
	if fingerprintsForPathIn(store, "rk-eq", "a.go", t0+13*dayMs) == nil {
		t.Error("a.go must be live inside the TTL")
	}
	if fingerprintsForPathIn(store, "rk-eq", "a.go", t0+20*dayMs) != nil {
		t.Error("a.go must be filtered out past the TTL")
	}
}
