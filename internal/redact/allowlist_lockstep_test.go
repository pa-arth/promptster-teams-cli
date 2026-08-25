package redact

// LOCKSTEP, MACHINE-CHECKED.
//
// projectFieldAllowlist (this package) and CANONICAL_KIND_ALLOWLIST
// (promptster-backend packages/shared/src/captureAllowlist.ts) are two
// independent implementations of the same default-deny projection. Both files
// have said "LOCKSTEP" in prose since they were written, and until this test
// nothing performed it — the backend's header even claimed the Go mirror was
// "GENERATED from captureAllowlistManifest()", which no generator ever did.
//
// The cost of that: `promptLength` was allowlisted server-side and absent on the
// device for ten days. A field present on only one side is stripped SILENTLY —
// no error, no telemetry, no dropped-field metric — and reads to a user as
// "you're on an older CLI". There is no observation that distinguishes the two.
//
// So the backend now serializes its canonical surface to a JSON artifact, and
// this test diffs our table against a checked-in byte-copy of it.
//
// FOUR PROPERTIES, each designed for rather than assumed:
//
//  1. IT FAILS IN BOTH DIRECTIONS, naming the kind and the field. A field only
//     the server allows is a device gap; a field only the device sends is worse
//     — those bytes leave the machine and are discarded at ingest.
//
//  2. A MISSING OR CORRUPT ARTIFACT IS A FAILURE, NEVER A SKIP. `t.Skip` on an
//     absent testdata file is the classic disarmed guard: the cheapest way to
//     silence this test would be `rm testdata/*.json`, and that must be as loud
//     as a real divergence. The checksum covers hand-editing the artifact to go
//     green; the envelope-version pin covers a reshaped document being misparsed
//     into an empty allowlist — which a default-deny differ would call clean.
//
//  3. THE ASYMMETRY IS DECLARED, NOT ASSUMED AWAY. The two surfaces are
//     legitimately not identical: the CLI has no cost figure to send, no Codex
//     OTel session attributes to read, and does not emit every kind. Those are
//     enumerated below WITH REASONS. Anything not enumerated fails. Weakening
//     the comparison instead — "only diff fields both sides already have" —
//     would make this test structurally incapable of catching its own bug.
//
//  4. A STALE EXCEPTION FAILS. An exception that no longer describes a real
//     divergence is removed by this test failing, not by someone noticing. That
//     is the "what happens to this check after it succeeds?" question: an
//     exception list nobody prunes silently becomes a second allowlist.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The checked-in copy of promptster-backend's
// packages/shared/artifacts/capture-allowlist-canonical.json.
const captureAllowlistArtifactPath = "testdata/capture-allowlist-canonical.json"

// The ENVELOPE version the backend stamps — the document's structure, not its
// contents. Adding a kind does not change it; reshaping the document does. Any
// other value is refused rather than risk reading a new shape as an empty
// allowlist.
const captureAllowlistArtifactVersion = 1

// How to fix any failure in this file. Printed once per failing test, because a
// guard whose failure has no mechanical remedy gets suppressed instead of fixed.
const captureAllowlistFixHint = `
FIX (server first, always):
  1. promptster-backend:    pnpm gen:capture-manifest
  2. promptster-teams-cli:  make sync-capture-allowlist

If the divergence is DELIBERATE, declare it in this file with a reason — do not
widen either allowlist to make this pass. A field added to the DEVICE side that
the server does not allow leaves the machine and is discarded at ingest; one
added to the server that the device cannot populate is dead config. Ship order is
server-then-device: default-deny strips ahead-of-server fields silently, never
the reverse.`

// ---------------------------------------------------------------------------
// The declared asymmetry. Every entry below was verified against the emitters in
// internal/normalize and internal/capture on 2026-08-05, not inferred from the
// field name.
// ---------------------------------------------------------------------------

// kindsNotEmittedOnDevice — canonical kinds the server allowlists that this CLI
// never emits. If the CLI starts emitting one, its absence from
// projectFieldAllowlist means every event of that kind projects to {} — so this
// list doubles as the record of what must be added first.
var kindsNotEmittedOnDevice = map[string]string{
	"instructions_loaded": "Not a watcher kind. It is minted SERVER-side by the tier-B hook route " +
		"(apps/api/src/routes/team/teams-tier-b-hooks.ts), which classifies a memory-file load from " +
		"the hook payload. No code path in this repo constructs it.",
}

