package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// The response_item copy of a human turn ALWAYS precedes the authoritative
// event_msg by one line — verified 25/25 on real local rollouts, at line N then
// N+1. Every fixture here preserves that order, because it is the ordering that
// forced the deferred-emit design: at the moment the response_item arrives, "has
// an event_msg been seen?" is always "not yet".
const (
	riUser = `{"timestamp":"2026-07-30T16:38:14.000Z","type":"response_item",` +
		`"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%s}]}}`
	emUser = `{"timestamp":"2026-07-30T16:38:15.000Z","type":"event_msg",` +
		`"payload":{"type":"user_message","message":%s}}`
	riAssistant = `{"timestamp":"2026-07-30T16:38:16.000Z","type":"response_item",` +
		`"payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working on it"}]}}`
	turnCtx = `{"timestamp":"2026-07-30T16:38:17.000Z","type":"turn_context",` +
		`"payload":{"model":"gpt-5.2-codex"}}`
)

func q(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func drivePrompts(t *testing.T, lines []string) []event.Event {
	t.Helper()
	p := NewCodexRolloutProcessor("sess-fallback")
	var prompts []event.Event
	for _, line := range lines {
		for _, e := range p.Process([]byte(line)) {
			if e.Kind == "prompt" {
				prompts = append(prompts, e)
			}
		}
	}
	return prompts
}

func promptTexts(events []event.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		d, _ := e.Data.(map[string]interface{})
		text, _ := d["text"].(string)
		out = append(out, text)
	}
	return out
}

// The load-bearing guarantee: on every host that emits event_msg — which is
// every rollout we can observe locally — this fallback changes NOTHING. If this
// fails, the fix has started double-counting real customers' prompts, which is
// strictly worse than the bug it fixes.
func TestCodexResponseItemUserIsNotDoubleCountedWhenEventMsgFollows(t *testing.T) {
	human := "Read PROBLEM.md and fix the failing middleware test"
	prompts := drivePrompts(t, []string{
		strings.Replace(riUser, "%s", q(human), 1),
		strings.Replace(emUser, "%s", q(human), 1),
		riAssistant,
	})

	if len(prompts) != 1 {
		t.Fatalf("got %d prompts %q, want exactly 1 — the response_item copy must be discarded",
			len(prompts), promptTexts(prompts))
	}
	if got := promptTexts(prompts)[0]; got != human {
		t.Errorf("prompt text = %q, want %q", got, human)
	}
}

// The bug this exists for: opsAI's rollouts carry the human turn on the
// response_item channel with no event_msg behind it, and the normalizer skipped
// that channel as a duplicate — so 29 sessions of real work captured ZERO human
// prompts.
func TestCodexRecoversUserPromptWhenEventMsgAbsent(t *testing.T) {
	human := "why is the dedup branch dropping meta.children?"
	prompts := drivePrompts(t, []string{
		strings.Replace(riUser, "%s", q(human), 1),
		// No event_msg. The next line is what proves the turn moved on.
		riAssistant,
	})

	if len(prompts) != 1 {
		t.Fatalf("got %d prompts %q, want 1 recovered from the response_item",
			len(prompts), promptTexts(prompts))
	}
	got := prompts[0]
	gd, _ := got.Data.(map[string]interface{})
	if text, _ := gd["text"].(string); text != human {
		t.Errorf("prompt text = %q, want %q", text, human)
	}
	if got.Provenance == nil || got.Provenance.Attribution != "likely_human" {
		t.Errorf("recovered prompt must carry human provenance, got %+v", got.Provenance)
	}
}

// A turn_context or any other line is just as valid a "the turn moved on"
// signal as an assistant message — the flush must not depend on which.
func TestCodexRecoveredPromptFlushesOnAnyFollowingLine(t *testing.T) {
	prompts := drivePrompts(t, []string{
		strings.Replace(riUser, "%s", q("ship it"), 1),
		turnCtx,
	})
	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1 flushed by the following turn_context", len(prompts))
	}
}

// Unlike event_msg — which carries only what a person actually sent — the
// response_item user channel also carries Codex's own injected wrappers.
// Recovering prompts from it REQUIRES dropping them, or every session gains a
// phantom prompt made of the cwd header or the AGENTS.md file.
func TestCodexSyntheticUserWrappersNeverBecomePrompts(t *testing.T) {
	for _, synthetic := range []string{
		"<environment_context>\n  <cwd>/Users/dev/repo</cwd>\n</environment_context>",
		"<user_instructions>\nbe concise\n</user_instructions>",
		"<recommended_plugins>\nHere is a list of plugins\n</recommended_plugins>",
		"# AGENTS.md instructions for /Users/dev/repo\n\nAlways run the tests.",
	} {
		prompts := drivePrompts(t, []string{
			strings.Replace(riUser, "%s", q(synthetic), 1),
			riAssistant,
		})
		if len(prompts) != 0 {
			t.Errorf("synthetic wrapper became a prompt: %.40q", synthetic)
		}
	}
}

