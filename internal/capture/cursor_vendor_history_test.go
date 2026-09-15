package capture

import (
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

func TestPreviousCursorBillingCycle(t *testing.T) {
	start := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	reset := start.AddDate(0, 1, 0)
	previous, end, ok := previousCursorBillingCycle(start, reset)
	if !ok || !end.Equal(start) || !previous.Equal(time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("previous=%s end=%s ok=%v", previous, end, ok)
	}
	if _, _, ok := previousCursorBillingCycle(start, reset.Add(time.Hour)); ok {
		t.Fatal("accepted a changed billing period")
	}
	late := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	if _, _, ok := previousCursorBillingCycle(late, late.AddDate(0, 1, 0)); ok {
		t.Fatal("inferred an ambiguous month-end boundary")
	}
	day29 := time.Date(2026, 1, 29, 0, 0, 0, 0, time.UTC)
	if _, _, ok := previousCursorBillingCycle(day29, day29.AddDate(0, 1, 0)); ok {
		t.Fatal("inferred a day-29 boundary")
	}
}

func TestHistoricalCursorSnapshotQueuesOnceAndReplacesCorrection(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	row := cursorVendorRow{Timestamp: "1785700800000", ConversationID: "conversation-1", Model: "composer-2.5"}
	snapshot := buildCursorVendorSnapshot("account-a", []cursorVendorRow{row}, start, end, nil, cursorVendorShapeRecord{})
	var sent []event.Event
	enqueue := func(ev event.Event) bool { sent = append(sent, ev); return true }
	if err := queueHistoricalCursorVendorSnapshot(snapshot, "device", end, "2026.09", enqueue); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].Kind != "cursorVendorUsage" || sent[1].Kind != "cursorVendorSnapshot" {
		t.Fatalf("queued %d events", len(sent))
	}
	done := vendorV2Data(sent[1])
	if done["accountRef"] != "account-a" || done["protocolVersion"] != 2 ||
		done["billingCycleStartsAt"] != start.Format(cursorVendorCycleTimeFormat) ||
		done["billingCycleResetsAt"] != end.Format(cursorVendorCycleTimeFormat) {
		t.Fatalf("completion = %+v", done)
	}
	if err := queueHistoricalCursorVendorSnapshot(snapshot, "device", end.Add(time.Hour), "2026.09", enqueue); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("unchanged snapshot requeued: %d", len(sent))
	}
	if err := queueHistoricalCursorVendorSnapshot(snapshot, "device", end.Add(cursorVendorRepairInterval), "2026.09", enqueue); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 4 || sent[2].ID != sent[0].ID {
		t.Fatalf("periodic repair did not reuse immutable row ID: %d", len(sent))
	}
	row.Timestamp = "1785700801000"
	corrected := buildCursorVendorSnapshot("account-a", []cursorVendorRow{row}, start, end, nil, cursorVendorShapeRecord{})
	if err := queueHistoricalCursorVendorSnapshot(corrected, "device", end.Add(cursorVendorRepairInterval+time.Hour), "2026.09", enqueue); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 6 || sent[4].ID == sent[0].ID {
		t.Fatalf("correction did not supersede old row: %d", len(sent))
	}
}

func TestHistoricalCursorSnapshotRetriesAfterFailedQueue(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	row := cursorVendorRow{Timestamp: "1785700800000", ConversationID: "c"}
	snapshot := buildCursorVendorSnapshot("account-a", []cursorVendorRow{row}, start, end, nil, cursorVendorShapeRecord{})
	fail := func(event.Event) bool { return false }
	if err := queueHistoricalCursorVendorSnapshot(snapshot, "device", end, "", fail); err == nil {
		t.Fatal("failed queue reported success")
	}
	var sent []event.Event
	ok := func(ev event.Event) bool { sent = append(sent, ev); return true }
	if err := queueHistoricalCursorVendorSnapshot(snapshot, "device", end.Add(time.Minute), "", ok); err != nil || len(sent) != 2 {
		t.Fatalf("retry sent %d err=%v", len(sent), err)
	}
}
