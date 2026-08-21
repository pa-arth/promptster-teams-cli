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

// An API-key blob is an OBSERVED ABSENCE, and that is the whole point of the
// state: the shim demonstrably ran and Claude reported no window. Returning
// ok=false here — as this used to — made it byte-identical to a shim that never
// ran, and the surface printed "pay-as-you-go" for both.
func TestParseClaudeStatuslineBlob_NoRateLimits(t *testing.T) {
	blob := []byte(`{"session_id":"abc","model":{"id":"x"}}`)
	r, ok := parseClaudeStatuslineBlob(blob, 1700000000)
	if !ok {
		t.Fatal("an API-key blob must yield an OBSERVED absence, not nothing")
	}
	if r.SignalState != signalProviderAbsent {
		t.Errorf("signalState = %q, want %q", r.SignalState, signalProviderAbsent)
	}
	if r.ObservedAt != 1700000000 {
		t.Errorf("observedAt = %d, want the tick time", r.ObservedAt)
	}
	// ABSENT != 0. An absence carrying a percentage — even 0 — would render a
	// gauge for an account that has no window at all.
	if !r.empty() {
		t.Errorf("an absence must carry no window field at all, got %+v", r)
	}
}

// `rate_limits` present but carrying nothing usable is the same fact as no
// object: asked, told nothing. Claude reports only the two windows the contract
// names, so there is no third span hiding in an empty object.
func TestParseClaudeStatuslineBlob_EmptyRateLimitsIsAnAbsence(t *testing.T) {
	blob := []byte(`{"rate_limits":{}}`)
	r, ok := parseClaudeStatuslineBlob(blob, 1700000000)
	if !ok || r.SignalState != signalProviderAbsent || !r.empty() {
		t.Errorf("want an empty provider_absent reading, got ok=%v %+v", ok, r)
	}
}

// A SUBSCRIBER's first tick looks exactly like an API-key account's, and that is
// not a defect to fix here. Claude Code renders the status line before the
// session's first API response, when there is no window to report yet, so this
// blob — indistinguishable on the wire from the one above — comes from someone on
// a flat-fee plan.
//
// The shim reports what it observed and the emission stays identical. What must
// never happen is one of these becoming a billing claim, and that is settled in
// the backend's deriveNoSignalReason (an absence earns `usage_billed` only after
// it RECURS and Claude work is observed at or after it began), not by teaching
// this function to guess. Asserted here so a later reader who reaches for the
// obvious local fix finds the reason before the code.
func TestParseClaudeStatuslineBlob_PreFirstResponseIsStillAnAbsence(t *testing.T) {
	// A real pre-first-response blob: session and model present, cost zeroed, no
	// rate_limits yet. Same emission as a genuinely usage-billed account.
	blob := []byte(`{"session_id":"abc","model":{"id":"claude-opus-5"},"cost":{"total_cost_usd":0}}`)
	r, ok := parseClaudeStatuslineBlob(blob, 1700000000)
	if !ok {
		t.Fatal("a pre-first-response blob is still an OBSERVED absence")
	}
	if r.SignalState != signalProviderAbsent {
		t.Errorf("signalState = %q, want %q", r.SignalState, signalProviderAbsent)
	}
	// The point of the test: the shim does NOT special-case this. If someone
	// suppresses it here, "subscriber before first response" and "shim never ran"
	// collapse back into one silence with five causes.
	if r.empty() != true {
		t.Errorf("an absence must carry no window field at all, got %+v", r)
	}
}

// A blob we could not read says NOTHING about the account. We assert an absence
// only from a payload we actually parsed.
func TestParseClaudeStatuslineBlob_UnparseableIsNotAnAbsence(t *testing.T) {
	if _, ok := parseClaudeStatuslineBlob([]byte("not json at all"), 1700000000); ok {
		t.Error("an unparseable blob must not be reported as an observed absence")
	}
}

// A real reading omits signalState entirely, so the normal path is byte-identical
// to what shipped before the field existed.
func TestBuildWindowUsageEvent_ReportedOmitsSignalState(t *testing.T) {
	pct := 42.5
	e := buildWindowUsageEvent(providerClaudeCode,
		windowReading{FiveHourPct: &pct, ObservedAt: 1700000000}, 1700000001, "s", "d")
	data := e.Data.(map[string]interface{})
	if _, present := data["signalState"]; present {
		t.Error("a reported event must not carry signalState")
	}
	if len(data) != 4 { // provider, observedAt, capturedAt, fiveHourPct
		t.Errorf("unexpected keys on a reported event: %v", data)
	}
}

// An absence event carries the state and NO percentage key — not null, not 0.
func TestBuildWindowUsageEvent_AbsenceCarriesNoPercentageKeys(t *testing.T) {
	e := buildWindowUsageEvent(providerClaudeCode,
		absenceReading(signalProviderAbsent, 1700000000), 1700000001, "s", "d")
	data := e.Data.(map[string]interface{})
	if data["signalState"] != signalProviderAbsent {
		t.Errorf("signalState = %v", data["signalState"])
	}
	for _, k := range []string{"fiveHourPct", "weeklyPct", "fiveHourResetsAt", "weeklyResetsAt"} {
		if _, present := data[k]; present {
			t.Errorf("absence event must not carry %q at all (absent != 0)", k)
		}
	}
}

