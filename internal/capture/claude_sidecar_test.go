package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// Claude Code writes a four-key sidecar beside every sidechain transcript. The
// shape below is copied from a real one; `description` is the parent's Task
// `description`, which the CLI already emits verbatim as task_dispatch.summary.
//
// Present on 143/143 sidechains across 53 sessions of real capture, 20-50
// chars, and distinct within every one of the 21 clusters of concurrent
// same-type lanes — which is the point, since the type name is identical across
// such a cluster by definition.
const realSidecar = `{"agentType":"Explore","description":"Scout backend ingest contract","toolUseId":"toolu_01RmfPFBaayGJuQQHGmWBGLx","spawnDepth":1}`

func writeSidechain(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sess", "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "agent-aaa9a3a70c738c722.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		meta := filepath.Join(dir, "agent-aaa9a3a70c738c722.meta.json")
		if err := os.WriteFile(meta, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return transcript
}

func TestClaudeSidecarYieldsTheDispatchLabel(t *testing.T) {
	got := claudeSidecarDescription(writeSidechain(t, realSidecar))
	if got != "Scout backend ingest contract" {
		t.Fatalf("description = %q, want the dispatch label", got)
	}
}

// Absent means "this lane did not tell us", which is NOT the same as a lane
// with no purpose — so every failure path yields "" and the caller omits the
// key rather than stamping a placeholder.
func TestClaudeSidecarIsAbsentNotEmptyOnEveryFailure(t *testing.T) {
	for name, body := range map[string]string{
		"no sidecar at all":   "",
		"not json":            "agentType: Explore",
		"truncated mid-write": `{"agentType":"Explore","descrip`,
		"description absent":  `{"agentType":"Explore","toolUseId":"toolu_01","spawnDepth":1}`,
		"description empty":   `{"agentType":"Explore","description":""}`,
	} {
		if got := claudeSidecarDescription(writeSidechain(t, body)); got != "" {
			t.Fatalf("%s: got %q, want \"\" so the caller omits the key", name, got)
		}
	}
}

// ONLY `description` is read. toolUseId would need its own allowlist row on
// both sides and answers a question the description already answers in words;
// agentType already rides the rows as attributionAgent, and a second source for
// one field is a second chance to disagree.
func TestClaudeSidecarReadsNothingButTheDescription(t *testing.T) {
	got := claudeSidecarDescription(writeSidechain(t, realSidecar))
	for _, leaked := range []string{"toolu_", "Explore", "spawnDepth"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("sidecar read pulled %q into the label: %q", leaked, got)
		}
	}
}

// THE CALL SITE. claudeSidecarDescription's unit tests above all passed with
// the line that calls it deleted — a library with no caller is green and inert
// on every real transcript. This asserts the processor the watcher actually
// builds for a sidechain path carries the label.
func TestWatcherWiresTheSidecarLabelIntoTheProcessor(t *testing.T) {
	proc := newClaudeProcessorForPath(writeSidechain(t, realSidecar))
	if !proc.UsageOnly {
		t.Fatal("a sidechain processor must be counters-only")
	}
	if proc.Summary != "Scout backend ingest contract" {
		t.Fatalf("proc.Summary = %q, want the sidecar's description", proc.Summary)
	}
	if proc.AgentID != "aaa9a3a70c738c722" {
		t.Fatalf("proc.AgentID = %q, want the lane id beside the label", proc.AgentID)
	}
}

// The main chain has no dispatch behind it and must not acquire one.
func TestWatcherLeavesTheMainChainUnlabelled(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "2677c24e-0000-4000-8000-000000000000.jsonl")
	if err := os.WriteFile(main, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proc := newClaudeProcessorForPath(main)
	if proc.UsageOnly {
		t.Fatal("the main chain must not be counters-only")
	}
	if proc.Summary != "" {
		t.Fatalf("main chain acquired a dispatch label: %q", proc.Summary)
	}
}

// THE RACE. The sidecar is a separate file from the transcript, so the watcher
// can discover one before the other exists: measured on 145 real sidechains,
// the `.meta.json` was born AFTER its `.jsonl` on 25 of them (17.2%), the
// closest by 10ms. A processor is built once and lives for the whole watcher
// run, so a construction-time read that loses that race caches "" forever and
// every subsequent subagent_usage on the lane ships unlabelled — which reads
// downstream as "this lane did not tell us", not as "we looked too early".
//
// Poll with no sidecar, write it, poll again: the label must arrive.
func TestSidecarWrittenAfterTheTranscriptStillLandsOnTheLane(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	dir := filepath.Join(root, "-Users-me-repo", "2677c24e-0000-4000-8000-000000000000", "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "agent-aaa9a3a70c738c722.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pre-decide the match so this test exercises the processor path, not the
	// classifier — a sidechain carries no cwd line of its own.
	key := claudeProgressKey(transcript)
	saveClaudeWatchProgress(claudeWatchProgress{
		Offsets: map[string]int64{},
		Match:   map[string]string{key: "yes"},
		// Fingerprint the same root set the poll will compute, or the cache-sync
		// drops the seeded decision and no processor is ever built.
		RootsFP: captureRootsFingerprint(workspaceMatchRoots(resolvePath(workspace))),
		V:       claudeProgressSchemaV,
	})

	session := Session{DeviceID: "sess-race", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	cutoff := session.StartedAt.Add(-2 * time.Minute)
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}

	pollClaudeTranscripts(session, resolvePath(workspace), cutoff, processors, true, false)
	proc := processors[key]
	if proc == nil {
		t.Fatal("no processor built for the sidechain")
	}
	if proc.Summary != "" {
		t.Fatalf("label appeared before the sidecar existed: %q", proc.Summary)
	}

	// The sidecar lands a moment later, as it does on 17% of real lanes.
	meta := filepath.Join(dir, "agent-aaa9a3a70c738c722.meta.json")
	if err := os.WriteFile(meta, []byte(realSidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	pollClaudeTranscripts(session, resolvePath(workspace), cutoff, processors, true, false)
	if processors[key] != proc {
		t.Fatal("processor was rebuilt; this test no longer exercises the retry")
	}
	if proc.Summary != "Scout backend ingest contract" {
		t.Fatalf("proc.Summary = %q — the lane stayed unlabelled after its sidecar arrived", proc.Summary)
	}
}
