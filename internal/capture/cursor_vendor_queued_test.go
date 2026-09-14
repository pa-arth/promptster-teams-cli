package capture

import (
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

func vendorEventData(ev event.Event) map[string]interface{} { return ev.Data.(map[string]interface{}) }

func TestCursorVendorV2AppendsOnlyNewRowsAndPreservesOrdinals(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)
	at := start.Add(12 * time.Hour)
	shape := cursorVendorShapeRecord{}
	a := cursorVendorRow{Timestamp: "1788200000000", ConversationID: "conv-a"}
	b := cursorVendorRow{Timestamp: "1788300000000", ConversationID: "conv-b"}
	earlier := cursorVendorRow{Timestamp: "1788100000000", ConversationID: "conv-earlier"}
	makeSnapshot := func(rows ...cursorVendorRow) cursorVendorSnapshot {
		snap := buildCursorVendorSnapshot(rows, start, end, nil, shape)
		snap.AccountRef = "unknown"
		return snap
	}
	var sent []event.Event
	enqueue := func(ev event.Event) bool { sent = append(sent, ev); return true }
	if !queueCompleteCursorVendorSnapshot(makeSnapshot(a, b), "device-a", at, "", enqueue) || len(sent) != 3 {
		t.Fatalf("first poll sent %d events, want 2 rows and completion", len(sent))
	}
	firstID := vendorEventData(sent[0])["snapshotId"]
	firstOrdinal := map[string]interface{}{}
	for _, ev := range sent[:2] {
		firstOrdinal[vendorEventData(ev)["conversationId"].(string)] = vendorEventData(ev)["ordinal"]
	}
	sent = nil
	if !queueCompleteCursorVendorSnapshot(makeSnapshot(earlier, a, b), "device-a", at.Add(15*time.Minute), "", enqueue) || len(sent) != 2 {
		t.Fatalf("one insertion sent %d events, want one row and completion", len(sent))
	}
	if vendorEventData(sent[0])["snapshotId"] != firstID || vendorEventData(sent[0])["ordinal"] != 2 || vendorEventData(sent[0])["conversationId"] != "conv-earlier" {
		t.Fatalf("new row did not append under stable snapshot ID: %+v", sent[0].Data)
	}
	if vendorEventData(sent[1])["protocolVersion"] != 2 || vendorEventData(sent[1])["rowCount"] != 3 {
		t.Fatalf("v2 completion = %+v", sent[1].Data)
	}
	sent = nil
	if !queueCompleteCursorVendorSnapshot(makeSnapshot(b, a, earlier), "device-a", at.Add(30*time.Minute), "", enqueue) || len(sent) != 1 {
		t.Fatalf("unchanged reordered poll sent %d events", len(sent))
	}
	if firstOrdinal["conv-a"] == firstOrdinal["conv-b"] {
		t.Fatal("baseline ordinals collided")
	}
	sent = nil
	changed := a
	changed.Model = "new-model"
	if !queueCompleteCursorVendorSnapshot(makeSnapshot(changed, b, earlier), "device-a", at.Add(45*time.Minute), "", enqueue) || len(sent) != 4 {
		t.Fatalf("correction sent %d events, want one full replacement", len(sent))
	}
	if vendorEventData(sent[0])["snapshotId"] == firstID {
		t.Fatal("correction reused old generation")
	}
}

func TestCursorVendorV2AccountAttributionDoesNotRestageRows(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	snap := buildCursorVendorSnapshot([]cursorVendorRow{{Timestamp: "1788200000000", ConversationID: "conv-a"}}, start, start.Add(30*24*time.Hour), nil, cursorVendorShapeRecord{})
	snap.AccountRef = "unknown"
	var sent []event.Event
	enqueue := func(ev event.Event) bool { sent = append(sent, ev); return true }
	queueCompleteCursorVendorSnapshot(snap, "device-a", start.Add(time.Hour), "", enqueue)
	if _, ok := vendorEventData(sent[0])["accountRef"]; ok {
		t.Fatal("v2 row carries mutable accountRef")
	}
	sent = nil
	snap.AccountRef = "account-a"
	queueCompleteCursorVendorSnapshot(snap, "device-a", start.Add(2*time.Hour), "", enqueue)
	if len(sent) != 1 || vendorEventData(sent[0])["accountRef"] != "account-a" {
		t.Fatalf("attribution update sent %+v", sent)
	}
}

func TestCursorVendorV2FailedAppendRetriesAndPeriodicRepair(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	snap := buildCursorVendorSnapshot([]cursorVendorRow{{Timestamp: "1788200000000", ConversationID: "a"}, {Timestamp: "1788300000000", ConversationID: "b"}}, start, start.Add(30*24*time.Hour), nil, cursorVendorShapeRecord{})
	at := start.Add(time.Hour)
	calls := 0
	if queueCompleteCursorVendorSnapshot(snap, "device-a", at, "", func(event.Event) bool { calls++; return calls != 2 }) {
		t.Fatal("partial append reported success")
	}
	calls = 0
	if !queueCompleteCursorVendorSnapshot(snap, "device-a", at.Add(15*time.Minute), "", func(event.Event) bool { calls++; return true }) || calls != 3 {
		t.Fatalf("retry sent %d events", calls)
	}
	calls = 0
	queueCompleteCursorVendorSnapshot(snap, "device-a", at.Add(cursorVendorRowsRefreshInterval+15*time.Minute), "", func(event.Event) bool { calls++; return true })
	if calls != 3 {
		t.Fatalf("repair sent %d events", calls)
	}
}
