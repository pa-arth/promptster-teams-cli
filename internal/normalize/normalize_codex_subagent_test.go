package normalize

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// Real session_meta headers captured from codex-cli 0.146.0, trimmed of their
// (very large) base_instructions blob. ONE hour of one engineer's work, split by
// Codex across two rollout files: the thread the human types into, and the
// subagent thread it delegates to. Note the ids — the subagent's `id` (and so
// its FILENAME uuid, which is where the watcher seeds the session id from) is
// its own, while `session_id`/`parent_thread_id` name the conversation both
// threads belong to.
const (
	codexParentThreadID = "019fb396-91d9-7770-bcc0-329fcedfa8e0"
	codexSubThreadID    = "019fb396-9280-7710-91d4-f36e7797376b"

	codexParentMeta = `{"timestamp":"2026-07-30T15:13:59.762Z","type":"session_meta","payload":{"session_id":"` + codexParentThreadID + `","id":"` + codexParentThreadID + `","timestamp":"2026-07-30T15:13:37.286Z","cwd":"/tmp/ws","originator":"codex-tui","cli_version":"0.146.0","source":"cli","thread_source":"user","model_provider":"openai"}}`

	codexSubagentMeta = `{"timestamp":"2026-07-30T15:17:08.541Z","type":"session_meta","payload":{"session_id":"` + codexParentThreadID + `","id":"` + codexSubThreadID + `","parent_thread_id":"` + codexParentThreadID + `","timestamp":"2026-07-30T15:13:37.466Z","cwd":"/tmp/ws","originator":"codex-tui","source":{"subagent":{"other":"guardian"}},"thread_source":"subagent","model_provider":"openai"}}`
)

// The human's thread: the prompt lives here, and so therefore does workdir.
var codexParentLines = []string{
	codexParentMeta,
	`{"timestamp":"2026-07-30T15:14:02.000Z","type":"event_msg","payload":{"type":"user_message","message":"fix the retry backoff","images":[]}}`,
	`{"timestamp":"2026-07-30T15:14:40.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Delegated.","phase":"final_answer"}}`,
}

// The delegated thread: all the WORK lives here, and not one user_message the
// human wrote.
var codexSubagentLines = []string{
	codexSubagentMeta,
	`{"timestamp":"2026-07-30T15:17:09.000Z","type":"event_msg","payload":{"type":"user_message","message":"You are judging one planned coding-agent action.","images":[]}}`,
	`{"timestamp":"2026-07-30T15:17:10.000Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call_S1","success":true,"changes":{"/tmp/ws/retry.go":{"type":"update","unified_diff":"@@ -1,3 +1,3 @@\n a\n-b\n+b2\n c\n","move_path":null}},"status":"completed"}}`,
	`{"timestamp":"2026-07-30T15:17:11.000Z","type":"response_item","payload":{"type":"function_call","name":"read_file","arguments":"{\"path\":\"retry.go\"}","call_id":"call_S2"}}`,
	`{"timestamp":"2026-07-30T15:17:11.500Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_S2","output":"ok"}}`,
	`{"timestamp":"2026-07-30T15:17:12.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":17718,"cached_input_tokens":0,"output_tokens":237,"reasoning_output_tokens":0,"total_tokens":17955}}}}`,
	`{"timestamp":"2026-07-30T15:17:13.000Z","type":"event_msg","payload":{"type":"agent_message","message":"allow","phase":"final_answer"}}`,
}

// run replays one rollout the way the watcher does: a processor seeded from the
// FILENAME uuid (codexSessionIDFromPath), fed the file line by line.
func runCodexRollout(t *testing.T, filenameUUID string, lines []string) []event.Event {
	t.Helper()
	p := NewCodexRolloutProcessor(filenameUUID)
	p.RepoRoot = "acme/thing"
	p.RepoHost = "github.com"
	p.RepoTracked = true
	var events []event.Event
	for _, line := range lines {
		events = append(events, p.Process([]byte(line))...)
	}
	return events
}

func codexData(e event.Event) map[string]interface{} {
	m, _ := e.Data.(map[string]interface{})
	return m
}

// TestCodexSubagentThreadJoinsParentSession is the contract the production split
// violated: a Codex conversation is ONE session no matter how many threads Codex
// spreads it over. Before the fix the delegated thread was stamped with its own
// filename uuid, so the engineer's prompts and the work done on their behalf
// landed in two disjoint backend sessions — the judge then graded prompts with
// no visible work and work with no visible intent, and repo attribution (which
// rides only on prompt events) was lost on all the work.
func TestCodexSubagentThreadJoinsParentSession(t *testing.T) {
	parent := runCodexRollout(t, codexParentThreadID, codexParentLines)
	sub := runCodexRollout(t, codexSubThreadID, codexSubagentLines)

	if len(parent) == 0 || len(sub) == 0 {
		t.Fatalf("no events: parent=%d subagent=%d", len(parent), len(sub))
	}
	for _, e := range parent {
		if e.SessionID != codexParentThreadID {
			t.Fatalf("parent event kind=%s sessionId=%q, want %q", e.Kind, e.SessionID, codexParentThreadID)
		}
	}
	for _, e := range sub {
		if e.SessionID != codexParentThreadID {
			t.Errorf("subagent event kind=%s sessionId=%q, want the conversation %q — delegated work must roll up, not fork a session",
				e.Kind, e.SessionID, codexParentThreadID)
		}
	}
}