// An absence and a reading at the same observedAt are different facts and must
// not collapse onto one id.
func TestBuildWindowUsageEvent_AbsenceAndReadingHaveDistinctIDs(t *testing.T) {
	pct := 0.0
	reading := buildWindowUsageEvent(providerClaudeCode,
		windowReading{FiveHourPct: &pct, ObservedAt: 1700000000}, 1, "s", "d")
	absence := buildWindowUsageEvent(providerClaudeCode,
		absenceReading(signalProviderAbsent, 1700000000), 1, "s", "d")
	if reading.ID == absence.ID {
		t.Error("a genuine 0%% reading and an observed absence must not share an id")
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

	// TWO lines when the shim wins: the config claim, then the observation.
	// Asserted by count as well as content, because a doctor that silently
	// stops reporting the observation is the same class of regression the
	// observation exists to catch.
	lines := StatuslineDoctor("")
	if len(lines) != 2 {
		t.Fatalf("doctor should report the config line AND the observation line, got %+v", lines)
	}
	if !lines[0].OK {
		t.Errorf("doctor should report OK when the shim wins, got %+v", lines[0])
	}
	// No spool has been written in this test's state dir, so the observation
	// line must WARN rather than claim a reading that does not exist.
	if !lines[1].Warn || !strings.Contains(lines[1].Text, "not observed yet") {
		t.Errorf("observation line should warn that nothing has been observed, got %+v", lines[1])
	}
}

// TestStatuslineDoctorReportsObservedWindow: the other half — a spool that
// exists must be REPORTED, so "configured" and "producing readings" can be told
// apart from the same command.
func TestStatuslineDoctorReportsObservedWindow(t *testing.T) {
	claudeDir, _ := statuslineTestEnv(t)
	writeSettings(t, filepath.Join(claudeDir, "settings.json"),
		map[string]interface{}{"statusLine": statusLineToMap(statusLineConfig{Type: "command", Command: shimCommand()})})
	_ = saveStatuslinePrior(statuslinePriorRecord{Wrapped: true})
	if err := writeClaudeContextSpool("11111111-0000-0000-0000-000000000001",
		claudeContextSpool{ContextWindowTokens: 1_000_000, ObservedAt: 10}); err != nil {
		t.Fatal(err)
	}

	lines := StatuslineDoctor("")
	if len(lines) != 2 {
		t.Fatalf("expected two lines, got %+v", lines)
	}
	if !lines[1].OK || !strings.Contains(lines[1].Text, "1.0M") {
		t.Errorf("observation line should report the observed window, got %+v", lines[1])
	}
}

// TestShimContextSpoolPrivacy: the context spool is TWO INTEGERS. The blob it is
// lifted from also carries a transcript path, a cwd, a workspace and a running
// cost total, and none of them may reach disk here — the point of a narrow
// projection is lost if the file it writes is wide.
func TestShimContextSpoolPrivacy(t *testing.T) {
	_, stateDir := statuslineTestEnv(t)
	const secretPath = "/home/me/.claude/projects/secret-repo/transcript.jsonl"
	const secretCwd = "/home/me/secret-repo"
	blob := []byte(`{"session_id":"22222222-0000-0000-0000-000000000002",` +
		`"transcript_path":"` + secretPath + `","cwd":"` + secretCwd + `",` +
		`"model":{"id":"claude-secret-model"},` +
		`"cost":{"total_cost_usd":41.55},` +
		`"context_window":{"context_window_size":200000,"total_input_tokens":184577}}`)
	id, reading, ok := parseClaudeContextWindow(blob, 1700000000)
	if !ok {
		t.Fatal("expected a reading")
	}
	if err := writeClaudeContextSpool(id, reading); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "claude-context", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secretPath, secretCwd, "claude-secret-model", "41.55", "transcript", "184577"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Fatalf("content leaked into the context spool (%q): %s", leak, data)
		}
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

// A statusLine key the engineer DELETED must stay deleted. Deleting it removes
// our shim as surely as replacing it does, so the engineer wants no statusline —
// writing back the command we saved when we wrapped them would resurrect
// configuration they deliberately removed.
//
// Raised by review on PR #124, on the uninstall path: `statusline disable` is a
// deliberate act by someone managing their statusline, but `uninstall` calls
// this on every run, which is what turned a corner into something reachable.
func TestDisableDoesNotResurrectADeletedStatusline(t *testing.T) {
	claudeDir, _ := statuslineTestEnv(t)
	path := filepath.Join(claudeDir, "settings.json")
	original := statusLineConfig{Type: "command", Command: "my-cool-statusline.sh --flag"}
	writeSettings(t, path, map[string]interface{}{"statusLine": statusLineToMap(original), "otherKey": "keep-me"})

	if _, err := EnableStatusline(); err != nil {
		t.Fatal(err)
	}
	// The engineer deletes the statusLine key outright — shim and all.
	m, err := readSettingsMap(path)
	if err != nil {
		t.Fatal(err)
	}
	delete(m, "statusLine")
	if err := writeSettingsMap(path, m); err != nil {
		t.Fatal(err)
	}

	if err := DisableStatusline(); err != nil {
		t.Fatal(err)
	}

	if cur, ok := readStatusLine(path); ok {
		t.Fatalf("disable resurrected a statusLine the engineer deleted: %+v", cur)
	}
	if m, _ := readSettingsMap(path); m["otherKey"] != "keep-me" {
		t.Error("disable clobbered an unrelated settings key")
	}
	// The stored prior describes a slot nobody holds any more; a later enable
	// must start fresh rather than re-wrap a command that is no longer there.
	if rec, ok := loadStatuslinePrior(); ok && rec.Prior != nil {
		t.Errorf("the stale prior survived: %+v", rec.Prior)
	}
}