// Prefix-anchored, for the same reason the backend's NON_HUMAN_PREFIXES is: an
// engineer asking about a wrapper is still an engineer asking.
func TestCodexHumanPromptQuotingAWrapperSurvives(t *testing.T) {
	human := "why does <environment_context> keep showing up in my prompts?"
	prompts := drivePrompts(t, []string{
		strings.Replace(riUser, "%s", q(human), 1),
		riAssistant,
	})
	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1 — the drop must be prefix-anchored, not a substring match", len(prompts))
	}
}

// A delegated thread's user turns are the orchestrator's instructions to a
// subagent. They are dropped on the event_msg path for exactly this reason, and
// the fallback must not reopen the hole: it would inflate the engineer's prompt
// count with machine-authored text the fluency judge then grades as their own
// prompting.
func TestCodexSubagentThreadUserPromptsAreNotRecovered(t *testing.T) {
	meta := `{"timestamp":"2026-07-30T16:38:12.000Z","type":"session_meta",` +
		`"payload":{"id":"thread-b","session_id":"conv-a","parent_thread_id":"thread-a",` +
		`"thread_source":"subagent","cwd":"/Users/dev/repo"}}`
	prompts := drivePrompts(t, []string{
		meta,
		strings.Replace(riUser, "%s", q("Investigate the failing test and report back"), 1),
		riAssistant,
	})
	if len(prompts) != 0 {
		t.Errorf("subagent thread produced %d prompts %q, want 0", len(prompts), promptTexts(prompts))
	}
}

// A turn made only of images/attachments has no text to recover. It must not
// mint an empty prompt — opsAI already has 3 stored prompt rows with no text at
// all, and those are indistinguishable from a capture failure.
func TestCodexUserMessageWithNoTextPartsIsNotAPrompt(t *testing.T) {
	line := `{"timestamp":"2026-07-30T16:38:14.000Z","type":"response_item",` +
		`"payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"..."}]}}`
	prompts := drivePrompts(t, []string{line, riAssistant})
	if len(prompts) != 0 {
		t.Errorf("image-only turn produced %d prompts, want 0", len(prompts))
	}
}

// Recovered prompts must carry the same session metadata as event_msg ones, or
// a session whose prompts all came via the fallback silently loses its repo
// identity — and repoRoot is what joins telemetry to outcome_events.
func TestCodexRecoveredPromptStampsRepoIdentity(t *testing.T) {
	p := NewCodexRolloutProcessor("sess-meta")
	p.RepoRoot = "acme/api"
	p.RepoHost = "github.com"
	p.RepoTracked = true

	var prompts []event.Event
	for _, line := range []string{
		`{"timestamp":"2026-07-30T16:38:12.000Z","type":"session_meta","payload":{"id":"t1","cwd":"/Users/dev/repo"}}`,
		strings.Replace(riUser, "%s", q("add a retry to the webhook sender"), 1),
		riAssistant,
	} {
		for _, e := range p.Process([]byte(line)) {
			if e.Kind == "prompt" {
				prompts = append(prompts, e)
			}
		}
	}
	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1", len(prompts))
	}
	for key, want := range map[string]interface{}{
		"repoRoot":    "acme/api",
		"repoHost":    "github.com",
		"repoTracked": true,
	} {
		pd, _ := prompts[0].Data.(map[string]interface{})
		if got := pd[key]; got != want {
			t.Errorf("recovered prompt %s = %v, want %v", key, got, want)
		}
	}
}

// Multi-turn: alternating recovered and authoritative turns in one rollout must
// yield exactly one prompt each, in order, with no cross-contamination of the
// one-line buffer.
func TestCodexMixedRecoveredAndAuthoritativeTurns(t *testing.T) {
	prompts := drivePrompts(t, []string{
		strings.Replace(riUser, "%s", q("first turn"), 1),
		strings.Replace(emUser, "%s", q("first turn"), 1),
		riAssistant,
		strings.Replace(riUser, "%s", q("second turn"), 1),
		riAssistant,
		strings.Replace(riUser, "%s", q("third turn"), 1),
		strings.Replace(emUser, "%s", q("third turn"), 1),
		riAssistant,
	})
	want := []string{"first turn", "second turn", "third turn"}
	got := promptTexts(prompts)
	if len(got) != len(want) {
		t.Fatalf("got %d prompts %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("prompt[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
