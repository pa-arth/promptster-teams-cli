package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyWindowMinutes(t *testing.T) {
	cases := []struct {
		m    float64
		want windowRole
	}{
		{299, windowFiveHour},  // real Codex 5h value
		{300, windowFiveHour},  // nominal 5h
		{10079, windowWeekly},  // real Codex weekly value
		{10080, windowWeekly},  // nominal 7d
		{43800, windowUnknown}, // real team-plan monthly window — must NOT map
		{0, windowUnknown},     //
		{1440, windowUnknown},  // daily — neither
	}
	for _, c := range cases {
		if got := classifyWindowMinutes(c.m); got != c.want {
			t.Errorf("classifyWindowMinutes(%v) = %v, want %v", c.m, got, c.want)
		}
	}
}

// TestMapCodexRateLimits_ByWindowMinutes proves the mapping keys on
// window_minutes and NOT key order: primary=weekly, secondary=5h reversed.
func TestMapCodexRateLimits_ByWindowMinutes(t *testing.T) {
	rl := map[string]interface{}{
		// Deliberately REVERSED: the 5h window is in `secondary`, the weekly in
		// `primary`. A key-order mapping would get both backwards.
		"primary":   map[string]interface{}{"used_percent": 40.0, "window_minutes": 10079.0, "resets_at": 2000000000.0},
		"secondary": map[string]interface{}{"used_percent": 12.0, "window_minutes": 299.0, "resets_at": 1000000000.0},
	}
	r := mapCodexRateLimits(rl, 1500000000)
	if r.FiveHourPct == nil || *r.FiveHourPct != 12.0 {
		t.Errorf("fiveHourPct = %v, want 12 (from the secondary/5h window)", r.FiveHourPct)
	}
	if r.WeeklyPct == nil || *r.WeeklyPct != 40.0 {
		t.Errorf("weeklyPct = %v, want 40 (from the primary/weekly window)", r.WeeklyPct)
	}
	if r.FiveHourResetsAt == nil || *r.FiveHourResetsAt != 1000000000 {
		t.Errorf("fiveHourResetsAt = %v, want 1000000000", r.FiveHourResetsAt)
	}
	if r.WeeklyResetsAt == nil || *r.WeeklyResetsAt != 2000000000 {
		t.Errorf("weeklyResetsAt = %v, want 2000000000", r.WeeklyResetsAt)
	}
}

// TestMapCodexRateLimits_ResetsInSecondsCountdown proves the documented
// countdown shape (contract.md §3) is converted to an absolute epoch on-device.
func TestMapCodexRateLimits_ResetsInSecondsCountdown(t *testing.T) {
	rl := map[string]interface{}{
		"primary": map[string]interface{}{"used_percent": 17.0, "window_minutes": 300.0, "resets_in_seconds": 3600.0},
	}
	observedAt := int64(1700000000)
	r := mapCodexRateLimits(rl, observedAt)
	if r.FiveHourResetsAt == nil {
		t.Fatal("fiveHourResetsAt absent; want observedAt + 3600")
	}
	if *r.FiveHourResetsAt != observedAt+3600 {
		t.Errorf("fiveHourResetsAt = %d, want %d", *r.FiveHourResetsAt, observedAt+3600)
	}
}

// TestMapCodexRateLimits_ResetsAtWinsOverCountdown: when both are present, the
// already-absolute resets_at is used verbatim (no double-conversion).
func TestMapCodexRateLimits_ResetsAtWinsOverCountdown(t *testing.T) {
	rl := map[string]interface{}{
		"primary": map[string]interface{}{
			"used_percent": 5.0, "window_minutes": 300.0,
			"resets_at": 1900000000.0, "resets_in_seconds": 3600.0,
		},
	}
	r := mapCodexRateLimits(rl, 1700000000)
	if r.FiveHourResetsAt == nil || *r.FiveHourResetsAt != 1900000000 {
		t.Errorf("fiveHourResetsAt = %v, want 1900000000 (absolute resets_at)", r.FiveHourResetsAt)
	}
}

