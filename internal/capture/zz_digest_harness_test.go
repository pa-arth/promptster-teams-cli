package capture

import (
	"encoding/json"
	"strings"
	"os"
	"testing"
	"time"
)

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }
func bl(v bool) *bool        { return &v }

// TestZZDumpDigestFixture is a harness, not an assertion: it emits the exact
// bytes the CLI would put on the wire for a fixed row set, so the backend's
// canonicalSnapshotSha256 can be run over the same rows and the two hex digests
// compared directly.
func TestZZDumpDigestFixture(t *testing.T) {
	out := os.Getenv("DIGEST_FIXTURE_OUT")
	if out == "" {
		t.Skip("no DIGEST_FIXTURE_OUT")
	}

	rows := []cursorVendorRow{
		// 1. ordinary interactive row, no new fields
		{
			Timestamp: "1787873137139", Model: "cursor-grok-4.6-high", Kind: "USAGE_EVENT_KIND_INCLUDED_IN_ULTRA",
			ConversationID: "3f0a1b2c-4d5e-6f70-8192-a3b4c5d6e7f8",
			IsHeadless:     bl(false), ChargedCents: f64(48.5714),
			TokenUsage: &cursorVendorTokenUsage{
				InputTokens: i64(104089), OutputTokens: i64(2371), CacheReadTokens: i64(1288331), TotalCents: f64(35.6402),
			},
			IsTokenBasedCall: bl(true), IsChargeable: bl(false),
			OwningUser: "359430439", SubscriptionProductID: "prod_ultra",
		},
		// 2. automation row carrying ALL FOUR new fields
		{
			Timestamp: "1787873200000", Model: "grok-bot-automation", Kind: "USAGE_EVENT_KIND_INCLUDED_IN_ULTRA",
			ConversationID: "sand-subagent-9c1d2e3f-4a5b-6c7d-8e9f-0a1b2c3d4e5f",
			IsHeadless:     bl(false), ChargedCents: f64(0.5714),
			TokenUsage: &cursorVendorTokenUsage{
				InputTokens: i64(31), OutputTokens: i64(0), CacheReadTokens: i64(999999), CacheWriteTokens: i64(77777), TotalCents: f64(0.0001),
			},
			IsTokenBasedCall: bl(true), IsChargeable: bl(true),
			OwningUser: "359430439", SubscriptionProductID: "prod_ultra",
			CloudAgentID: "ca_abc123", AutomationID: "au_xyz789", ServiceAccountID: "sa_qqq000",
		},
		// 3. TWIN of 2, differing ONLY in the four unhashed fields — proves the
		//    admitted collision and that neither side hashes them.
		{
			Timestamp: "1787873200000", Model: "grok-bot-automation", Kind: "USAGE_EVENT_KIND_INCLUDED_IN_ULTRA",
			ConversationID: "sand-subagent-9c1d2e3f-4a5b-6c7d-8e9f-0a1b2c3d4e5f",
			IsHeadless:     bl(false), ChargedCents: f64(0.5714),
			TokenUsage: &cursorVendorTokenUsage{
				InputTokens: i64(31), OutputTokens: i64(0), CacheReadTokens: i64(999999), CacheWriteTokens: i64(0), TotalCents: f64(0.0001),
			},
			IsTokenBasedCall: bl(true), IsChargeable: bl(true),
			OwningUser: "359430439", SubscriptionProductID: "prod_ultra",
			CloudAgentID: "", AutomationID: "", ServiceAccountID: "",
		},
		// 4. everything the vendor may omit, omitted
		{
			Timestamp: "1787873300000", Model: "", Kind: "",
			ConversationID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			TokenUsage:     nil,
		},
		// 5. TokenUsage present but every counter nil
		{
			Timestamp: "1787873400000", Model: "default", Kind: "USAGE_EVENT_KIND_INCLUDED_IN_ULTRA",
			ConversationID: "bbbbbbbb-cccc-dddd-eeee-ffffffffffff",
			IsHeadless:     bl(true), TokenUsage: &cursorVendorTokenUsage{},
			IsTokenBasedCall: bl(false), IsChargeable: bl(false),
			OwningUser: "359430439", SubscriptionProductID: "prod_ultra",
		},
		// 6. owningUser that looks like an email — must be dropped on BOTH sides
		{
			Timestamp: "1787873500000", Model: "gpt-5.5-high", Kind: "USAGE_EVENT_KIND_USAGE_BASED",
			ConversationID: "cccccccc-dddd-eeee-ffff-000000000000",
			IsHeadless:     bl(false), ChargedCents: f64(0),
			TokenUsage: &cursorVendorTokenUsage{
				InputTokens: i64(0), OutputTokens: i64(0), CacheReadTokens: i64(0), CacheWriteTokens: i64(0), TotalCents: f64(0),
			},
			IsTokenBasedCall: bl(false), IsChargeable: bl(true),
			OwningUser: "someone@ops.ai", SubscriptionProductID: "prod_ultra",
		},
		// 7. FLOAT FORMATTING TRAPS. Go floatKey is FormatFloat('g',-1); the
		//    backend is String(number). These are the values where the two
		//    formats are known to be able to diverge.
		{
			Timestamp: "1787873600000", Model: "float-trap-large", Kind: "k",
			ConversationID: "dddddddd-eeee-ffff-0000-111111111111",
			ChargedCents:   f64(1234567), // Go 'g' -> "1.234567e+06"; JS -> "1234567"
			TokenUsage:     &cursorVendorTokenUsage{TotalCents: f64(0.00001)}, // Go -> "1e-05"; JS -> "0.00001"
			OwningUser:     "359430439", SubscriptionProductID: "prod_ultra",
		},
		// 8. float traps, second band
		{
			Timestamp: "1787873700000", Model: "float-trap-small", Kind: "k",
			ConversationID: "eeeeeeee-ffff-0000-1111-222222222222",
			ChargedCents:   f64(1e21),
			TokenUsage:     &cursorVendorTokenUsage{TotalCents: f64(-0.5), InputTokens: i64(9007199254740993)},
			OwningUser:     "359430439", SubscriptionProductID: "prod_ultra",
		},
		// 9. unicode + separator-adjacent content in a string field
		{
			Timestamp: "1787873800000", Model: "モデル / claude-opus-5-low", Kind: "k",
			ConversationID: "ffffffff-0000-1111-2222-333333333333",
			ChargedCents:   f64(0.1 + 0.2),
			TokenUsage:     &cursorVendorTokenUsage{InputTokens: i64(-1), TotalCents: f64(100000)},
			IsHeadless:     bl(false),
			OwningUser:     "359430439", SubscriptionProductID: "prod ultra",
		},
	}

	if os.Getenv("DIGEST_NO_TRAPS") != "" {
		kept := rows[:0]
		for _, r := range rows {
			if r.Model == "float-trap-large" || r.Model == "float-trap-small" {
				continue
			}
			kept = append(kept, r)
		}
		rows = kept
	}

	cycleStart := time.Date(2026, 9, 2, 20, 9, 35, 0, time.UTC)
	cycleEnd := time.Date(2026, 10, 2, 20, 9, 35, 0, time.UTC)
	snap := buildCursorVendorSnapshot(rows, cycleStart, cycleEnd, nil, cursorVendorShapeRecord{HTTPStatus: 200})
	events := snap.rowEvents("dev-7b690c46f368ceda")

	datas := make([]interface{}, 0, len(events))
	for _, e := range events {
		datas = append(datas, e.Data)
	}
	lines := make([]string, 0, len(snap.Rows))
	for _, r := range snap.Rows {
		lines = append(lines, strings.ReplaceAll(canonicalRowLine(r), cursorVendorFieldSep, " | "))
	}
	payload := map[string]interface{}{
		"canonicalLines": lines,
		"snapshotId":             snap.SnapshotID,
		"contentSha256":          snap.ContentSha256,
		"rowCount":               len(snap.Rows),
		"billingCycleStartsAtMs": cycleStart.UnixMilli(),
		"billingCycleResetsAtMs": cycleEnd.UnixMilli(),
		"eventData":              datas,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("contentSha256=%s snapshotId=%s rows=%d", snap.ContentSha256, snap.SnapshotID, len(snap.Rows))
}
