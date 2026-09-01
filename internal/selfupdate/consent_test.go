package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// consentHome points GlobalPromptsterDir at a temp dir so these tests never
// touch the developer's real stored answer.
func consentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	return home
}

func TestConsentRoundTrips(t *testing.T) {
	consentHome(t)

	if got := LoadConsent(); got != ConsentUnknown {
		t.Fatalf("fresh machine consent = %v, want unknown", got)
	}
	if HasDecided() {
		t.Fatal("a machine nobody has asked must not report a decision")
	}

	for _, want := range []Consent{ConsentGranted, ConsentDenied} {
		SaveConsent(want, "0.25.0")
		if got := LoadConsent(); got != want {
			t.Fatalf("after SaveConsent(%v), LoadConsent = %v", want, got)
		}
		if !HasDecided() {
			t.Fatalf("after SaveConsent(%v), HasDecided = false", want)
		}
	}
}

// A damaged record must read as UNKNOWN, never as granted. Reading corrupt bytes
// as permission would install code nobody agreed to; reading them as a decline
// would silently freeze the machine forever with no way to notice. Unknown is
// the only answer that is both safe and recoverable — the next login re-asks.
func TestDamagedConsentRecordReadsAsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not json", "yes please"},
		{"empty file", ""},
		{"unrecognized answer", `{"answer":"maybe"}`},
		{"missing answer field", `{"version":"0.25.0"}`},
		{"json null", "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := consentHome(t)
			dir := filepath.Join(home, ".promptster-teams")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "auto-update-consent.json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := LoadConsent(); got != ConsentUnknown {
				t.Fatalf("damaged record read as %v, want unknown", got)
			}
		})
	}
}

// The record must survive a process boundary — it is the entire difference
// between this design and the per-cycle prompt it replaced. A "preference" that
// only lives in memory is just a prompt with extra steps.
func TestConsentSurvivesReload(t *testing.T) {
	consentHome(t)
	SaveConsent(ConsentGranted, "0.25.0")

	// LoadConsent reads from disk every call, so a second read with no state in
	// between is exactly what the daemon does on its next cycle.
	for i := 0; i < 3; i++ {
		if got := LoadConsent(); got != ConsentGranted {
			t.Fatalf("read %d = %v, want granted", i, got)
		}
	}
}

// An unwritable state dir must not crash or block setup: the caller is mid-login
// and the worst acceptable outcome is being asked again next time.
func TestSaveConsentIsBestEffort(t *testing.T) {
	home := consentHome(t)
	// Occupy the state dir path with a FILE, so MkdirAll and the write both fail.
	if err := os.WriteFile(filepath.Join(home, ".promptster-teams"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	SaveConsent(ConsentGranted, "0.25.0") // must not panic
	if got := LoadConsent(); got != ConsentUnknown {
		t.Fatalf("consent = %v, want unknown after a failed write", got)
	}
}
