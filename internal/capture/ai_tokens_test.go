package capture

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// aiTokensOf pulls the scalar aiTokens field out of a commit_attribution event's
// projected Data, failing if it is missing or not a number. After the JSON
// round-trip through eventDataMap the int arrives as float64.
func aiTokensOf(t *testing.T, ev event.Event) int {
	t.Helper()
	data, ok := ev.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("event Data is %T, want map", ev.Data)
	}
	v, present := data["aiTokens"]
	if !present {
		t.Fatalf("aiTokens missing from commit_attribution payload: %+v", data)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("aiTokens is %T, want a number: %+v", v, v)
	}
	return int(f)
}

// TestCountTiktokenTokensOfflineDeterministic proves the o200k encoder is
// OFFLINE (embedded BPE, no runtime fetch) and DETERMINISTIC. An unreachable
// HTTP(S) proxy plus an empty cache dir guarantee that any network attempt would
// fail — so a stable, positive count for a fixed string can only come from the
// embedded ranks. The same input must always yield the same count.
func TestCountTiktokenTokensOfflineDeterministic(t *testing.T) {
	// If the encoder tried the network these would force a hard failure.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("TIKTOKEN_CACHE_DIR", t.TempDir()) // no cached blob to fall back on

	const text = "func main() {\n\tfmt.Println(\"hello, world\")\n}"
	first := countTiktokenTokens(text)
	if first <= 0 {
		t.Fatalf("countTiktokenTokens returned %d for non-empty text — offline o200k init failed", first)
	}
	if again := countTiktokenTokens(text); again != first {
		t.Errorf("non-deterministic count: %d then %d", first, again)
	}
	// The empty string is zero tokens, never a network trip.
	if n := countTiktokenTokens(""); n != 0 {
		t.Errorf("empty string tokens = %d, want 0", n)
	}
}

// TestCommitAiTokensLikelyAIOnly: only likely_ai added lines feed the count.
// unknown lines and lines from a file with no matching diff text contribute
// nothing. The count equals countTiktokenTokens of the newline-joined likely_ai
// added lines.
func TestCommitAiTokensLikelyAIOnly(t *testing.T) {
	diff := "diff --git a/ai.go b/ai.go\n" +
		"--- /dev/null\n" +
		"+++ b/ai.go\n" +
		"@@ -0,0 +1,3 @@\n" +
		"+package main\n" +
		"+\n" +
		"+func main() {}\n" +
		"diff --git a/hand.go b/hand.go\n" +
		"--- /dev/null\n" +
		"+++ b/hand.go\n" +
		"@@ -0,0 +1,1 @@\n" +
		"+var x = 1\n"

	files := []attrFile{
		{Path: "ai.go", LineRanges: []attrLineRange{{Start: 1, End: 3, Attribution: attributionLikelyAI}}},
		{Path: "hand.go", LineRanges: []attrLineRange{{Start: 1, End: 1, Attribution: attributionUnknown}}},
	}

	want := countTiktokenTokens("package main\n\nfunc main() {}")
	if want <= 0 {
		t.Fatalf("fixture want count = %d, expected > 0", want)
	}
	got := commitAiTokens(diff, files)
	if got != want {
		t.Errorf("commitAiTokens = %d, want %d (only the likely_ai lines from ai.go)", got, want)
	}
}

// TestCommitAiTokensNoAILines: a commit whose changed lines are all unknown
// yields aiTokens == 0.
func TestCommitAiTokensNoAILines(t *testing.T) {
	diff := "diff --git a/hand.go b/hand.go\n" +
		"--- /dev/null\n" +
		"+++ b/hand.go\n" +
		"@@ -0,0 +1,2 @@\n" +
		"+var x = 1\n" +
		"+var y = 2\n"
	files := []attrFile{
		{Path: "hand.go", LineRanges: []attrLineRange{{Start: 1, End: 2, Attribution: attributionUnknown}}},
	}
	if got := commitAiTokens(diff, files); got != 0 {
		t.Errorf("commitAiTokens with no likely_ai lines = %d, want 0", got)
	}
}