// TestCodexConversationCarriesBothPromptsAndWork states the customer-visible
// symptom directly: one conversation, one session id, holding BOTH the prompts
// (with workdir/repoRoot) and the file_diff/tool work.
func TestCodexConversationCarriesBothPromptsAndWork(t *testing.T) {
	all := append(
		runCodexRollout(t, codexParentThreadID, codexParentLines),
		runCodexRollout(t, codexSubThreadID, codexSubagentLines)...,
	)

	kindsBySession := map[string]map[string]int{}
	for _, e := range all {
		if kindsBySession[e.SessionID] == nil {
			kindsBySession[e.SessionID] = map[string]int{}
		}
		kindsBySession[e.SessionID][e.Kind]++
	}
	if len(kindsBySession) != 1 {
		t.Fatalf("one Codex conversation produced %d sessions (%v), want 1", len(kindsBySession), kindsBySession)
	}
	kinds := kindsBySession[codexParentThreadID]
	for _, want := range []string{"prompt", "file_diff", "tool_use"} {
		if kinds[want] == 0 {
			t.Errorf("session has no %s events (got %v) — prompts and work must share one session", want, kinds)
		}
	}

	var sawWorkdir bool
	for _, e := range all {
		if e.Kind != "prompt" {
			continue
		}
		d := codexData(e)
		if d["workdir"] == "~/ws" || d["repoRoot"] == "acme/thing" {
			sawWorkdir = true
		}
	}
	if !sawWorkdir && kinds["prompt"] > 0 {
		t.Errorf("no prompt in the merged session carries workdir/repoRoot — repo attribution is still lost")
	}
}

// TestCodexSubagentPromptsAreNotHumanPrompts guards the regression the merge
// would otherwise introduce. A delegated thread's user_message is the
// orchestrator's instruction, not something a person typed; once the thread
// rolls up into the human's real session, emitting those as prompts would
// inflate the engineer's prompt count with machine-authored text the fluency
// judge then grades as their own prompting. (Local codex 0.146 produced 41 such
// synthetic messages in a single hour.) Same rule the Claude sidechain path
// already enforces.
func TestCodexSubagentPromptsAreNotHumanPrompts(t *testing.T) {
	sub := runCodexRollout(t, codexSubThreadID, codexSubagentLines)
	for _, e := range sub {
		if e.Kind == "prompt" {
			t.Errorf("delegated thread emitted a prompt event (%v) — subagent instructions are not human prompts", codexData(e)["text"])
		}
		if e.Kind == "ai_response" {
			t.Errorf("delegated thread emitted an ai_response — its prose is written to its orchestrator, not to the human")
		}
	}
}

// TestCodexSubagentSpendSurvivesAsUsage: dropping the prose must not drop the
// COST. A subagent burns real tokens on the engineer's behalf and they belong to
// the conversation that spawned it.
func TestCodexSubagentSpendSurvivesAsUsage(t *testing.T) {
	sub := runCodexRollout(t, codexSubThreadID, codexSubagentLines)
	var found bool
	for _, e := range sub {
		if e.Kind != "subagent_usage" {
			continue
		}
		found = true
		d := codexData(e)
		if d["inputTokens"] != int64(17718) || d["outputTokens"] != int64(237) {
			t.Errorf("subagent_usage tokens = in:%v out:%v, want in:17718 out:237", d["inputTokens"], d["outputTokens"])
		}
		if d["agentId"] != codexSubThreadID {
			t.Errorf("subagent_usage agentId = %v, want the thread id %q", d["agentId"], codexSubThreadID)
		}
		if d["sidechain"] != true {
			t.Errorf("subagent_usage missing the sidechain marker: %v", d)
		}
		if _, leaked := d["lastAssistantMessage"]; leaked {
			t.Errorf("subagent_usage carries agent prose")
		}
	}
	if !found {
		t.Errorf("delegated thread emitted no subagent_usage — its spend went missing")
	}
}