// serverOnlyFields — allowlisted at the backend write boundary and deliberately
// NOT on the device, keyed kind → field → reason.
//
// The legitimate shape of an entry is narrow: a field the device CANNOT source.
// "The device does not populate it yet" is a gap, and belongs in an issue rather
// than here — the whole point of this table is that it stays small enough to read.
//
// A field the device CAN source and deliberately does not is a DIFFERENT fact and
// lives in deviceDeclinesToEmit below. Filing one here would be the same class of
// error as a prose reason that outruns its evidence: it reads as a capability we
// lack, so nobody ever revisits it, and the open question is retired by filing
// rather than by deciding. The two tables are asserted disjoint.
var serverOnlyFields = map[string]map[string]string{
	"ai_response": {
		"costUsd": "The CLI has no cost figure to send — it emits tokens and the backend prices " +
			"them. This is the provider's own per-request number off the OTel wire " +
			"(claude_code.cost.usage), which no transcript carries. Kept out of USAGE_FIELDS " +
			"server-side for exactly this reason.",
		"speed": "No CLI rail sources it. `speed` is a Claude Code OTel `api_request` attribute; " +
			"no transcript, rollout or hook payload this CLI reads carries it. Cursor's " +
			"neighbouring `model_params` entry `{fast false}` is a DIFFERENT vocabulary — a " +
			"boolean about Cursor's own fast-mode, not Claude's speed token — and mapping one " +
			"onto the other would invent a fact. Unlike `effort` beside it, there is nothing " +
			"here to decide: the value does not exist on this side.",
		"cachedInputTokens": "Codex OTel only (`cached_input`). No Codex rail on this device reports " +
			"it: the rollout token_count this CLI parses carries no cached-input breakdown.",
		// cacheWriteInputTokens was here until 2026-08-12, filed as "Codex OTel only,
		// and server-side it is 0 on every corpus record." Both halves expired at the
		// GPT-5.6 GA (2026-07-09), which introduced a 1.25x-input cache-write fee: the
		// zeros were a fact about a corpus predating the charge, not about the rail,
		// and the device DOES source it — internal/normalize reads it off the rollout
		// token_count now. Worth noting how this exception aged, since the table is
		// full of ones that could age the same way: the reason was true when written
		// and said nothing about what would make it false. A "0 on every record"
		// justification is the fragile kind — it is evidence of absence only for as
		// long as nobody starts sending the field.
		"sseKind": "Codex OTel only — the SSE frame kind (`response.completed`). This CLI reads " +
			"completed rollout lines from disk; there are no SSE frames on the device rail.",
		"ttftMs": "Codex OTel only — per-request time to first token. A wire-timing measurement " +
			"that cannot be reconstructed from a rollout file after the fact.",
	},
	"api_request": {
		"durationMs": "This CLI emits no api_request event at all (grep: zero constructors in " +
			"internal/normalize and internal/capture). The kind's device entry exists to cover the " +
			"backend proxy rail; the OTel receiver is what populates this field.",
		"endpoint": "Same: no api_request emitter on this device. `endpoint` is the field that tells " +
			"codex.api_request (an HTTP call to /models) apart from claude_code.api_request (an " +
			"inference round-trip) — a distinction that only exists on the OTel wire.",
	},
	"plan_decision": {
		"fromMode": "OTel only — emitted by the claude_code.permission_mode_changed handler. This " +
			"CLI's plan_decision comes from an ExitPlanMode PostToolUse hook, which reports the " +
			"approve/reject choice and carries no mode transition.",
		"toMode":  "OTel only — see fromMode.",
		"trigger": "OTel only — see fromMode. (This CLI does emit `trigger` on context_compact, where both sides allowlist it.)",
	},
	"prompt": {
		"promptLength": "Deliberately not emitted, and the gap is one-sided by design. `promptLength` " +
			"exists server-side because a Codex OTel prompt with log_user_prompt=false ships " +
			"prompt=\"[REDACTED]\" and would otherwise persist as an empty row. This rail allowlists " +
			"`text` and sends the prompt itself, so there is nothing to fall back FROM. Emitting a " +
			"length here would also reverse a written position — normalize_claude_jsonl.go: \"No " +
			"timing, length, or paste signals are emitted — teams capture is content, not behavioral " +
			"analysis of the developer.\"",
	},
	"session_start": {
		// OPEN FINDING, recorded rather than fixed. This one is NOT a rail gap: the
		// Codex processor DOES set a `model` key on session_start
		// (normalize_codex.go — `"model": stringField(payload, "model_provider")`),
		// and this projection strips it before signing, so it never leaves the
		// machine. Allowlisting it would be the wrong fix, not the missing one: the
		// rollout session_meta carries `model_provider` ("openai") and no model id
		// at all, while the server documents this field as "the model the
		// conversation OPENED with". Turning the strip off would deliver a provider
		// name into a model-id column — two sides of a join with different
		// vocabularies, which is worse than the absence. The device edit and the
		// server edit are both wrong until the CLI has a real model id to send.
		"model": "DIVERGENT, deliberately unresolved. The device emits `model` = Codex " +
			"`model_provider` (a PROVIDER, e.g. \"openai\"); the server's field means a model id. " +
			"Allowlisting it on device would populate a model-id field with a provider name. " +
			"Nothing is lost on the wire today — the value is stripped here before signing. Fix is " +
			"upstream in normalize_codex.go, not in either allowlist.",
		"approvalPolicy": "Codex OTel only. The rollout session_meta this CLI parses carries cwd, " +
			"originator, cli_version and model_provider — no approval policy.",
		"sandboxPolicy":   "Codex OTel only — same session_meta gap as approvalPolicy.",
		"reasoningEffort": "Codex OTel only — same session_meta gap as approvalPolicy.",
	},
	"tool_decision": {
		"callId": "This CLI emits no tool_decision event at all. Approval decisions are an OTel-wire " +
			"concept (Codex `approved`, Claude Code `accept`/`reject`); nothing in a transcript or " +
			"hook payload on this device reports one.",
		"decision":       "Same: no tool_decision emitter on this device.",
		"decisionSource": "Same: no tool_decision emitter on this device.",
	},
	"tool_result": {
		"callId": "OTel only — the vendor's own tool-call id. The Claude transcript rail RESOLVES a " +
			"tool_result into its concrete kind (command / file_diff / tool_use) via " +
			"normalizePostToolUseByTool and consumes tool_use_id as the event's stable id rather " +
			"than emitting it as data; the Cursor hook rail emits tool_result with {tool, status} only.",
		"ok":         "OTel only — see callId. This CLI's only tool_result emitter (the Cursor failure hook) reports `status`, never a boolean.",
		"durationMs": "OTel only — see callId. No device tool_result emitter measures a duration.",
		"mcpServer":  "OTel only — the Codex spelling of MCP identity. No tool_result emitter on this device resolves an MCP server.",
		"mcpServerName": "OTel only — the Claude Code spelling of the same fact. Cursor's MCP identity " +
			"work (#148) lands on tool_use, not tool_result.",
		"mcpToolName": "OTel only — see mcpServerName.",
	},
}

