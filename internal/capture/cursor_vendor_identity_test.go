package capture

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// cursor-vendor-rail-liveness §4.3 / §4.4 — the four fields the collector began
// forwarding on 2026-09-06, and the three properties that make them worth
// forwarding at all:
//
//  1. absent, zero and present are THREE outcomes for `cacheWriteTokens`;
//  2. absent and `""` are ONE outcome for the identities, deliberately;
//  3. none of the four is in the digest, because the backend recomputes it.

// stageOneRow drives a raw vendor row through the ACTUAL capture boundary —
// unmarshal, snapshot build, row event — and returns the staged payload. Not a
// hand-built struct: the json tags are half of what is being tested, and a
// struct literal would pass with the tag misspelled.
func stageOneRow(t *testing.T, rawRow string) map[string]interface{} {
	t.Helper()
	var page cursorVendorUsagePage
	if err := json.Unmarshal([]byte(`{"usageEventsDisplay":[`+rawRow+`]}`), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.UsageEventsDisplay) != 1 {
		t.Fatalf("rows=%d, want 1", len(page.UsageEventsDisplay))
	}
	start := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	snap := buildCursorVendorSnapshot(
		page.UsageEventsDisplay, start, start.Add(30*24*time.Hour), nil,
		cursorVendorShapeRecord{HTTPStatus: 200},
	)
	events := snap.rowEvents("device-1")
	if len(events) != 1 {
		t.Fatalf("events=%d, want 1", len(events))
	}
	data, ok := events[0].Data.(map[string]interface{})
	if !ok {
		t.Fatalf("payload is %T, want map", events[0].Data)
	}
	return data
}

// TestCacheWriteTokensKeepsAbsentZeroAndPresentDistinct — the §4.3 invariant.
//
// A ZERO THAT MEANS "UNMEASURED" IS THE BUG THIS PINS. On a rail whose subject
// is context waste, "the vendor reported no cache-write figure" and "the vendor
// reported zero cache writes" are opposite readings, and a `0` default asserts
// the second having only observed the first. The backend normalizes absent to
// `null` and zero to `0`; the device has to hand it two different things for
// that to mean anything.
func TestCacheWriteTokensKeepsAbsentZeroAndPresentDistinct(t *testing.T) {
	base := `"timestamp":"1788307200000","model":"composer-2.5","kind":"USAGE_EVENT_KIND_INCLUDED_IN_ULTRA","conversationId":"c1"`

	present := stageOneRow(t, `{`+base+`,"tokenUsage":{"inputTokens":10,"cacheWriteTokens":4096}}`)
	if got, ok := present["cacheWriteTokens"]; !ok || got != int64(4096) {
		t.Fatalf("present: cacheWriteTokens=%v (ok=%v), want 4096", got, ok)
	}

	zero := stageOneRow(t, `{`+base+`,"tokenUsage":{"inputTokens":10,"cacheWriteTokens":0}}`)
	got, ok := zero["cacheWriteTokens"]
	if !ok {
		t.Fatal("zero: key dropped — an explicit 0 must survive as 0, not vanish into absence")
	}
	if got != int64(0) {
		t.Fatalf("zero: cacheWriteTokens=%v, want 0", got)
	}

	absent := stageOneRow(t, `{`+base+`,"tokenUsage":{"inputTokens":10}}`)
	if got, ok := absent["cacheWriteTokens"]; ok {
		t.Fatalf("absent: cacheWriteTokens=%v present — an unreported count must emit NO key, never 0", got)
	}

	// The whole-object case: an aborted request carries no `tokenUsage` at all,
	// and that must reach the same absence rather than panic or default.
	noUsage := stageOneRow(t, `{`+base+`}`)
	if _, ok := noUsage["cacheWriteTokens"]; ok {
		t.Fatal("row with no tokenUsage object emitted a cacheWriteTokens")
	}
}

