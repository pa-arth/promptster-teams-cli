package capture

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

func vendorV2Data(ev event.Event) map[string]interface{} { return ev.Data.(map[string]interface{}) }
func TestCursorVendorV2SendsOnlyNovelRowsAndManifest(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	at := start.Add(12 * time.Hour)
	a := cursorVendorRow{Timestamp: "1788200000000", ConversationID: "a"}
	b := cursorVendorRow{Timestamp: "1788300000000", ConversationID: "b"}
	c := cursorVendorRow{Timestamp: "1788100000000", ConversationID: "c"}
	snapshot := func(account string, rows ...cursorVendorRow) cursorVendorSnapshot {
		return buildCursorVendorSnapshot(account, rows, start, start.Add(30*24*time.Hour), nil, cursorVendorShapeRecord{})
	}
	var sent []event.Event
	enqueue := func(ev event.Event) bool { sent = append(sent, ev); return true }
	if !queueCursorVendorSnapshotV2(snapshot("account-a", a, b), "device-a", at, "", enqueue) || len(sent) != 3 {
		t.Fatalf("first poll sent %d", len(sent))
	}
	poolID := vendorV2Data(sent[0])["snapshotId"]
	if vendorV2Data(sent[0])["rowHash"] == nil {
		t.Fatal("row has no content identity")
	}
	if _, ok := vendorV2Data(sent[0])["accountRef"]; ok {
		t.Fatal("pool row carries account attribution")
	}
	manifest, err := base64.StdEncoding.DecodeString(vendorV2Data(sent[2])["manifest"].(string))
	if err != nil || len(manifest) != 32 {
		t.Fatalf("first manifest bytes %d, err %v", len(manifest), err)
	}
	sent = nil
	queueCursorVendorSnapshotV2(snapshot("account-a", a, b), "device-a", at.Add(5*time.Minute), "", enqueue)
	if len(sent) != 1 {
		t.Fatalf("unchanged poll sent %d events", len(sent))
	}
	if _, ok := vendorV2Data(sent[0])["manifest"]; ok {
		t.Fatal("unchanged poll restaged manifest")
	}
	sent = nil
	queueCursorVendorSnapshotV2(snapshot("account-a", c, a, b), "device-a", at.Add(15*time.Minute), "", enqueue)
	if len(sent) != 2 || vendorV2Data(sent[0])["snapshotId"] != poolID {
		t.Fatalf("one new row sent %+v", sent)
	}
	sent = nil
	changed := a
	changed.Model = "corrected"
	queueCursorVendorSnapshotV2(snapshot("account-a", c, changed, b), "device-a", at.Add(30*time.Minute), "", enqueue)
	if len(sent) != 2 {
		t.Fatalf("one corrected row sent %d events", len(sent))
	}
	sent = nil
	queueCursorVendorSnapshotV2(snapshot("account-b", c, changed, b), "device-a", at.Add(45*time.Minute), "", enqueue)
	if len(sent) != 1 || vendorV2Data(sent[0])["accountRef"] != "account-b" {
		t.Fatalf("account switch sent %+v", sent)
	}
}

func TestCursorVendorV2RetryAfterPartialAppendCannotChangeIdentity(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := cursorVendorRow{Timestamp: "1788200000000", ConversationID: "a"}
	b := cursorVendorRow{Timestamp: "1788300000000", ConversationID: "b"}
	c := cursorVendorRow{Timestamp: "1788100000000", ConversationID: "c"}
	snap := func(rows ...cursorVendorRow) cursorVendorSnapshot {
		return buildCursorVendorSnapshot("account-a", rows, start, start.Add(30*24*time.Hour), nil, cursorVendorShapeRecord{})
	}
	first := map[string]string{}
	calls := 0
	if queueCursorVendorSnapshotV2(snap(a, b), "device-a", start.Add(time.Hour), "", func(ev event.Event) bool {
		if ev.Kind == "cursorVendorUsage" {
			first[vendorV2Data(ev)["rowHash"].(string)] = ev.ID
		}
		calls++
		return calls != 2
	}) {
		t.Fatal("partial enqueue reported success")
	}
	retried := map[string]string{}
	queueCursorVendorSnapshotV2(snap(c, a, b), "device-a", start.Add(2*time.Hour), "", func(ev event.Event) bool {
		if ev.Kind == "cursorVendorUsage" {
			retried[vendorV2Data(ev)["rowHash"].(string)] = ev.ID
		}
		return true
	})
	for hash, id := range first {
		if retried[hash] != id {
			t.Fatalf("row %s changed id after partial append", hash)
		}
	}
}

func TestCursorVendorV2DuplicateRowsHaveOneStoredIdentity(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	row := cursorVendorRow{Timestamp: "1788200000000", ConversationID: "a"}
	snap := buildCursorVendorSnapshot("account-a", []cursorVendorRow{row, row}, start, start.Add(30*24*time.Hour), nil, cursorVendorShapeRecord{})
	var sent []event.Event
	queueCursorVendorSnapshotV2(snap, "device-a", start.Add(time.Hour), "", func(ev event.Event) bool { sent = append(sent, ev); return true })
	if len(sent) != 2 {
		t.Fatalf("identical rows sent %d events, want one row and completion", len(sent))
	}
	manifest, _ := base64.StdEncoding.DecodeString(vendorV2Data(sent[1])["manifest"].(string))
	if len(manifest) != 32 || string(manifest[:16]) != string(manifest[16:]) {
		t.Fatal("manifest lost duplicate multiplicity")
	}
}

func TestCursorVendorV2ManifestChangesWhenSnapshotDigestDoesNot(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old := cursorVendorRow{Timestamp: "1788200000000", ConversationID: "a"}
	changed := old
	changed.CloudAgentID = "agent-a"
	first := buildCursorVendorSnapshot("account-a", []cursorVendorRow{old}, start, start.Add(30*24*time.Hour), nil, cursorVendorShapeRecord{})
	second := buildCursorVendorSnapshot("account-a", []cursorVendorRow{changed}, start, start.Add(30*24*time.Hour), nil, cursorVendorShapeRecord{})
	if first.SnapshotID != second.SnapshotID {
		t.Fatal("fixture changed canonical snapshot id")
	}
	var sent []event.Event
	enqueue := func(ev event.Event) bool { sent = append(sent, ev); return true }
	queueCursorVendorSnapshotV2(first, "device-a", start.Add(time.Hour), "", enqueue)
	oldManifestHash := vendorV2Data(sent[len(sent)-1])["manifestSha256"]
	sent = nil
	queueCursorVendorSnapshotV2(second, "device-a", start.Add(2*time.Hour), "", enqueue)
	if len(sent) != 2 || vendorV2Data(sent[1])["manifest"] == nil || vendorV2Data(sent[1])["manifestSha256"] == oldManifestHash {
		t.Fatal("noncanonical row change did not send a new row and manifest")
	}
}

func TestCursorVendorCrossLanguageDigestGolden(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tiny := 0.0000001
	rows := []cursorVendorRow{
		{Timestamp: "1788200000000", ConversationID: "alpha", ChargedCents: &tiny},
		{Timestamp: "1788300000000", ConversationID: "beta"},
	}
	got := buildCursorVendorSnapshot("account-a", rows, start, start.Add(30*24*time.Hour), nil, cursorVendorShapeRecord{}).ContentSha256
	if got != "73144f5efdce4b50a7e317f490fa1eafbfd37fe3bc4f0299334c4a4892aae198" {
		t.Fatalf("canonical digest = %s", got)
	}
}
