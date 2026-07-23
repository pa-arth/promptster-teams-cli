package capture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// statuslineTestEnv points CLAUDE_CONFIG_DIR (the User settings layer) and the
// promptster state dir at temp dirs so tests never touch the real machine.
func statuslineTestEnv(t *testing.T) (claudeDir, stateDir string) {
	t.Helper()
	claudeDir = t.TempDir()
	stateDir = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	return claudeDir, stateDir
}

func TestParseClaudeStatuslineBlob(t *testing.T) {
	blob := []byte(`{
		"session_id":"abc","model":{"id":"claude-x"},
		"transcript_path":"/home/me/.claude/projects/x/transcript.jsonl",
		"rate_limits":{
			"five_hour":{"used_percentage":42.5,"resets_at":1900000000},
			"seven_day":{"used_percentage":8.0,"resets_at":1950000000}
		}
	}`)
	r, ok := parseClaudeStatuslineBlob(blob, 1700000000)
	if !ok {
		t.Fatal("expected a reading")
	}
	if r.FiveHourPct == nil || *r.FiveHourPct != 42.5 {
		t.Errorf("fiveHourPct = %v, want 42.5", r.FiveHourPct)
	}
	if r.WeeklyPct == nil || *r.WeeklyPct != 8.0 {
		t.Errorf("weeklyPct = %v, want 8", r.WeeklyPct)
	}
	if r.FiveHourResetsAt == nil || *r.FiveHourResetsAt != 1900000000 {
		t.Errorf("fiveHourResetsAt = %v", r.FiveHourResetsAt)
	}
	if r.ObservedAt != 1700000000 {
		t.Errorf("observedAt = %d, want tick time", r.ObservedAt)
	}
}

func TestParseClaudeStatuslineBlob_AbsentWeeklyOmitted(t *testing.T) {
	blob := []byte(`{"rate_limits":{"five_hour":{"used_percentage":42.5,"resets_at":1900000000}}}`)
	r, ok := parseClaudeStatuslineBlob(blob, 1700000000)
	if !ok {
		t.Fatal("expected a reading")
	}
	if r.WeeklyPct != nil || r.WeeklyResetsAt != nil {
		t.Error("absent seven_day must yield no weekly fields (absent != zero)")
	}
}

func TestParseClaudeStatuslineBlob_NoRateLimits(t *testing.T) {
	blob := []byte(`{"session_id":"abc","model":{"id":"x"}}`)
	if _, ok := parseClaudeStatuslineBlob(blob, 1700000000); ok {
		t.Error("an API-key blob with no rate_limits must yield ok=false, not a zeroed reading")
	}
}

func TestParseClaudeStatuslineBlob_GenuineZeroKept(t *testing.T) {
	blob := []byte(`{"rate_limits":{"five_hour":{"used_percentage":0,"resets_at":1900000000}}}`)
	r, ok := parseClaudeStatuslineBlob(blob, 1700000000)
	if !ok || r.FiveHourPct == nil || *r.FiveHourPct != 0 {
		t.Errorf("a genuine 0%% must be kept as 0, got ok=%v pct=%v", ok, r.FiveHourPct)
	}
}

// TestEnableDisableFreshRoundTrip: no prior statusline → enable installs ours →
// disable removes the key, leaving the file as it started.
func TestEnableDisableFreshRoundTrip(t *testing.T) {
	claudeDir, _ := statuslineTestEnv(t)
	path := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := EnableStatusline()
	if err != nil {
		t.Fatal(err)
	}
	if !res.InstalledFresh {
		t.Errorf("expected InstalledFresh, got %+v", res)
	}
	if _, ok := readStatusLine(path); !ok {
		t.Fatal("shim not installed")
	}
	if eff := resolveEffectiveStatusLine(""); !eff.IsShim {
		t.Error("effective statusline should be our shim after enable")
	}

	if err := DisableStatusline(); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStatusLine(path); ok {
		t.Error("disable should remove the statusLine key we installed")
	}
}