// TestMapCodexRateLimits_MonthlyWindowOmitted: the real team-plan shape (a single
// 43800-minute window) maps to NOTHING — absent, not zero.
func TestMapCodexRateLimits_MonthlyWindowOmitted(t *testing.T) {
	rl := map[string]interface{}{
		"primary":   map[string]interface{}{"used_percent": 17.0, "window_minutes": 43800.0, "resets_at": 1787369300.0},
		"secondary": nil,
	}
	r := mapCodexRateLimits(rl, 1700000000)
	if !r.empty() {
		t.Errorf("expected empty reading for a monthly-only window, got %+v", r)
	}
	if r.FiveHourPct != nil || r.WeeklyPct != nil {
		t.Error("monthly window must not populate a 5h or weekly percentage")
	}
}

// TestMapCodexRateLimits_NaNOmitted: a malformed percentage is omitted, never 0.
func TestMapCodexRateLimits_NaNOmitted(t *testing.T) {
	nan := jsonNaN(t)
	rl := map[string]interface{}{
		"primary": map[string]interface{}{"used_percent": nan, "window_minutes": 300.0, "resets_at": 1900000000.0},
	}
	r := mapCodexRateLimits(rl, 1700000000)
	if r.FiveHourPct != nil {
		t.Errorf("NaN percentage must be omitted, got %v", *r.FiveHourPct)
	}
	// The reset is still valid, so it should survive.
	if r.FiveHourResetsAt == nil {
		t.Error("valid reset should survive alongside a dropped NaN percentage")
	}
}

// TestMapCodexRateLimits_GenuineZeroKept: a real 0% is emitted as 0 (0 != absent).
func TestMapCodexRateLimits_GenuineZeroKept(t *testing.T) {
	rl := map[string]interface{}{
		"primary": map[string]interface{}{"used_percent": 0.0, "window_minutes": 300.0, "resets_at": 1900000000.0},
	}
	r := mapCodexRateLimits(rl, 1700000000)
	if r.FiveHourPct == nil {
		t.Fatal("a genuine 0% must be kept, not dropped as absent")
	}
	if *r.FiveHourPct != 0 {
		t.Errorf("fiveHourPct = %v, want 0", *r.FiveHourPct)
	}
}

// TestLatestCodexWindowReading_PicksLatestByTimestamp writes two rollout files
// with different token_count timestamps and confirms the newest wins.
func TestLatestCodexWindowReading_PicksLatestByTimestamp(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions", "2026", "07", "23")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}

	older := rolloutTokenCount("2026-07-23T10:00:00.000Z", 10.0, 300.0, 1900000000)
	newer := rolloutTokenCount("2026-07-23T12:00:00.000Z", 55.0, 300.0, 1950000000)
	writeRollout(t, filepath.Join(sessions, "rollout-2026-07-23T10-00-00-aaaaaaaa-1111-2222-3333-444444444444.jsonl"), older)
	writeRollout(t, filepath.Join(sessions, "rollout-2026-07-23T12-00-00-bbbbbbbb-1111-2222-3333-444444444444.jsonl"), newer)

	r, session, ok := latestCodexWindowReading(filepath.Join(dir, "sessions"), time.Now().Add(-24*time.Hour))
	if !ok {
		t.Fatal("expected a reading")
	}
	if r.FiveHourPct == nil || *r.FiveHourPct != 55.0 {
		t.Errorf("fiveHourPct = %v, want 55 (the newer file)", r.FiveHourPct)
	}
	if session == "" {
		t.Error("expected a rollout session id")
	}
}

// TestLatestCodexWindowReading_MultipleTokenCountsInOneFile: the LAST
// token_count-with-rate_limits in a file wins.
func TestLatestCodexWindowReading_MultipleTokenCountsInOneFile(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		rolloutTokenCount("2026-07-23T10:00:00.000Z", 10.0, 300.0, 1900000000),
		`{"timestamp":"2026-07-23T10:30:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"noise"}]}}`,
		rolloutTokenCount("2026-07-23T11:00:00.000Z", 80.0, 300.0, 1950000000),
	}
	writeRollout(t, filepath.Join(sessions, "rollout-2026-07-23T10-00-00-cccccccc-1111-2222-3333-444444444444.jsonl"), lines...)

	r, _, ok := latestCodexWindowReading(sessions, time.Now().Add(-24*time.Hour))
	if !ok {
		t.Fatal("expected a reading")
	}
	if r.FiveHourPct == nil || *r.FiveHourPct != 80.0 {
		t.Errorf("fiveHourPct = %v, want 80 (the later token_count in the file)", r.FiveHourPct)
	}
}

