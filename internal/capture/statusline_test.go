package capture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
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

// --- the wrap must never become a replacement --------------------------------
//
// These four pin the one property that makes wrapping an engineer's statusline
// defensible at all: whatever happens to the command we wrapped, the line the
// engineer sees is THEIRS or nothing — never ours. The shipped shim failed this
// on a plain timeout, and the timeout was reachable by a normal statusline
// (claude-hud, measured at 0.6s-4.5s against a 2.5s bound), so the failure was
// the common case rather than a corner.

// shimWrapPrior stores a wrapped prior command for the tests below.
func shimWrapPrior(t *testing.T, command string) {
	t.Helper()
	if err := saveStatuslinePrior(statuslinePriorRecord{
		Wrapped: true,
		Prior:   &statusLineConfig{Type: "command", Command: command},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWrappedFailureNeverDrawsOurLine: the wrapped command exits non-zero with
// no output and no cache to fall back on. A blank line is the correct answer;
// promptster branding is not.
func TestWrappedFailureNeverDrawsOurLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -c")
	}
	statuslineTestEnv(t)
	shimWrapPrior(t, "exit 1")

	blob := []byte(`{"session_id":"s","rate_limits":{"five_hour":{"used_percentage":50,"resets_at":1900000000}}}`)
	out := runPriorStatusline(blob)
	if strings.Contains(string(out), "promptster") {
		t.Fatalf("our line replaced the wrapped statusline: %q", out)
	}
	if len(out) != 0 {
		t.Errorf("want an empty render, got %q", out)
	}
}

// TestWrappedTimeoutServesLastGoodLine: a tick slower than the bound redraws the
// engineer's previous line, not ours. This is the exact shape of the claude-hud
// conflict — a working statusline that is merely slow.
func TestWrappedTimeoutServesLastGoodLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -c")
	}
	statuslineTestEnv(t)
	blob := []byte(`{"session_id":"s","rate_limits":{"five_hour":{"used_percentage":50,"resets_at":1900000000}}}`)

	// ONE command that is fast on its first tick and slow on every tick after —
	// the same string throughout, because changing it would (correctly) drop the
	// cache and stop testing what this test is about.
	mark := filepath.Join(t.TempDir(), "slow")
	shimWrapPrior(t, "[ -f "+mark+" ] && sleep 30; printf 'THEIR-LINE'")

	// The fast tick seeds the cache.
	if got := string(runPriorStatusline(blob)); got != "THEIR-LINE" {
		t.Fatalf("seed tick = %q, want THEIR-LINE", got)
	}

	// Arm the slowness and shrink the bound so the tick certainly overruns it.
	if err := os.WriteFile(mark, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	orig := priorCommandTimeout
	priorCommandTimeout = 100 * time.Millisecond
	t.Cleanup(func() { priorCommandTimeout = orig })

	start := time.Now()
	out := string(runPriorStatusline(blob))
	elapsed := time.Since(start)

	if strings.Contains(out, "promptster") {
		t.Fatalf("a slow tick drew our line over theirs: %q", out)
	}
	if out != "THEIR-LINE" {
		t.Errorf("timeout render = %q, want the cached THEIR-LINE", out)
	}
	// And the bound has to actually bind. `sleep 30` is a GRANDCHILD holding the
	// stdout pipe open; without cmd.WaitDelay this same call returned in 30s
	// despite a 100ms context — a timeout that timed out and then waited anyway.
	if elapsed > 5*time.Second {
		t.Errorf("timeout did not bound the render: took %s", elapsed)
	}
}

// TestRewrapDropsTheCachedLine: when the engineer swaps their statusline, the
// old one's remembered output must not be served as a fallback for the new one.
func TestRewrapDropsTheCachedLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -c")
	}
	statuslineTestEnv(t)
	blob := []byte(`{"session_id":"s"}`)

	shimWrapPrior(t, "printf 'OLD-LINE'")
	_ = runPriorStatusline(blob)

	shimWrapPrior(t, "exit 1")
	if out := string(runPriorStatusline(blob)); out != "" {
		t.Errorf("re-wrap served the old statusline's output: %q", out)
	}
}