// TestVendorAutomationIdentityForwardsPresentAndDropsEmpty — the §4.4 invariant.
//
// Cursor sends `""` for the identities that do not apply to a row rather than
// omitting the key. Empty is dropped here, which lands on the same `null` the
// backend's `nonEmptyString` produces from a literal `""` — so absent and empty
// are ONE reading end to end, and an interactive row carries none of the three
// keys instead of three blanks. That is a consistency requirement, not a
// preference: emitting `""` on some rows and omitting on others would make a
// blank identity look like a real one whose value happens to be empty.
func TestVendorAutomationIdentityForwardsPresentAndDropsEmpty(t *testing.T) {
	base := `"timestamp":"1788307200000","model":"grok-bot-automation","kind":"USAGE_EVENT_KIND_INCLUDED_IN_ULTRA","conversationId":"sand-subagent-abc"`

	present := stageOneRow(t, `{`+base+`,"cloudAgentId":"ca_1","automationId":"au_2","serviceAccountId":"sa_3"}`)
	for field, want := range map[string]string{
		"cloudAgentId": "ca_1", "automationId": "au_2", "serviceAccountId": "sa_3",
	} {
		if got := present[field]; got != want {
			t.Fatalf("present: %s=%v, want %q", field, got, want)
		}
	}

	// The real shape of an interactive row: the keys ARE sent, as empty strings.
	empty := stageOneRow(t, `{`+base+`,"cloudAgentId":"","automationId":"","serviceAccountId":""}`)
	for _, field := range []string{"cloudAgentId", "automationId", "serviceAccountId"} {
		if got, ok := empty[field]; ok {
			t.Fatalf("empty: %s=%q emitted — `\"\"` must drop, matching the backend's nonEmptyString", field, got)
		}
	}

	absent := stageOneRow(t, `{`+base+`}`)
	for _, field := range []string{"cloudAgentId", "automationId", "serviceAccountId"} {
		if _, ok := absent[field]; ok {
			t.Fatalf("absent: %s emitted", field)
		}
	}

	// A row may carry one identity and not the others — the three are three
	// different claims, not one field with three spellings.
	partial := stageOneRow(t, `{`+base+`,"cloudAgentId":"","automationId":"au_only","serviceAccountId":""}`)
	if partial["automationId"] != "au_only" {
		t.Fatalf("partial: automationId=%v, want au_only", partial["automationId"])
	}
	if _, ok := partial["cloudAgentId"]; ok {
		t.Fatal("partial: an empty cloudAgentId rode along with a populated automationId")
	}
}

// TestNewVendorFieldsAreNotExpectedRowFields pins the §4.3/§4.4 monitor
// decision, because it is the kind of decision a later reader "tidies".
//
// An entry in cursorVendorExpectedRowFields PAGES SOMEONE when it goes missing.
// `cacheWriteTokens` was absent from the 2026-08-27 probe and present on Cursor
// 3.19.13, and the fleet runs whatever each engineer installed — so listing it
// alarms on version skew. The identities are populated only on rows that HAVE
// one. All four stay observable through `shapeObservedFields` regardless.
func TestNewVendorFieldsAreNotExpectedRowFields(t *testing.T) {
	expected := map[string]bool{}
	for _, f := range cursorVendorExpectedRowFields {
		expected[f] = true
	}
	for _, f := range []string{
		"tokenUsage.cacheWriteTokens", "cloudAgentId", "automationId", "serviceAccountId",
	} {
		if expected[f] {
			t.Errorf("%s is in cursorVendorExpectedRowFields — its absence would now page an "+
				"operator. It is parsed but NOT expected on purpose: absence has an innocent "+
				"cause (an older Cursor, or a row with no such identity).", f)
		}
	}

	// The corollary: adding them must not have disturbed what IS expected. A
	// shape alarm that quietly stopped covering `inputTokens` is worse than one
	// that cries wolf.
	for _, f := range []string{
		"tokenUsage.inputTokens", "tokenUsage.outputTokens",
		"tokenUsage.cacheReadTokens", "tokenUsage.totalCents", "chargedCents",
	} {
		if !expected[f] {
			t.Errorf("%s dropped out of cursorVendorExpectedRowFields", f)
		}
	}
}