// deviceDeclinesToEmit — the server allowlists it, a device rail CAN source it,
// and this CLI deliberately does not emit it. Kind → field → reason.
//
// A separate table from serverOnlyFields on purpose, and the separation is the
// point rather than tidiness. Those entries say "we cannot"; these say "we chose
// not to, and here is the choice". Collapsing the two would file a live decision
// under a heading that reads as a capability gap — after which nobody revisits
// it, because there is apparently nothing to revisit. That is how an open
// question gets retired by filing instead of by deciding, and it is the same
// class of error as a prose reason that outruns its evidence.
//
// So an entry here is a QUESTION someone is expected to answer eventually, not a
// closed fact. It must name the site that declines, so the next reader can go and
// look rather than take this table's word for it.
var deviceDeclinesToEmit = map[string]map[string]string{
	"ai_response": {
		"effort": "AVAILABLE ON THE CURSOR RAIL, DELIBERATELY UNEMITTED — a choice, not a gap. " +
			"Cursor's afterAgentThought hook carries a reasoning-effort parameter in " +
			"`model_params` ({effort high|medium|low}); normalize_cursor_hook.go:48 parses " +
			"model_params into cursorHookModelParm and the emitter declines to put it on the " +
			"event. The reason is written at normalize_cursor_hook.go:249 and is exactly this " +
			"test's subject: `effort` was allowlisted on NEITHER side, so emitting it would be " +
			"stripped silently on one and read as 'an older CLI'. Half of that is now false — " +
			"the server allowlists it as of promptster-backend#634 — so the question 'should " +
			"the Cursor rail emit effort?' is OPEN and belongs to whoever next touches Cursor " +
			"capture. NOTE the grain (design.md §5): Claude reports effort per api_request and " +
			"it can change mid-session, so it must ride on ai_response and must never be " +
			"flattened onto the session. Contrast `speed` in serverOnlyFields: that one has no " +
			"device source at all and nothing to decide.",
	},
}

