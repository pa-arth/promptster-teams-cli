package capture

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// TestCodexFileDiffReachesAiPathsLedger (§10.3) is the end-to-end confirmation
// that a Codex AI edit reaches the SAME AI-paths ledger a Claude edit does — the
// join point the git-watch attribution and durability/rework engines read. The
// path is SOURCE-AGNOSTIC by design: dedupeFileDiff records any file_diff whose
// provenance is likely_ai, and the Codex normalizer stamps exactly that on a
// patch_apply_end. No Codex-specific plumbing exists or is needed; this test pins
// that so a future change to either side can't silently drop Codex from
// attribution.
func TestCodexFileDiffReachesAiPathsLedger(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	taskRoot := t.TempDir()

	// A real codex patch_apply_end rollout line → the Codex normalizer's file_diff.
	line := `{"timestamp":"2026-06-06T20:38:50.783Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call_A","success":true,"changes":{"pkg/target.go":{"type":"update","unified_diff":"@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"}},"status":"completed"}}`
	p := normalize.NewCodexRolloutProcessor("codex-sess")
	events := p.Process([]byte(line))

	var fileDiff *event.Event
	for i := range events {
		if events[i].Kind == "file_diff" {
			fileDiff = &events[i]
		}
	}
	if fileDiff == nil {
		t.Fatalf("codex patch_apply_end produced no file_diff: %+v", events)
	}
	// Precondition: the Codex normalizer marked it AI — the source-agnostic trigger
	// dedupeFileDiff keys the ledger record on.
	if fileDiff.Provenance == nil || fileDiff.Provenance.Attribution != "likely_ai" {
		t.Fatalf("codex file_diff provenance = %+v, want attribution likely_ai", fileDiff.Provenance)
	}

	// Feed it through the SAME dedup choke point every file_diff passes.
	dedupeFileDiff(taskRoot, fileDiff)

	// It must now be in the AI-paths ledger under this root, keyed to the codex
	// session — exactly as a Claude edit would be.
	rootKey := gitWatchRootKey(taskRoot)
	if got := readAiTouchedPaths(rootKey); got["pkg/target.go"] != "codex-sess" {
		t.Fatalf("readAiTouchedPaths = %+v, want pkg/target.go => codex-sess", got)
	}
}