// TestEnableDisableWrapRoundTrip: an existing statusline is wrapped and restored
// byte-equivalently (compared at the statusLine object level).
func TestEnableDisableWrapRoundTrip(t *testing.T) {
	claudeDir, _ := statuslineTestEnv(t)
	path := filepath.Join(claudeDir, "settings.json")
	pad := 2
	original := statusLineConfig{Type: "command", Command: "my-cool-statusline.sh --flag", Padding: &pad}
	writeSettings(t, path, map[string]interface{}{"statusLine": statusLineToMap(original), "otherKey": "keep-me"})

	res, err := EnableStatusline()
	if err != nil {
		t.Fatal(err)
	}
	if !res.WrappedExisting {
		t.Errorf("expected WrappedExisting, got %+v", res)
	}
	shim, ok := readStatusLine(path)
	if !ok || !isOurShim(shim.Command) {
		t.Fatal("shim not installed over the existing statusline")
	}
	if shim.Padding == nil || *shim.Padding != 2 {
		t.Error("padding from the wrapped statusline should be preserved on the shim")
	}
	// The unrelated key must survive.
	if m, _ := readSettingsMap(path); m["otherKey"] != "keep-me" {
		t.Error("enable clobbered an unrelated settings key")
	}

	if err := DisableStatusline(); err != nil {
		t.Fatal(err)
	}
	restored, ok := readStatusLine(path)
	if !ok {
		t.Fatal("disable should have restored the prior statusline")
	}
	if !reflect.DeepEqual(statusLineToMap(restored), statusLineToMap(original)) {
		t.Errorf("statusLine not restored verbatim:\n got %+v\nwant %+v", restored, original)
	}
}

// TestEnableIdempotent: enabling twice does not double-wrap.
func TestEnableIdempotent(t *testing.T) {
	claudeDir, _ := statuslineTestEnv(t)
	path := filepath.Join(claudeDir, "settings.json")
	writeSettings(t, path, map[string]interface{}{"statusLine": statusLineToMap(statusLineConfig{Type: "command", Command: "orig.sh"})})

	if _, err := EnableStatusline(); err != nil {
		t.Fatal(err)
	}
	first, _ := readStatusLine(path)
	res, err := EnableStatusline()
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyEnabled {
		t.Errorf("second enable should report AlreadyEnabled, got %+v", res)
	}
	second, _ := readStatusLine(path)
	if first.Command != second.Command {
		t.Error("second enable changed the shim command (double-wrap?)")
	}
	// Disable must still restore the ORIGINAL, not the shim.
	if err := DisableStatusline(); err != nil {
		t.Fatal(err)
	}
	restored, _ := readStatusLine(path)
	if restored.Command != "orig.sh" {
		t.Errorf("restored = %q, want orig.sh (idempotent enable must not lose the prior)", restored.Command)
	}
}

// TestEnableRewrapsChangedCommand: if the engineer swaps their statusline after
// we wrapped, a re-enable wraps the NEW one rather than dropping it.
func TestEnableRewrapsChangedCommand(t *testing.T) {
	claudeDir, _ := statuslineTestEnv(t)
	path := filepath.Join(claudeDir, "settings.json")
	writeSettings(t, path, map[string]interface{}{"statusLine": statusLineToMap(statusLineConfig{Type: "command", Command: "first.sh"})})
	if _, err := EnableStatusline(); err != nil {
		t.Fatal(err)
	}
	// Engineer replaces the statusline with a new command (overwriting our shim).
	writeSettings(t, path, map[string]interface{}{"statusLine": statusLineToMap(statusLineConfig{Type: "command", Command: "second.sh"})})

	res, err := EnableStatusline()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rewrapped {
		t.Errorf("expected Rewrapped, got %+v", res)
	}
	if err := DisableStatusline(); err != nil {
		t.Fatal(err)
	}
	restored, _ := readStatusLine(path)
	if restored.Command != "second.sh" {
		t.Errorf("restored = %q, want second.sh (re-wrap must preserve the NEW command)", restored.Command)
	}
}

// TestEffectiveStatuslineProjectShadow: a project-layer statusLine outranks our
// user-layer shim, and doctor reports the shadow.
func TestEffectiveStatuslineProjectShadow(t *testing.T) {
	claudeDir, _ := statuslineTestEnv(t)
	// User layer: our shim.
	writeSettings(t, filepath.Join(claudeDir, "settings.json"),
		map[string]interface{}{"statusLine": statusLineToMap(statusLineConfig{Type: "command", Command: shimCommand()})})
	// Record that we installed it, so doctor knows we expect to run.
	_ = saveStatuslinePrior(statuslinePriorRecord{Wrapped: true})

	// Project layer: a different statusline.
	projectDir := t.TempDir()
	writeSettings(t, filepath.Join(projectDir, ".claude", "settings.json"),
		map[string]interface{}{"statusLine": statusLineToMap(statusLineConfig{Type: "command", Command: "project-line.sh"})})

	eff := resolveEffectiveStatusLine(projectDir)
	if eff.Layer != "project" {
		t.Errorf("effective layer = %q, want project (it should shadow user)", eff.Layer)
	}
	if eff.IsShim {
		t.Error("effective statusline should NOT be our shim when a project layer shadows it")
	}

	lines := StatuslineDoctor(projectDir)
	if len(lines) != 1 || !lines[0].Warn || !strings.Contains(strings.ToLower(lines[0].Text), "shadow") {
		t.Errorf("doctor should warn about the shadow, got %+v", lines)
	}
}

