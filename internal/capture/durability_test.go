package capture

import (
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

const dayMs int64 = 24 * 60 * 60 * 1000

// durVerdictFor pulls the durability verdict event for a given path out of a
// slice, or fails. Verdicts are emitted one per (commit, path).
func durVerdictFor(t *testing.T, evs []event.Event, path string) map[string]interface{} {
	t.Helper()
	for _, ev := range evs {
		if ev.Kind != "durability_verdict" {
			t.Fatalf("kind = %q, want durability_verdict", ev.Kind)
		}
		data, ok := ev.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("event Data is %T, want map", ev.Data)
		}
		if data["path"] == path {
			return data
		}
	}
	t.Fatalf("no durability_verdict for %q in %d event(s)", path, len(evs))
	return nil
}

// rangeSet flattens a verdict's range array ("durableRanges" or "churnedRanges")
// into a set of "start..end" strings for order-independent assertions.
func rangeSet(t *testing.T, data map[string]interface{}, field string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	arr, ok := data[field].([]interface{})
	if !ok {
		return out
	}
	for _, r := range arr {
		rm := r.(map[string]interface{})
		out[itoaF(rm["start"])+".."+itoaF(rm["end"])] = true
	}
	return out
}

func itoaF(v interface{}) string {
	switch n := v.(type) {
	case int:
		return strings.TrimSpace(strings.NewReplacer().Replace(itoa(n)))
	case float64:
		return itoa(int(n))
	}
	return "?"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestParseUnifiedDiffHunks pins the hunk parser (old AND new side) against real
// git --unified=0 forms: a replace, a pure insertion, and a pure deletion.
func TestParseUnifiedDiffHunks(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/foo.go b/foo.go",
		"--- a/foo.go",
		"+++ b/foo.go",
		"@@ -2 +2 @@ ctx", // replace old line 2 with new line 2
		"-b",
		"+B",
		"@@ -3,0 +4,2 @@ ctx", // insert 2 lines after old line 3
		"+d",
		"+e",
		"@@ -10,2 +12,0 @@ ctx", // delete old lines 10-11
		"-x",
		"-y",
	}, "\n")

	got := parseUnifiedDiffHunks(diff)
	foo := got["foo.go"]
	if len(foo) != 3 {
		t.Fatalf("foo.go hunks = %+v, want 3", foo)
	}
	if foo[0] != (diffHunk{OldStart: 2, OldLen: 1, NewStart: 2, NewLen: 1}) {
		t.Errorf("hunk 0 = %+v, want replace 2,1/2,1", foo[0])
	}
	if foo[1] != (diffHunk{OldStart: 3, OldLen: 0, NewStart: 4, NewLen: 2}) {
		t.Errorf("hunk 1 = %+v, want insert 3,0/4,2", foo[1])
	}
	if foo[2] != (diffHunk{OldStart: 10, OldLen: 2, NewStart: 12, NewLen: 0}) {
		t.Errorf("hunk 2 = %+v, want delete 10,2/12,0", foo[2])
	}
}

// TestDurabilityLineDurableAfter30d: an AI line committed to the branch and left
// untouched is reported durable once it crosses the 30-day window, exactly once.
func TestDurabilityLineDurableAfter30d(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}

	recordAiTouchedPath("sess-dur", key, "ai.go")
	writeCommitFile(t, ws, "ai.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds ai.go")
	sha := gitOut("rev-parse", "HEAD")

	const t0 int64 = 1_000_000_000_000

	// Seed the ledger from the AI commit; nothing churned, no verdict yet.
	if churn := pollDurabilityCommit(ws, key, sess, sha, t0); len(churn) != 0 {
		t.Fatalf("seeding an AI commit must churn nothing, got %+v", churn)
	}
	// Before 30 days: no durable verdict.
	if v := harvestDurable(sess, ws, key, t0+29*dayMs); len(v) != 0 {
		t.Fatalf("nothing is durable before 30d, got %+v", v)
	}
	// After 30 days: ai.go lines 1..3 are durable, emitted once.
	v := harvestDurable(sess, ws, key, t0+31*dayMs)
	data := durVerdictFor(t, v, "ai.go")
	durable := rangeSet(t, data, "durableRanges")
	if !durable["1..3"] {
		t.Errorf("durableRanges = %+v, want 1..3", durable)
	}
	// Idempotent: a second harvest re-emits nothing (the range resolved).
	if again := harvestDurable(sess, ws, key, t0+31*dayMs); len(again) != 0 {
		t.Errorf("a durable range must be emitted once, got re-emit %+v", again)
	}
}