// TestOwnLineStillRendersForAnEmptySlot: the one case our line is ours to draw
// — nothing was wrapped, so nothing is displaced.
func TestOwnLineStillRendersForAnEmptySlot(t *testing.T) {
	statuslineTestEnv(t)
	blob := []byte(`{"session_id":"s","rate_limits":{"five_hour":{"used_percentage":50,"resets_at":1900000000},"seven_day":{"used_percentage":7,"resets_at":1950000000}}}`)
	out := string(runPriorStatusline(blob))
	if !strings.Contains(out, "promptster") {
		t.Errorf("empty slot should render our line, got %q", out)
	}
}

// TestConcurrentWritersNeverPublishASplicedFile: the last-good cache is written
// by the shim, and the shim runs once per status-line tick in EVERY open Claude
// Code session at once. A shared `<path>.tmp` lets two of them interleave into
// one file that is then renamed into place, so a session's fallback line becomes
// a splice of another's. Every read must be one writer's WHOLE value.
func TestConcurrentWritersNeverPublishASplicedFile(t *testing.T) {
	statuslineTestEnv(t)

	// Distinct lengths and distinct fill bytes, so a splice cannot masquerade as
	// a clean write the way same-shaped payloads would.
	payloads := [][]byte{
		bytes.Repeat([]byte("A"), 1024),
		bytes.Repeat([]byte("B"), 4096),
		bytes.Repeat([]byte("C"), 16384),
	}
	valid := map[string]bool{}
	for _, p := range payloads {
		valid[string(p)] = true
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers run throughout, so a torn file has to survive only until the next
	// rename to be caught.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := loadStatuslineLastGood(); len(got) > 0 && !valid[string(got)] {
					t.Errorf("read a spliced cache: %d bytes, starts %q", len(got), got[:1])
					return
				}
			}
		}()
	}

	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func(p []byte) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				saveStatuslineLastGood(p)
			}
		}(payloads[w%len(payloads)])
	}

	// Writers finish, then readers are told to stop.
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()

	// And no temp files may be left lying around in the state dir.
	entries, err := os.ReadDir(state.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
}
// --- re-healing a displaced shim ---------------------------------------------
//
// The failure these pin: another tool's setup writes `statusLine.command`
// directly, our shim is gone, and window capture stops on that machine with no
// visible symptom — the engineer's statusline looks fine, because it is the
// other tool's, rendering normally.

// hudCommand stands in for the real thing: any third-party statusline that
// writes the slot for itself.
const hudCommand = "node /plugins/claude-hud/dist/index.js"