// TestStatuslineDoctorActive: when our shim wins, doctor reports OK.
func TestStatuslineDoctorActive(t *testing.T) {
	claudeDir, _ := statuslineTestEnv(t)
	writeSettings(t, filepath.Join(claudeDir, "settings.json"),
		map[string]interface{}{"statusLine": statusLineToMap(statusLineConfig{Type: "command", Command: shimCommand()})})
	_ = saveStatuslinePrior(statuslinePriorRecord{Wrapped: true})

	lines := StatuslineDoctor("")
	if len(lines) != 1 || !lines[0].OK {
		t.Errorf("doctor should report OK when the shim wins, got %+v", lines)
	}
}

// TestShimPassesPriorStdoutThrough: the shim runs the wrapped command and passes
// its stdout through, while spooling the window reading.
func TestShimPassesPriorStdoutThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -c")
	}
	_, stateDir := statuslineTestEnv(t)
	// Store a prior command that echoes a marker.
	if err := saveStatuslinePrior(statuslinePriorRecord{
		Wrapped: true,
		Prior:   &statusLineConfig{Type: "command", Command: "printf 'PRIOR-LINE-OK'"},
	}); err != nil {
		t.Fatal(err)
	}

	blob := []byte(`{"session_id":"s","rate_limits":{"five_hour":{"used_percentage":50,"resets_at":1900000000}}}`)
	out := runPriorStatusline(blob)
	if !strings.Contains(string(out), "PRIOR-LINE-OK") {
		t.Errorf("prior stdout not passed through: %q", out)
	}

	// The shim's spool step is exercised separately; here confirm the spool round
	// trips when we write it directly (the shim calls writeClaudeWindowSpool).
	r, _ := parseClaudeStatuslineBlob(blob, 1700000000)
	if err := writeClaudeWindowSpool(r); err != nil {
		t.Fatal(err)
	}
	spoolPath := filepath.Join(stateDir, "claude-window.json")
	if _, err := os.Stat(spoolPath); err != nil {
		t.Fatalf("spool not written: %v", err)
	}
}

// TestShimPrivacyNoContentInSpool: the blob's session/model/transcript fields
// never reach the spool — only the window scalars.
func TestShimPrivacyNoContentInSpool(t *testing.T) {
	_, stateDir := statuslineTestEnv(t)
	const secretPath = "/home/me/.claude/projects/secret-repo/transcript.jsonl"
	blob := []byte(`{"session_id":"SESSION-SECRET","model":{"id":"claude-secret-model"},"transcript_path":"` + secretPath + `","rate_limits":{"five_hour":{"used_percentage":50,"resets_at":1900000000}}}`)
	r, ok := parseClaudeStatuslineBlob(blob, 1700000000)
	if !ok {
		t.Fatal("expected a reading")
	}
	if err := writeClaudeWindowSpool(r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "claude-window.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"SESSION-SECRET", "claude-secret-model", secretPath, "transcript"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Fatalf("content leaked into spool (%q): %s", leak, data)
		}
	}
}

// TestShimSpoolDrainOnce: reading the spool removes it (emit-exactly-once).
func TestShimSpoolDrainOnce(t *testing.T) {
	statuslineTestEnv(t)
	five := 12.0
	if err := writeClaudeWindowSpool(windowReading{FiveHourPct: &five, ObservedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readClaudeWindowSpool(); !ok {
		t.Fatal("first drain should return the reading")
	}
	if _, ok := readClaudeWindowSpool(); ok {
		t.Error("second drain should find nothing — the spool must be consumed once")
	}
}

// --- helpers -----------------------------------------------------------------

func writeSettings(t *testing.T, path string, m map[string]interface{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