// deviceOnlyFields — projected by this CLI and NOT allowlisted at the backend
// write boundary, keyed kind → field → reason.
//
// THIS DIRECTION IS THE DANGEROUS ONE. The bytes leave the engineer's machine,
// cross the network, and are discarded at ingest with no error: the value simply
// never appears, for no visible reason. An entry here needs a positive account of
// where the value IS consumed, not merely a note that it is dropped.
var deviceOnlyFields = map[string]map[string]string{
	"presence": {
		"pendingEvents": "Consumed at ingest BEFORE projection, then deliberately not persisted onto " +
			"the timeline row. teams-ingest.ts reads event.data.pendingEvents off the raw beat and " +
			"denormalizes it to engineer_keys.latest_pending_events / latest_pending_reported_at " +
			"(the 2026-08-04 'connected and idle vs. connected and hours behind' fix). The stored " +
			"row keeps device/CLI metadata only, so the backlog reading lives in exactly one place " +
			"instead of two that can disagree. NOT a silent drop — but nothing in " +
			"captureAllowlist.ts says so, which is why it is written down here. Emitted at " +
			"internal/capture/presence.go:139.",
		"pendingOldestEventAt": "Same beat, same pre-projection path; lands in " +
			"engineer_keys.latest_pending_oldest_event_at. The two move as one — the count alone " +
			"cannot tell 62k-queued-five-minutes-ago from 62k-queued-three-weeks-ago.",
		"cursorHooks": "Same pre-projection path as pendingEvents, and denormalized for the same " +
			"reason. teams-ingest.ts reads event.data.cursorHooks off the raw beat, refuses any word " +
			"outside the closed enum, and writes engineer_keys.latest_cursor_hook_state / " +
			"latest_cursor_hook_reported_at (promptster-backend#777). NOT persisted onto the timeline " +
			"row on purpose: this answers a CURRENT-STATE question — 'is anyone's hooks.json being " +
			"rejected right now' — and a state word on every beat would be a second, disagreeing " +
			"source for it. The history that matters is carried by cursorHookRepairs, which is " +
			"cumulative. Emitted at internal/capture/presence.go via InspectCursorHookRail().",
		"cursorHookRepairs": "Same beat, same path; lands in engineer_keys.latest_cursor_hook_repairs. " +
			"Cumulative over the life of the repair log, which is what makes the row-per-beat history " +
			"unnecessary: 'did the v0.18.1 repair ever run on this machine' is a scalar, not a series.",
		"cursorHookUnverifiable": "Same beat, same path; lands in " +
			"engineer_keys.latest_cursor_hook_unverifiable. Rides with the state rather than alone — " +
			"it is the honesty term on `ok`, which means 'nothing PROVABLY wrong', and where this is " +
			"non-zero that is a weaker claim than the word sounds.",
		"cursorStopSeen": "Same beat, same pre-projection path; lands in " +
			"engineer_keys.latest_cursor_stop_seen / latest_cursor_stop_reported_at " +
			"(promptster-backend#816, drizzle/0058 ↔ drizzle-teams/0084). It is the DENOMINATOR " +
			"`cursorHooks` never had: that word says the rail is INSTALLED, which turned out to be a " +
			"different question from whether it WORKS. On 2026-08-25 a machine reporting `ok` for " +
			"weeks was found producing no usage row for 38% of its Cursor turns, and no counter here " +
			"or on the device had recorded one of them. It gates the other four on the SERVER side " +
			"too — a CLI reporting hook state and not stops must not be read as five measured zeros.",
		"cursorStopUsageRows": "Same beat, same path; lands in " +
			"engineer_keys.latest_cursor_stop_usage_rows. The numerator: `stop` invocations that " +
			"emitted a priceable row. Equals cursorGenerations.UsageRows, which `doctor` already " +
			"printed on the machine and nothing carried off it.",
		"cursorStopEmpty": "Same beat, same path; lands in engineer_keys.latest_cursor_stop_empty. " +
			"The HONEST drop — an aborted turn the vendor reported no tokens and no model for, so " +
			"usageEvent declines it. Separated from the rest because it is the one bucket that needs " +
			"no fix, and it is what turns '38% missing' into an answer rather than an alarm.",
		"cursorHookOverruns": "Same beat, same path; lands in " +
			"engineer_keys.latest_cursor_hook_overruns. Invocations abandoned on cursorHookBudget, " +
			"any step. Ours rather than the vendor's, and the reason it is worth shipping: the hook " +
			"runs synchronously inside the engineer's agent loop, so an overrun is us stalling their " +
			"agent for two seconds and THEN losing the measurement. Counted through its own file and " +
			"its own lock (internal/capture/cursor_hook_overruns.go) because contention on the " +
			"generations lock is the leading candidate for what it counts.",
		"cursorHookUnparsed": "Same beat, same path; lands in " +
			"engineer_keys.latest_cursor_hook_unparsed. Payloads that never named a step. Should be " +
			"zero; every other counter here is conditional on a parse, so a non-zero one says they " +
			"describe a subset of the traffic and not the traffic.",
	},
	// `heartbeat` is NOT a kind this CLI mints. The emitter walk finds zero
	// construction sites for it; the only beat built here is `presence`
	// (presence.go:139), and "heartbeat" is merely the prose name for that beat —
	// which is exactly why this entry read as an emitter until the walk was
	// written and said otherwise. First correction it produced.
	//
	// The table entry stays, because the SERVER accepts both spellings
	// (LIVENESS_BEAT_KINDS = {presence, heartbeat}, teams-ingest.ts:382), so
	// another or older client can send one and this projection must not strip its
	// payload. The divergence is therefore real at the table level and carries no
	// live bytes from THIS binary — a distinction the reason has to make, because
	// "these bytes leave the machine" is the entire argument for treating the
	// device-only direction as the dangerous one.
	"heartbeat": {
		"pendingEvents": "Mirrors presence.pendingEvents for the server's other accepted beat " +
			"spelling. This CLI constructs no `heartbeat` event, so no bytes leave here for it today.",
		"pendingOldestEventAt":   "Mirrors presence.pendingOldestEventAt, same reason.",
		"cursorHooks":            "Mirrors presence.cursorHooks for the server's other accepted beat spelling.",
		"cursorHookRepairs":      "Mirrors presence.cursorHookRepairs, same reason.",
		"cursorHookUnverifiable": "Mirrors presence.cursorHookUnverifiable, same reason.",
		"cursorStopSeen":         "Mirrors presence.cursorStopSeen for the server's other accepted beat spelling.",
		"cursorStopUsageRows":    "Mirrors presence.cursorStopUsageRows, same reason.",
		"cursorStopEmpty":        "Mirrors presence.cursorStopEmpty, same reason.",
		"cursorHookOverruns":     "Mirrors presence.cursorHookOverruns, same reason.",
		"cursorHookUnparsed":     "Mirrors presence.cursorHookUnparsed, same reason.",
	},
}

// serverOnlyElementFields / deviceOnlyElementFields — the same two directions for
// array-ELEMENT allowlists. Both are empty: the element tables are in full
// lockstep today, and that is worth checking precisely because they are the
// load-bearing privacy line (the top-level projection is key-only, so an
// allowlisted array rides through whole unless its elements are clamped).
var serverOnlyElementFields = map[string]map[string]map[string]string{}
var deviceOnlyElementFields = map[string]map[string]map[string]string{}

// ---------------------------------------------------------------------------
// Loading the artifact — every failure mode here is fatal.
// ---------------------------------------------------------------------------

type artifactStringArrayClamp struct {
	MaxItems  int    `json:"maxItems"`
	MaxLength int    `json:"maxLength"`
	Pattern   string `json:"pattern"`
}

type artifactKind struct {
	Fields        []string                            `json:"fields"`
	ArrayElements map[string][]string                 `json:"arrayElements"`
	StringArrays  map[string]artifactStringArrayClamp `json:"stringArrays"`
}

type captureAllowlistArtifact struct {
	// Pointers so an ABSENT key is distinguishable from a zero one. A document
	// that lost its version field must not read as version 0 and be reported as a
	// version mismatch — it is a corrupt artifact, a different failure.
	ArtifactVersion *int                    `json:"artifactVersion"`
	ManifestVersion *int                    `json:"manifestVersion"`
	Checksum        string                  `json:"checksum"`
	Kinds           map[string]artifactKind `json:"kinds"`
}

// Gutting floors. NOT an exactness check — the diff below is that, in both
// directions. These exist so a truncated or half-written artifact fails with
// "the artifact is a stub" instead of forty confusing per-kind diffs. They may
// rise. They may not fall without deleting kinds on both sides, which is a
// deliberate act that shows up in a review.
const (
	minArtifactKinds         = 30
	minArtifactFields        = 150
	minArtifactElementFields = 20
)