// writeUserStatusLine puts a raw statusLine into the user layer, the way another
// tool's setup does — settings.json edited directly, our shim not consulted.
func writeUserStatusLine(t *testing.T, command string) {
	t.Helper()
	m, err := readSettingsMap(userSettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	m["statusLine"] = map[string]interface{}{"type": "command", "command": command}
	if err := writeSettingsMap(userSettingsPath(), m); err != nil {
		t.Fatal(err)
	}
}

// currentUserCommand reads back whatever holds the slot now.
func currentUserCommand(t *testing.T) string {
	t.Helper()
	cfg, ok := readStatusLine(userSettingsPath())
	if !ok {
		return ""
	}
	return cfg.Command
}

// TestRehealRewrapsAfterAnotherToolEvictsUs is the claude-hud sequence end to
// end: we wrap hud, hud's setup overwrites the slot with itself, and the heal
// puts the shim back around hud rather than on top of it.
func TestRehealRewrapsAfterAnotherToolEvictsUs(t *testing.T) {
	statuslineTestEnv(t)
	writeUserStatusLine(t, hudCommand)
	if _, err := EnableStatusline(); err != nil {
		t.Fatal(err)
	}
	if !StatuslineWrapped() {
		t.Fatal("setup: expected our shim to be installed")
	}

	// claude-hud's setup runs again and takes the slot back.
	writeUserStatusLine(t, hudCommand)
	if StatuslineWrapped() {
		t.Fatal("setup: expected to be displaced")
	}

	res, err := RehealStatusline()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rewrapped {
		t.Fatalf("expected a re-wrap, got %+v", res)
	}
	if !StatuslineWrapped() {
		t.Error("the shim was not put back")
	}
	// And hud must still be what renders — a heal that dropped the prior would
	// silently replace the engineer's statusline with ours.
	rec, ok := loadStatuslinePrior()
	if !ok || rec.Prior == nil || rec.Prior.Command != hudCommand {
		t.Errorf("the displacing statusline was not preserved as the prior: %+v", rec.Prior)
	}
}

// TestRehealLeavesADisabledStatuslineAlone: `statusline disable` clears the
// prior record, and an off switch that something reverses on a timer is not an
// off switch.
func TestRehealLeavesADisabledStatuslineAlone(t *testing.T) {
	statuslineTestEnv(t)
	writeUserStatusLine(t, hudCommand)
	if _, err := EnableStatusline(); err != nil {
		t.Fatal(err)
	}
	if err := DisableStatusline(); err != nil {
		t.Fatal(err)
	}

	res, err := RehealStatusline()
	if err != nil {
		t.Fatal(err)
	}
	if res.Rewrapped {
		t.Error("re-wrapped a statusline the engineer had disabled")
	}
	if got := currentUserCommand(t); got != hudCommand {
		t.Errorf("disable did not leave hud in place: %q", got)
	}
}

// TestRehealDoesNotResurrectADeletedStatusline: an absent key means the engineer
// wants no status line. Filling the hole with ours invents configuration they
// removed on purpose.
func TestRehealDoesNotResurrectADeletedStatusline(t *testing.T) {
	statuslineTestEnv(t)
	writeUserStatusLine(t, hudCommand)
	if _, err := EnableStatusline(); err != nil {
		t.Fatal(err)
	}

	m, err := readSettingsMap(userSettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	delete(m, "statusLine")
	if err := writeSettingsMap(userSettingsPath(), m); err != nil {
		t.Fatal(err)
	}

	res, err := RehealStatusline()
	if err != nil {
		t.Fatal(err)
	}
	if res.Rewrapped {
		t.Error("resurrected a statusline the engineer deleted")
	}
	if got := currentUserCommand(t); got != "" {
		t.Errorf("statusLine came back as %q", got)
	}
}

// TestRehealGivesUpAgainstATimer bounds a fight: something else rewrites the
// slot on every check, so we stop swinging and let doctor say a human must pick.
func TestRehealGivesUpAgainstATimer(t *testing.T) {
	statuslineTestEnv(t)
	writeUserStatusLine(t, hudCommand)
	if _, err := EnableStatusline(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < maxAutoHeals; i++ {
		writeUserStatusLine(t, hudCommand) // the adversary, every single check
		res, err := RehealStatusline()
		if err != nil {
			t.Fatal(err)
		}
		if !res.Rewrapped {
			t.Fatalf("heal %d: expected a re-wrap, got %+v", i+1, res)
		}
	}

	writeUserStatusLine(t, hudCommand)
	res, err := RehealStatusline()
	if err != nil {
		t.Fatal(err)
	}
	if !res.GaveUp {
		t.Errorf("expected to give up after %d heals, got %+v", maxAutoHeals, res)
	}
	if got := currentUserCommand(t); got != hudCommand {
		t.Errorf("kept fighting after giving up: %q", got)
	}
}

// TestRehealCounterResetsWhenWeAreLeftAlone: an occasional eviction — the
// engineer re-runs another tool's setup now and then — must never accumulate
// toward the give-up bound. A check that finds us in place clears the count.
func TestRehealCounterResetsWhenWeAreLeftAlone(t *testing.T) {
	statuslineTestEnv(t)
	writeUserStatusLine(t, hudCommand)
	if _, err := EnableStatusline(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < maxAutoHeals*3; i++ {
		writeUserStatusLine(t, hudCommand)
		res, err := RehealStatusline()
		if err != nil {
			t.Fatal(err)
		}
		if !res.Rewrapped {
			t.Fatalf("eviction %d was not repaired: %+v", i+1, res)
		}
		// The quiet checks in between, which are the common case.
		for j := 0; j < 3; j++ {
			if _, err := RehealStatusline(); err != nil {
				t.Fatal(err)
			}
		}
		if rec, ok := loadStatuslinePrior(); !ok || rec.Heals != 0 {
			t.Fatalf("eviction %d: heal count did not reset, got %d", i+1, rec.Heals)
		}
	}
}

// NOT TESTED HERE: the managed-policy gate. managedSettingsPath() is a hardcoded
// OS-owned path, and the only way to exercise it from a test would be an env
// override — which would hand any user a way to redirect managed policy at a
// file they control. The gate is three lines and reads plainly; that is a better
// trade than a test-only escape hatch through an admin boundary.