// TestCodexSubagentCacheWriteSurvives: a delegate pays the GPT-5.6 cache-write
// fee (1.25x the uncached input rate, GA 2026-07-09) exactly as the main chain
// does, so the count has to reach the backend from THIS emitter too. It rides a
// different path than ai_response on both legs — subagentUsage builds its own
// data map, and the projector allowlists the kind by its own entry rather than
// through projectUsageFields — so the main-chain test passing says nothing about
// this one. A delegated turn whose write count is dropped prices at $0 and looks
// identical to a delegate that never wrote to cache.
//
// Its own fixture rather than a cache_write field added to codexSubagentLines:
// that slice is the pre-5.6 capture those other tests assert real numbers
// against, and a 5.6-era field does not belong in it.
func TestCodexSubagentCacheWriteSurvives(t *testing.T) {
	lines := []string{
		codexSubagentMeta,
		`{"timestamp":"2026-07-30T15:17:12.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":17718,"cached_input_tokens":9000,"cache_write_input_tokens":4200,"output_tokens":237,"total_tokens":17955}}}}`,
		`{"timestamp":"2026-07-30T15:17:13.000Z","type":"event_msg","payload":{"type":"agent_message","message":"allow","phase":"final_answer"}}`,
	}
	var found bool
	for _, e := range runCodexRollout(t, codexSubThreadID, lines) {
		if e.Kind != "subagent_usage" {
			continue
		}
		found = true
		d := codexData(e)
		if d["cacheWriteInputTokens"] != int64(4200) {
			t.Errorf("subagent_usage cacheWriteInputTokens = %v, want int64(4200) — a delegate's cache writes are billed too", d["cacheWriteInputTokens"])
		}
		// The Anthropic addend key must stay clear here as well: a Codex delegate
		// row carrying it would be priced beside inputTokens rather than out of it.
		if _, present := d["cacheWriteTokens"]; present {
			t.Errorf("cacheWriteTokens present (%v) on a Codex subagent row; that key is the Anthropic addend", d["cacheWriteTokens"])
		}
		if d["inputTokens"] != int64(17718) {
			t.Errorf("inputTokens = %v, want int64(17718) — the INCLUSIVE total both cache counts are carved out of", d["inputTokens"])
		}
	}
	if !found {
		t.Errorf("delegated thread emitted no subagent_usage — its cache-write spend went missing")
	}
}

// TestCodexOneSessionStartPerConversation: parent and subagent each open with
// their own session_meta, but one logical session begins exactly once. The
// deterministic id must collapse them rather than stack N starts on one session.
func TestCodexOneSessionStartPerConversation(t *testing.T) {
	parent := runCodexRollout(t, codexParentThreadID, codexParentLines)
	sub := runCodexRollout(t, codexSubThreadID, codexSubagentLines)

	ids := map[string]bool{}
	for _, e := range append(parent, sub...) {
		if e.Kind == "session_start" {
			ids[e.ID] = true
		}
	}
	if len(ids) != 1 {
		t.Errorf("one conversation produced %d distinct session_start ids, want 1 (the backend cannot collapse the rest)", len(ids))
	}
}

// TestCodexConversationID pins the id-resolution order on its own, including the
// pre-0.145 shape that carries only `id` and must keep resolving exactly as it
// always did.
func TestCodexConversationID(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    string
	}{
		{
			name:    "legacy rollout: only the thread's own id",
			payload: map[string]interface{}{"id": "thread-a"},
			want:    "thread-a",
		},
		{
			name:    "user thread: session_id equals its own id",
			payload: map[string]interface{}{"id": "thread-a", "session_id": "thread-a"},
			want:    "thread-a",
		},
		{
			name:    "subagent: the conversation root wins over the thread id",
			payload: map[string]interface{}{"id": "thread-b", "session_id": "thread-a", "parent_thread_id": "thread-a"},
			want:    "thread-a",
		},
		{
			name:    "nested subagent: the ROOT wins over the immediate parent",
			payload: map[string]interface{}{"id": "thread-c", "session_id": "thread-a", "parent_thread_id": "thread-b"},
			want:    "thread-a",
		},
		{
			name:    "parentage without a root: fall back to the parent",
			payload: map[string]interface{}{"id": "thread-b", "parent_thread_id": "thread-a"},
			want:    "thread-a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodexConversationID(tc.payload); got != tc.want {
				t.Errorf("CodexConversationID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCodexUserThreadUnaffected: the fix must not touch an ordinary single-thread
// rollout — the overwhelming majority of the fleet. Its prompts stay prompts and
// its session id stays the filename uuid.
func TestCodexUserThreadUnaffected(t *testing.T) {
	events := runCodexRollout(t, codexParentThreadID, codexParentLines)
	kinds := map[string]int{}
	for _, e := range events {
		kinds[e.Kind]++
		if e.SessionID != codexParentThreadID {
			t.Errorf("kind=%s sessionId=%q, want %q", e.Kind, e.SessionID, codexParentThreadID)
		}
	}
	if kinds["prompt"] != 1 || kinds["ai_response"] != 1 || kinds["session_start"] != 1 {
		t.Errorf("user thread kinds = %v, want 1 each of prompt/ai_response/session_start", kinds)
	}
	for _, e := range events {
		if e.Kind == "prompt" && e.Actor.Type != "human" {
			t.Errorf("human prompt actor = %q, want human", e.Actor.Type)
		}
	}
}