// captureAllowlistShape reproduces, byte for byte, the sorted line form the
// backend hashes in captureAllowlistArtifact.ts. Two languages cannot be trusted
// to agree on a JSON serializer's key order and escaping; they can be trusted to
// agree on sorted ASCII lines. Same trick as the backend's redactionContractShape().
func captureAllowlistShape(kinds map[string]artifactKind) string {
	var lines []string
	for kind, spec := range kinds {
		lines = append(lines, "kind:"+kind)
		for _, field := range spec.Fields {
			lines = append(lines, fmt.Sprintf("field:%s:%s", kind, field))
		}
		for arrayField, elementFields := range spec.ArrayElements {
			for _, elementField := range elementFields {
				lines = append(lines, fmt.Sprintf("elem:%s:%s:%s", kind, arrayField, elementField))
			}
		}
		for field, clamp := range spec.StringArrays {
			lines = append(lines, fmt.Sprintf("strarr:%s:%s:maxItems=%d", kind, field, clamp.MaxItems))
			lines = append(lines, fmt.Sprintf("strarr:%s:%s:maxLength=%d", kind, field, clamp.MaxLength))
			lines = append(lines, fmt.Sprintf("strarr:%s:%s:pattern=%s", kind, field, clamp.Pattern))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func loadCaptureAllowlistArtifact(t *testing.T) captureAllowlistArtifact {
	t.Helper()

	raw, err := os.ReadFile(captureAllowlistArtifactPath)
	if err != nil {
		// NOT t.Skip. A check that disarms itself when its input disappears is
		// worth nothing — deleting the artifact would otherwise be the cheapest
		// way to silence every assertion in this file.
		t.Fatalf("the capture allowlist artifact is missing or unreadable (%s): %v\n"+
			"An absent artifact is a FAILURE, not a skip: without it this test cannot tell a "+
			"synchronised allowlist from a diverged one.%s",
			captureAllowlistArtifactPath, err, captureAllowlistFixHint)
	}

	var artifact captureAllowlistArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("the capture allowlist artifact (%s) is not parseable JSON: %v%s",
			captureAllowlistArtifactPath, err, captureAllowlistFixHint)
	}

	if artifact.ArtifactVersion == nil {
		t.Fatalf("the capture allowlist artifact (%s) carries no artifactVersion — it is corrupt or "+
			"was not produced by the generator.%s", captureAllowlistArtifactPath, captureAllowlistFixHint)
	}
	if *artifact.ArtifactVersion != captureAllowlistArtifactVersion {
		t.Fatalf("capture allowlist artifact envelope version %d, this test understands %d.\n"+
			"The document was reshaped upstream. Do NOT relax the pin: an unparsed field reads as an "+
			"EMPTY allowlist here, and a default-deny differ calls an empty server side clean.%s",
			*artifact.ArtifactVersion, captureAllowlistArtifactVersion, captureAllowlistFixHint)
	}
	if artifact.ManifestVersion == nil {
		t.Fatalf("the capture allowlist artifact (%s) carries no manifestVersion.%s",
			captureAllowlistArtifactPath, captureAllowlistFixHint)
	}

	// The checksum is over the SHAPE, so a comment or key-order change upstream
	// does not churn it and a single edited field name does not survive it. It is
	// what turns "quietly patch testdata until the test is green" into a
	// deliberate, reviewable forgery.
	shape := captureAllowlistShape(artifact.Kinds)
	sum := sha256.Sum256([]byte(shape))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if artifact.Checksum != want {
		t.Fatalf("capture allowlist artifact checksum mismatch.\n  declared:   %s\n  recomputed: %s\n"+
			"The artifact was hand-edited or truncated in transit. Re-sync it; do not patch it.%s",
			artifact.Checksum, want, captureAllowlistFixHint)
	}

	fields, elementFields := 0, 0
	for _, spec := range artifact.Kinds {
		fields += len(spec.Fields)
		for _, elems := range spec.ArrayElements {
			elementFields += len(elems)
		}
	}
	if len(artifact.Kinds) < minArtifactKinds || fields < minArtifactFields ||
		elementFields < minArtifactElementFields {
		t.Fatalf("the capture allowlist artifact is a stub: %d kinds / %d fields / %d element fields "+
			"(floors: %d / %d / %d). A partially-written artifact must fail as a stub, not as forty "+
			"per-kind diffs.%s",
			len(artifact.Kinds), fields, elementFields,
			minArtifactKinds, minArtifactFields, minArtifactElementFields, captureAllowlistFixHint)
	}

	return artifact
}

// report emits ONE failure per test carrying every finding, so a real divergence
// is read as a list rather than scrolled past between repeated fix instructions.
func report(t *testing.T, headline string, findings []string) {
	t.Helper()
	if len(findings) == 0 {
		return
	}
	t.Errorf("%s (%d):\n  - %s\n%s", headline, len(findings),
		strings.Join(findings, "\n  - "), captureAllowlistFixHint)
}

func set(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCaptureAllowlistKindsAreInLockstep — every kind, both directions.
func TestCaptureAllowlistKindsAreInLockstep(t *testing.T) {
	artifact := loadCaptureAllowlistArtifact(t)
	var findings []string

	for _, kind := range sortedKeys(artifact.Kinds) {
		if _, onDevice := projectFieldAllowlist[kind]; onDevice {
			continue
		}
		if _, declared := kindsNotEmittedOnDevice[kind]; declared {
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"%s — allowlisted at the SERVER write boundary, absent from projectFieldAllowlist. "+
				"Every event of this kind projects to {} on device. Add it here, or declare it in "+
				"kindsNotEmittedOnDevice with the reason this CLI cannot emit it.", kind))
	}

	for _, kind := range sortedKeys(projectFieldAllowlist) {
		if _, onServer := artifact.Kinds[kind]; onServer {
			continue
		}
		// No exception category for this direction, on purpose: a kind this CLI
		// projects and the server does not allowlist is discarded WHOLE at ingest.
		// There is no design under which that is intended.
		findings = append(findings, fmt.Sprintf(
			"%s — in projectFieldAllowlist, NOT allowlisted at the server write boundary. Every "+
				"event of this kind is dropped whole at ingest, with no error and no telemetry.", kind))
	}

	report(t, "capture allowlist kinds have diverged", findings)
}

// TestCaptureAllowlistFieldsAreInLockstep — every field of every shared kind.
func TestCaptureAllowlistFieldsAreInLockstep(t *testing.T) {
	artifact := loadCaptureAllowlistArtifact(t)
	var findings []string

	for _, kind := range sortedKeys(artifact.Kinds) {
		deviceFields, onDevice := projectFieldAllowlist[kind]
		if !onDevice {
			continue // reported by the kinds test
		}
		device := set(deviceFields)
		server := set(artifact.Kinds[kind].Fields)

		for _, field := range artifact.Kinds[kind].Fields {
			if device[field] {
				continue
			}
			if _, declared := serverOnlyFields[kind][field]; declared {
				continue
			}
			if _, declined := deviceDeclinesToEmit[kind][field]; declined {
				continue
			}
			findings = append(findings, fmt.Sprintf(
				"%s.%s — SERVER-only. The CLI rail will never deliver it, and the absence is "+
					"indistinguishable from an old CLI. Add it to projectFieldAllowlist; or declare "+
					"serverOnlyFields[%q][%q] if the device CANNOT source it; or "+
					"deviceDeclinesToEmit[%q][%q] if a rail can source it and we choose not to.",
				kind, field, kind, field, kind, field))
		}

		for _, field := range deviceFields {
			if server[field] {
				continue
			}
			if _, declared := deviceOnlyFields[kind][field]; declared {
				continue
			}
			findings = append(findings, fmt.Sprintf(
				"%s.%s — DEVICE-only. These bytes leave the engineer's machine and are discarded at "+
					"ingest with no error. Add it server-side FIRST, or declare "+
					"deviceOnlyFields[%q][%q] with where the value is actually consumed.",
				kind, field, kind, field))
		}
	}

	report(t, "capture allowlist fields have diverged", findings)
}

// TestCaptureAllowlistArrayElementsAreInLockstep — the nested privacy line.
//
// The top-level projection is key-only, so an allowlisted array field carries its
// elements verbatim unless an element allowlist clamps them. That makes the
// element tables load-bearing for the no-source guarantee, and worth the same
// bidirectional check as the fields above.
func TestCaptureAllowlistArrayElementsAreInLockstep(t *testing.T) {
	artifact := loadCaptureAllowlistArtifact(t)
	var findings []string

	for _, kind := range sortedKeys(artifact.Kinds) {
		if _, onDevice := projectFieldAllowlist[kind]; !onDevice {
			continue
		}
		serverElems := artifact.Kinds[kind].ArrayElements
		deviceElems := projectArrayElementAllowlist[kind]

		for _, arrayField := range sortedKeys(serverElems) {
			deviceSet := set(deviceElems[arrayField])
			for _, elementField := range serverElems[arrayField] {
				if deviceSet[elementField] {
					continue
				}
				if _, declared := serverOnlyElementFields[kind][arrayField][elementField]; declared {
					continue
				}
				findings = append(findings, fmt.Sprintf(
					"%s.%s[].%s — SERVER-only. The CLI strips it before signing, so it can never "+
						"arrive. Add it to projectArrayElementAllowlist, or declare "+
						"serverOnlyElementFields[%q][%q][%q].",
					kind, arrayField, elementField, kind, arrayField, elementField))
			}
		}

		for _, arrayField := range sortedKeys(deviceElems) {
			serverSet := set(serverElems[arrayField])
			for _, elementField := range deviceElems[arrayField] {
				if serverSet[elementField] {
					continue
				}
				if _, declared := deviceOnlyElementFields[kind][arrayField][elementField]; declared {
					continue
				}
				findings = append(findings, fmt.Sprintf(
					"%s.%s[].%s — DEVICE-only. It leaves the machine and is discarded at ingest. "+
						"Add it server-side FIRST, or declare deviceOnlyElementFields[%q][%q][%q].",
					kind, arrayField, elementField, kind, arrayField, elementField))
			}
		}
	}

	report(t, "capture allowlist array elements have diverged", findings)
}

// TestCaptureAllowlistStringArrayClampsAreInLockstep — volume and shape bounds.
//
// The backend carries the shape rule as an RE2-compatible pattern string; this
// side carries it as a Go predicate. Rather than declare that undiffable, the
// pattern is COMPILED here and both are run over a probe corpus: an equivalence
// check, not a syntactic one. A predicate that has drifted from the pattern (or a
// pattern tightened server-side) fails on the probe that separates them, by value.
func TestCaptureAllowlistStringArrayClampsAreInLockstep(t *testing.T) {
	artifact := loadCaptureAllowlistArtifact(t)
	var findings []string

	probes := []string{
		"", "A", "_", "0", "0BAD", "STRIPE_API_KEY", "DATABASE_URL", "aws_secret",
		"AKIAIOSFODNN7EXAMPLE", "has space", "has-dash", "has.dot", "a=b", "a/b", "a+b",
		"quote\"d", "new\nline", "<redacted>", "tab\there", "ünïcode", "trailing_",
	}

	unmatchedOnDevice := map[string]bool{}
	for kind, clamps := range projectStringArrayClamp {
		for field := range clamps {
			unmatchedOnDevice[kind+"."+field] = true
		}
	}

	for _, kind := range sortedKeys(artifact.Kinds) {
		for _, field := range sortedKeys(artifact.Kinds[kind].StringArrays) {
			serverClamp := artifact.Kinds[kind].StringArrays[field]
			deviceClamp, ok := projectStringArrayClamp[kind][field]
			if !ok {
				findings = append(findings, fmt.Sprintf(
					"%s.%s — clamped as a string array at the SERVER write boundary and unclamped on "+
						"the device: the array would ride through this projection whole, the whole "+
						"array trusted because its key was allowlisted.", kind, field))
				continue
			}
			delete(unmatchedOnDevice, kind+"."+field)

			if deviceClamp.maxItems != serverClamp.MaxItems {
				findings = append(findings, fmt.Sprintf("%s.%s maxItems: device %d, server %d.",
					kind, field, deviceClamp.maxItems, serverClamp.MaxItems))
			}
			if deviceClamp.maxLength != serverClamp.MaxLength {
				findings = append(findings, fmt.Sprintf("%s.%s maxLength: device %d, server %d.",
					kind, field, deviceClamp.maxLength, serverClamp.MaxLength))
			}

			pattern, err := regexp.Compile(serverClamp.Pattern)
			if err != nil {
				t.Fatalf("%s.%s: the server pattern %q does not compile as RE2 — it cannot be "+
					"mirrored on this side at all.%s",
					kind, field, serverClamp.Pattern, captureAllowlistFixHint)
			}
			for _, probe := range probes {
				wantAllowed := pattern.MatchString(probe)
				gotAllowed := deviceClamp.allow != nil && deviceClamp.allow(probe)
				if wantAllowed != gotAllowed {
					findings = append(findings, fmt.Sprintf(
						"%s.%s disagrees on %q: server pattern %s says allowed=%v, the device "+
							"predicate says allowed=%v.",
						kind, field, probe, serverClamp.Pattern, wantAllowed, gotAllowed))
				}
			}
		}
	}

	for _, key := range sortedKeys(unmatchedOnDevice) {
		findings = append(findings, fmt.Sprintf(
			"%s — clamped as a string array on the device and not a string array at the server write "+
				"boundary. The two sides disagree about the field's SHAPE, which is louder than a "+
				"field-name diff.", key))
	}

	report(t, "capture allowlist string-array clamps have diverged", findings)
}

// TestCaptureAllowlistExceptionsAreStillReal — the guard on the guard.
//
// "What happens to this check after it succeeds?" An exception list nobody prunes
// becomes a second, undeclared allowlist: the day the CLI starts sending
// `costUsd`, a stale serverOnlyFields entry would wave it through unremarked. So
// every declared exception must still describe a LIVE divergence and must carry a
// non-empty reason — an exception without one is a TODO wearing a permission slip.
func TestCaptureAllowlistExceptionsAreStillReal(t *testing.T) {
	artifact := loadCaptureAllowlistArtifact(t)
	var findings []string

	for _, kind := range sortedKeys(kindsNotEmittedOnDevice) {
		if strings.TrimSpace(kindsNotEmittedOnDevice[kind]) == "" {
			findings = append(findings, fmt.Sprintf("kindsNotEmittedOnDevice[%q] carries no reason.", kind))
		}
		if _, onServer := artifact.Kinds[kind]; !onServer {
			findings = append(findings, fmt.Sprintf(
				"kindsNotEmittedOnDevice[%q] names a kind the server no longer allowlists. Delete it.", kind))
		}
		if _, onDevice := projectFieldAllowlist[kind]; onDevice {
			findings = append(findings, fmt.Sprintf(
				"kindsNotEmittedOnDevice[%q] says this CLI does not emit the kind, but it is in "+
					"projectFieldAllowlist. Delete the exception.", kind))
		}
	}

	// Both server-side exception tables carry the same two staleness obligations —
	// the server must still allowlist the field, and the device must still not.
	// Walked together so the newer table cannot be added without inheriting them.
	serverSideExceptionTables := []struct {
		name  string
		table map[string]map[string]string
	}{
		{"serverOnlyFields", serverOnlyFields},
		{"deviceDeclinesToEmit", deviceDeclinesToEmit},
	}
	for _, entry := range serverSideExceptionTables {
		for _, kind := range sortedKeys(entry.table) {
			for _, field := range sortedKeys(entry.table[kind]) {
				if strings.TrimSpace(entry.table[kind][field]) == "" {
					findings = append(findings, fmt.Sprintf("%s[%q][%q] carries no reason.", entry.name, kind, field))
				}
				if !set(artifact.Kinds[kind].Fields)[field] {
					findings = append(findings, fmt.Sprintf(
						"%s[%q][%q] is stale: the server no longer allowlists it.", entry.name, kind, field))
				}
				if set(projectFieldAllowlist[kind])[field] {
					findings = append(findings, fmt.Sprintf(
						"%s[%q][%q] is stale: the device allowlists it now, so the two sides agree. "+
							"Delete the exception — leaving it would stop this test noticing the day one "+
							"side drops it again.", entry.name, kind, field))
				}
			}
		}
	}

	// The two tables mean opposite things about our capability, so a field in both
	// asserts we can and cannot source it at once. Left unchecked, the weaker
	// reading ("cannot") is the one a reader takes, and the decision recorded in
	// the other table stops being visible.
	for _, kind := range sortedKeys(deviceDeclinesToEmit) {
		for _, field := range sortedKeys(deviceDeclinesToEmit[kind]) {
			if _, alsoServerOnly := serverOnlyFields[kind][field]; alsoServerOnly {
				findings = append(findings, fmt.Sprintf(
					"%s.%s is in BOTH serverOnlyFields (the device cannot source it) and "+
						"deviceDeclinesToEmit (it can, and we decline). Those cannot both be true. "+
						"Keep the one that matches the emitters.", kind, field))
			}
		}
	}

	for _, kind := range sortedKeys(deviceOnlyFields) {
		for _, field := range sortedKeys(deviceOnlyFields[kind]) {
			if strings.TrimSpace(deviceOnlyFields[kind][field]) == "" {
				findings = append(findings, fmt.Sprintf("deviceOnlyFields[%q][%q] carries no reason.", kind, field))
			}
			if !set(projectFieldAllowlist[kind])[field] {
				findings = append(findings, fmt.Sprintf(
					"deviceOnlyFields[%q][%q] is stale: the device no longer projects it.", kind, field))
			}
			if set(artifact.Kinds[kind].Fields)[field] {
				findings = append(findings, fmt.Sprintf(
					"deviceOnlyFields[%q][%q] is stale: the server allowlists it now. Delete the "+
						"exception — the gap it tracked is CLOSED, and leaving it open would hide the "+
						"next one.", kind, field))
			}
		}
	}

	for _, kind := range sortedKeys(serverOnlyElementFields) {
		for _, arrayField := range sortedKeys(serverOnlyElementFields[kind]) {
			for _, elementField := range sortedKeys(serverOnlyElementFields[kind][arrayField]) {
				if strings.TrimSpace(serverOnlyElementFields[kind][arrayField][elementField]) == "" {
					findings = append(findings, fmt.Sprintf(
						"serverOnlyElementFields[%q][%q][%q] carries no reason.", kind, arrayField, elementField))
				}
				if !set(artifact.Kinds[kind].ArrayElements[arrayField])[elementField] {
					findings = append(findings, fmt.Sprintf(
						"serverOnlyElementFields[%q][%q][%q] is stale: the server no longer allowlists it.",
						kind, arrayField, elementField))
				}
				if set(projectArrayElementAllowlist[kind][arrayField])[elementField] {
					findings = append(findings, fmt.Sprintf(
						"serverOnlyElementFields[%q][%q][%q] is stale: the device allowlists it now. Delete it.",
						kind, arrayField, elementField))
				}
			}
		}
	}

	for _, kind := range sortedKeys(deviceOnlyElementFields) {
		for _, arrayField := range sortedKeys(deviceOnlyElementFields[kind]) {
			for _, elementField := range sortedKeys(deviceOnlyElementFields[kind][arrayField]) {
				if strings.TrimSpace(deviceOnlyElementFields[kind][arrayField][elementField]) == "" {
					findings = append(findings, fmt.Sprintf(
						"deviceOnlyElementFields[%q][%q][%q] carries no reason.", kind, arrayField, elementField))
				}
				if !set(projectArrayElementAllowlist[kind][arrayField])[elementField] {
					findings = append(findings, fmt.Sprintf(
						"deviceOnlyElementFields[%q][%q][%q] is stale: the device no longer projects it.",
						kind, arrayField, elementField))
				}
				if set(artifact.Kinds[kind].ArrayElements[arrayField])[elementField] {
					findings = append(findings, fmt.Sprintf(
						"deviceOnlyElementFields[%q][%q][%q] is stale: the server allowlists it now. Delete it.",
						kind, arrayField, elementField))
				}
			}
		}
	}

	report(t, "declared capture-allowlist exceptions are stale", findings)
}

// TestProjectUsageFieldsMatchTheServerUsageBlock — the shared constant.
//
// projectUsageFields is the nine-field block the backend calls USAGE_FIELDS and
// keeps byte-for-byte in lockstep with this side. `costUsd` and the Codex-only
// token fields were deliberately kept OUT of it and added to the ai_response spec
// directly, precisely so widening the shared constant could not break that
// lockstep for a field one side can never populate. This asserts the constant is
// still the shared PREFIX of both usage-bearing kinds, in order — the exact
// property that arrangement depends on.
func TestProjectUsageFieldsMatchTheServerUsageBlock(t *testing.T) {
	artifact := loadCaptureAllowlistArtifact(t)
	var findings []string

	for _, kind := range []string{"ai_response", "subagent_usage"} {
		serverFields := artifact.Kinds[kind].Fields
		if len(serverFields) < len(projectUsageFields) {
			t.Fatalf("%s carries %d server fields, fewer than the %d shared usage fields.%s",
				kind, len(serverFields), len(projectUsageFields), captureAllowlistFixHint)
		}
		for i, field := range projectUsageFields {
			if serverFields[i] != field {
				findings = append(findings, fmt.Sprintf(
					"%s field %d: the shared usage block has %q, the server spec has %q. USAGE_FIELDS "+
						"is a byte-for-byte shared constant; a divergence in it breaks BOTH "+
						"usage-bearing kinds at once.", kind, i, field, serverFields[i]))
			}
		}
	}

	report(t, "the shared usage block has diverged", findings)
}
