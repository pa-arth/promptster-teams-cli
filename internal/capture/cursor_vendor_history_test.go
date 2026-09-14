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
}

func TestHistoricalCursorSnapshotQueuesOnceAndReplacesCorrection(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	row := cursorVendorRow{Timestamp: "1785700800000", ConversationID: "conversation-1", Model: "composer-2.5"}
	snapshot := buildCursorVendorSnapshot([]cursorVendorRow{row}, start, end, nil, cursorVendorShapeRecord{})
	snapshot.AccountRef = "account-a"
	var sent []event.Event
	enqueue := func(ev event.Event) bool { sent = append(sent, ev); return true }
	if err := queueHistoricalCursorVendorSnapshot(snapshot, "device", end, "2026.09", enqueue); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].Kind != "cursorVendorUsage" || sent[1].Kind != "cursorVendorSnapshot" {
		t.Fatalf("queued %d events", len(sent))
	}
	if err := queueHistoricalCursorVendorSnapshot(snapshot, "device", end.Add(time.Hour), "2026.09", enqueue); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("unchanged snapshot requeued: %d", len(sent))
	}
	if err := queueHistoricalCursorVendorSnapshot(snapshot, "device", end.Add(cursorVendorHistoryRepairInterval), "2026.09", enqueue); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 4 || sent[2].ID != sent[0].ID {
		t.Fatalf("periodic repair did not reuse immutable row ID: %d", len(sent))
	}
	row.Timestamp = "1785700801000"
	corrected := buildCursorVendorSnapshot([]cursorVendorRow{row}, start, end, nil, cursorVendorShapeRecord{})
	corrected.AccountRef = "account-a"
	if err := queueHistoricalCursorVendorSnapshot(corrected, "device", end.Add(2*time.Hour), "2026.09", enqueue); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 6 || sent[4].ID == sent[0].ID {
		t.Fatalf("correction did not supersede old row: %d", len(sent))
	}
}
