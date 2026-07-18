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
	attributeCommit(sess, ws, featureSha, t0+dayMs)

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