// TestDurabilityChurnBeforeWindow: a later commit that rewrites one AI line
// churns exactly that line (emitted at commit time); the surviving lines still
// go durable at 30d.
func TestDurabilityChurnBeforeWindow(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "base.txt", "base\n")
	git("add", "-A")
	git("commit", "-m", "base")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}

	recordAiTouchedPath("sess-dur", key, "ai.go")
	writeCommitFile(t, ws, "ai.go", "l1\nl2\nl3\n")
	git("add", "-A")
	git("commit", "-m", "ai adds ai.go")
	sha1 := gitOut("rev-parse", "HEAD")

	const t0 int64 = 1_000_000_000_000
	pollDurabilityCommit(ws, key, sess, sha1, t0)

	// A human rewrites line 2 four days later (no AI evidence for this commit).
	writeCommitFile(t, ws, "ai.go", "l1\nCHANGED\nl3\n")
	git("add", "-A")
	git("commit", "-m", "human rewrites line 2")
	sha2 := gitOut("rev-parse", "HEAD")

	churn := pollDurabilityCommit(ws, key, sess, sha2, t0+4*dayMs)
	cdata := durVerdictFor(t, churn, "ai.go")
	churned := rangeSet(t, cdata, "churnedRanges")
	if !churned["2..2"] {
		t.Errorf("churnedRanges = %+v, want line 2 churned", churned)
	}

	// At 30d, the untouched lines 1 and 3 are durable; line 2 is gone.
	v := harvestDurable(sess, ws, key, t0+31*dayMs)
	data := durVerdictFor(t, v, "ai.go")
	durable := rangeSet(t, data, "durableRanges")
	if !durable["1..1"] || !durable["3..3"] {
		t.Errorf("durableRanges = %+v, want 1..1 and 3..3 (line 2 churned)", durable)
	}
}

// TestDurabilityInsertionShiftsSurvivor: an unrelated insertion ABOVE an AI range
// shifts the range's reported line numbers but keeps it durable — the interval
// remap, done with no per-line git spawn.
func TestDurabilityInsertionShiftsSurvivor(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "ai.go", "a1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "base with ai.go")
	// The base commit's ai.go is the AI content.
	recordAiTouchedPath("sess-dur", gitWatchRootKey(ws), "ai.go")
	sha1 := gitOut("rev-parse", "HEAD")

	key := gitWatchRootKey(ws)
	sess := Session{DeviceID: "dev", TaskRoot: ws}
	const t0 int64 = 1_000_000_000_000
	pollDurabilityCommit(ws, key, sess, sha1, t0)

	// Insert two lines at the very top (human), pushing the AI lines to 3..5.
	writeCommitFile(t, ws, "ai.go", "new0\nnew00\na1\na2\na3\n")
	git("add", "-A")
	git("commit", "-m", "insert two lines above")
	sha2 := gitOut("rev-parse", "HEAD")
	pollDurabilityCommit(ws, key, sess, sha2, t0+dayMs)

	// The AI lines survived, now reported at 3..5, and go durable at 30d.
	v := harvestDurable(sess, ws, key, t0+31*dayMs)
	data := durVerdictFor(t, v, "ai.go")
	durable := rangeSet(t, data, "durableRanges")
	if !durable["3..5"] {
		t.Errorf("durableRanges = %+v, want shifted 3..5", durable)
	}
}