// TestNewVendorFieldsStayOutOfTheContentDigest is the one that keeps the rail
// alive, and it is not a style check.
//
// promptster-backend `canonicalSnapshotSha256` RECOMPUTES this digest over the
// staged rows and refuses the snapshot with `content_digest_mismatch` when it
// disagrees. Its list is fourteen fields and BE#898 did not widen it. A
// well-meaning "the new fields should be in the digest too" edit here would
// therefore not tighten integrity — it would refuse 100% of snapshots and take
// the rail dark, with the only symptom being usage going to zero.
func TestNewVendorFieldsStayOutOfTheContentDigest(t *testing.T) {
	zero, big := int64(0), int64(999999)
	bare := cursorVendorRow{
		Timestamp:      "1788307200000",
		Model:          "composer-2.5",
		ConversationID: "c1",
		TokenUsage:     &cursorVendorTokenUsage{InputTokens: &zero},
	}
	loaded := bare
	loaded.TokenUsage = &cursorVendorTokenUsage{InputTokens: &zero, CacheWriteTokens: &big}
	loaded.CloudAgentID = "ca_1"
	loaded.AutomationID = "au_2"
	loaded.ServiceAccountID = "sa_3"

	if canonicalRowLine(bare) != canonicalRowLine(loaded) {
		t.Fatal("the four fields added 2026-09-06 changed the canonical row line. The backend " +
			"recomputes this digest over fourteen fields and refuses any snapshot that disagrees; " +
			"widening it is a two-repo change that must bump cursorVendorSnapshotDigestVersion on " +
			"both sides in the same release.")
	}

	// And the digest is still doing its job on the fields it does cover.
	moved := bare
	moved.TokenUsage = &cursorVendorTokenUsage{InputTokens: &big}
	if canonicalRowLine(bare) == canonicalRowLine(moved) {
		t.Fatal("inputTokens moved without moving the canonical row line")
	}
}

// TestObservedFieldsSeeTheNewNamesOnTheWire — the monitor's own input. The
// observation is built off the RAW bytes, so it must name all four whether or
// not the struct has a slot for them; this is what keeps the four visible
// despite being deliberately unexpected.
func TestObservedFieldsSeeTheNewNamesOnTheWire(t *testing.T) {
	client := &cursorVendorClient{base: "https://api2.cursor.sh", http: &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(`{"totalUsageEventsCount":1,"usageEventsDisplay":[{` +
				`"timestamp":"1788307200000","model":"m","kind":"k","conversationId":"c",` +
				`"isHeadless":false,"chargedCents":1.5,"cloudAgentId":"ca_1","automationId":"",` +
				`"serviceAccountId":"","tokenUsage":{"inputTokens":1,"outputTokens":2,` +
				`"cacheReadTokens":3,"cacheWriteTokens":4,"totalCents":5}}]}`), nil
		}),
	}}
	_, observed, err := client.fetchUsagePage(cursorCredential{token: "t"}, 1)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range observed {
		seen[f] = true
	}
	for _, want := range []string{
		"tokenUsage.cacheWriteTokens", "cloudAgentId", "automationId", "serviceAccountId",
	} {
		if !seen[want] {
			t.Errorf("%s absent from shapeObservedFields — the four unexpected fields are only "+
				"visible through this record, so losing it here loses them entirely", want)
		}
	}
	// Unexpected means unexpected: none of them may show up as a shape alarm.
	if missing := missingExpectedFields(observed); len(missing) != 0 {
		t.Fatalf("missingExpectedFields=%v, want none on a full row", missing)
	}
}