// TestLatestCodexWindowReading_PrivacyNoPromptLeak feeds a rollout whose prompt
// lines contain a secret and asserts the secret NEVER appears in the emitted
// event JSON — only the window scalars leave.
func TestLatestCodexWindowReading_PrivacyNoPromptLeak(t *testing.T) {
	const secret = "sk-super-secret-value-DO-NOT-LEAK"
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"timestamp":"2026-07-23T09:00:00.000Z","type":"session_meta","payload":{"id":"deadbeef","cwd":"/home/me/repo"}}`,
		`{"timestamp":"2026-07-23T09:30:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"my key is ` + secret + `"}]}}`,
		rolloutTokenCount("2026-07-23T10:00:00.000Z", 33.0, 300.0, 1950000000),
	}
	writeRollout(t, filepath.Join(sessions, "rollout-2026-07-23T09-00-00-dddddddd-1111-2222-3333-444444444444.jsonl"), lines...)

	r, sid, ok := latestCodexWindowReading(sessions, time.Now().Add(-24*time.Hour))
	if !ok {
		t.Fatal("expected a reading")
	}
	e := buildWindowUsageEvent(providerCodex, r, time.Now().Unix(), sid, "device-hash")
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("SECRET LEAKED into the windowUsage event: %s", raw)
	}
	if strings.Contains(string(raw), "/home/me/repo") {
		t.Fatalf("cwd path leaked into the windowUsage event: %s", raw)
	}
}

// TestBuildWindowUsageEvent_AbsentOmitted: nil window fields never appear in Data.
func TestBuildWindowUsageEvent_AbsentOmitted(t *testing.T) {
	five := 20.0
	r := windowReading{FiveHourPct: &five, ObservedAt: 1700000000} // weekly + resets absent
	e := buildWindowUsageEvent(providerCodex, r, 1700000005, "sess", "dev")
	data := e.Data.(map[string]interface{})
	if _, ok := data["weeklyPct"]; ok {
		t.Error("absent weeklyPct must be omitted from Data, not present")
	}
	if _, ok := data["fiveHourResetsAt"]; ok {
		t.Error("absent fiveHourResetsAt must be omitted")
	}
	if data["fiveHourPct"] != 20.0 {
		t.Errorf("fiveHourPct = %v, want 20", data["fiveHourPct"])
	}
	if data["provider"] != providerCodex {
		t.Errorf("provider = %v, want codex", data["provider"])
	}
	if data["observedAt"].(int64) != 1700000000 || data["capturedAt"].(int64) != 1700000005 {
		t.Errorf("timestamps wrong: %v / %v", data["observedAt"], data["capturedAt"])
	}
}

// TestBuildWindowUsageEvent_DeterministicID: identical readings share an id
// (idempotent resend); a changed reading gets a new id.
func TestBuildWindowUsageEvent_DeterministicID(t *testing.T) {
	five := 20.0
	r := windowReading{FiveHourPct: &five, ObservedAt: 1700000000}
	a := buildWindowUsageEvent(providerCodex, r, 111, "s", "dev")
	b := buildWindowUsageEvent(providerCodex, r, 222, "s", "dev") // different capturedAt/sessionArgs
	if a.ID != b.ID {
		t.Errorf("same reading should share id: %s != %s", a.ID, b.ID)
	}
	six := 21.0
	r2 := windowReading{FiveHourPct: &six, ObservedAt: 1700000000}
	c := buildWindowUsageEvent(providerCodex, r2, 111, "s", "dev")
	if a.ID == c.ID {
		t.Error("a changed reading should get a new id")
	}
}

// --- helpers -----------------------------------------------------------------

func rolloutTokenCount(ts string, usedPct, windowMin float64, resetsAt int64) string {
	m := map[string]interface{}{
		"timestamp": ts,
		"type":      "event_msg",
		"payload": map[string]interface{}{
			"type": "token_count",
			"info": map[string]interface{}{"total_token_usage": map[string]interface{}{"total_tokens": 100}},
			"rate_limits": map[string]interface{}{
				"primary": map[string]interface{}{
					"used_percent": usedPct, "window_minutes": windowMin, "resets_at": resetsAt,
				},
			},
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func writeRollout(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// jsonNaN produces a float64 NaN the way a decoder would surface one, so the
// omission guard is exercised against a real NaN rather than a literal.
func jsonNaN(t *testing.T) float64 {
	t.Helper()
	// math.NaN via a computed value avoids importing math in the test just for this.
	zero := 0.0
	return zero / zero
}
