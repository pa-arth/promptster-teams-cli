package normalize

import (
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
)

// The emitter and the on-device projector are two default-deny surfaces that
// must agree: a field stamped by normalize but missing from
// redact.projectFieldAllowlist is stripped silently before signing, and arrives
// at the backend looking exactly like an older CLI that never emitted it. That
// is the failure this whole change exists to remove, so it is pinned end-to-end
// rather than per-package — a unit test on either side alone would stay green
// while the field vanished on the wire.
//
// (There is a THIRD allowlist, the backend's packages/shared/eventFieldProjection.ts.
// Nothing in this repo can test it; it is the reason the backend must deploy
// before any CLI tag.)

// TestRepoTrackedFalseSurvivesProjection: `false` is the value that matters. A
// projector that skipped falsy values — or an allowlist that never gained the
// key — would drop exactly the observation the change adds, and drop it into the
// one state ("absent") that reads as "we did not look".
func TestRepoTrackedFalseSurvivesProjection(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-proj-false")
	p.RepoRoot = "1e06276a18347f93" // opaque hash: a container folder, not a repo
	p.RepoTracked = false
	events := processAll(t, p,
		`{"type":"user","message":{"role":"user","content":"add a retry"},"timestamp":"2026-06-10T10:00:00.000Z","cwd":"/tmp/ws","sessionId":"ide-1","promptSource":"typed","uuid":"u-proj-false"}`,
	)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	redact.ProjectEvent(&e, false)

	d, ok := e.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("projected Data is %T, not a map", e.Data)
	}
	got, present := d["repoTracked"]
	if !present {
		t.Fatal("repoTracked did not survive projection — add it to redact.projectFieldAllowlist[\"prompt\"]")
	}
	if got != false {
		t.Errorf("projected repoTracked = %v, want false", got)
	}
	// The key it describes must survive with it, byte-identical.
	if d["repoRoot"] != "1e06276a18347f93" {
		t.Errorf("projected repoRoot = %v, want the hash key unchanged", d["repoRoot"])
	}
}

// TestRepoTrackedTrueSurvivesProjection: the true case, both lanes, so a
// codex-side drift in either the emitter or the allowlist is caught too.
func TestRepoTrackedTrueSurvivesProjection(t *testing.T) {
	p := NewCodexRolloutProcessor("sess-proj-true")
	p.RepoRoot = "acme/foo"
	p.RepoHost = "github.com"
	p.RepoTracked = true
	var prompt *event.Event
	for _, l := range []string{
		`{"timestamp":"2026-06-06T20:38:45.965Z","type":"session_meta","payload":{"id":"s","cwd":"/tmp/ws","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-06T20:38:47.624Z","type":"event_msg","payload":{"type":"user_message","message":"do the thing"}}`,
	} {
		for _, e := range p.Process([]byte(l)) {
			if e.Kind == "prompt" {
				ev := e
				prompt = &ev
			}
		}
	}
	if prompt == nil {
		t.Fatal("no prompt event emitted")
	}
	redact.ProjectEvent(prompt, false)

	d, ok := prompt.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("projected Data is %T, not a map", prompt.Data)
	}
	if got, present := d["repoTracked"]; !present || got != true {
		t.Errorf("projected repoTracked = %v (present %v), want true", got, present)
	}
	if d["repoRoot"] != "acme/foo" || d["repoHost"] != "github.com" {
		t.Errorf("projected identity = (%v, %v), want (acme/foo, github.com)", d["repoRoot"], d["repoHost"])
	}
}

// TestRepoTrackedAbsentSurvivesAsAbsent: the old-CLI / unresolvable-cwd state.
// Projection must not manufacture a value — `false` here would be a positive
// claim that the directory is not a working tree, made on no observation.
func TestRepoTrackedAbsentSurvivesAsAbsent(t *testing.T) {
	p := NewClaudeTranscriptProcessor("sess-proj-absent")
	events := processAll(t, p,
		`{"type":"user","message":{"role":"user","content":"add a retry"},"timestamp":"2026-06-10T10:00:00.000Z","cwd":"/tmp/ws","sessionId":"ide-1","promptSource":"typed","uuid":"u-proj-absent"}`,
	)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	redact.ProjectEvent(&e, false)

	d, _ := e.Data.(map[string]interface{})
	if _, present := d["repoTracked"]; present {
		t.Errorf("repoTracked must stay ABSENT through projection, got %v", d["repoTracked"])
	}
	if _, present := d["repoRoot"]; present {
		t.Errorf("repoRoot must stay ABSENT through projection, got %v", d["repoRoot"])
	}
}
