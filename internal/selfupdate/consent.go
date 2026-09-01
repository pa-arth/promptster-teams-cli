package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Consent is the engineer's DURABLE answer to "may this binary keep itself up to
// date", and it exists because the per-cycle prompt it replaces was unreachable
// by construction.
//
// The prompt lived in the update check, and the update check runs in the watch
// daemon — which `start` detaches and `autostart` runs under launchd/systemd,
// neither of which has a terminal. confirmUpdate therefore saw a non-TTY stdin,
// declined itself, and returned an outcome that startupBanner had no case for,
// so the daemon found every release, refused it silently, and repeated that
// every 30 minutes forever. The visible symptom was a fleet frozen on the
// version it was installed at with nothing anywhere saying why.
//
// The deeper reason a prompt cannot work here is the product shape: this CLI is
// installed once and then never typed again, so ANY design that waits for the
// engineer to run a command waits forever. Consent has to be collected at the
// one moment they are provably at a keyboard — `login` or `start` — and then
// remembered.
//
// It is deliberately NOT a security boundary. Signature verification is
// (verify.go: minisign over SHA256SUMS, then sha256 per asset), and the org's
// policy switch and pin are the fleet-level control. This file only records
// whether a human on an UNMANAGED machine agreed to the arrangement once. An
// org-managed machine never consults it: which build lands on a company's
// laptops is the org's decision, not the individual engineer's, and asking them
// would imply otherwise.
type Consent int

const (
	// ConsentUnknown means nobody has been asked yet. The updater treats this as
	// "do not install", but unlike ConsentDenied it is worth asking about.
	ConsentUnknown Consent = iota
	// ConsentGranted means an engineer said yes at login/start. Updates apply
	// silently from then on; they find out afterwards via the statusline suffix
	// and `doctor`, not beforehand via a prompt they would never see.
	ConsentGranted
	// ConsentDenied means an engineer said no. Nothing is fetched or installed;
	// `promptster-teams update` still works, because an explicit command is a
	// fresh, unambiguous instruction that outranks a stored preference.
	ConsentDenied
	// ConsentAsk means an engineer wants to decide per release. The daemon shows
	// a GUI dialog naming both versions and linking the release notes — the one
	// channel that CAN reach someone who never types a command, since the watcher
	// runs in their graphical session even though it has no terminal (notify.go).
	//
	// It is asked at most ONCE PER VERSION, not once per check. The check runs
	// every 30 minutes; a dialog on that cadence would be indistinguishable from
	// malware and would train the engineer to dismiss it on sight, which is the
	// slowest possible way to arrive back at a fleet that never updates.
	ConsentAsk
)

// consentRecord is the on-disk shape. Version and DecidedAt are not read by any
// logic — they exist so a support transcript can answer "when did this machine
// agree, and to what" without guessing.
type consentRecord struct {
	Answer    string    `json:"answer"`
	Version   string    `json:"version"`
	DecidedAt time.Time `json:"decidedAt"`
}

// consentPath stores the answer next to the update cursor rather than in the
// policy cache, because the two have opposite lifecycles: the policy cache is
// TTL'd and refetchable, while this is a one-time human decision that must
// outlive every cache eviction. Putting it in a refetchable file is how a
// "remembered" preference quietly becomes a per-cycle prompt again.
func consentPath() string {
	return filepath.Join(state.GlobalPromptsterDir(), "auto-update-consent.json")
}

// LoadConsent reads the stored answer. Every failure — missing file, unreadable
// file, corrupt JSON, unrecognized answer — resolves to ConsentUnknown rather
// than to granted or denied. Unknown is the only safe reading of a damaged
// record: it neither installs code the engineer never approved nor permanently
// suppresses updates because a byte flipped.
func LoadConsent() Consent {
	data, err := os.ReadFile(consentPath())
	if err != nil {
		return ConsentUnknown
	}
	var rec consentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return ConsentUnknown
	}
	switch strings.ToLower(strings.TrimSpace(rec.Answer)) {
	case "granted":
		return ConsentGranted
	case "denied":
		return ConsentDenied
	case "ask":
		return ConsentAsk
	default:
		return ConsentUnknown
	}
}

// SaveConsent records the answer, best-effort. A write failure is deliberately
// silent and non-fatal: the caller is mid-`login`, and failing that command
// because a preference file could not be written would break setup over a
// nicety. The cost of the failure is being asked again next time, which is the
// benign direction.
func SaveConsent(c Consent, version string) {
	answer := "unknown"
	switch c {
	case ConsentGranted:
		answer = "granted"
	case ConsentDenied:
		answer = "denied"
	case ConsentAsk:
		answer = "ask"
	}
	data, err := json.Marshal(consentRecord{
		Answer:    answer,
		Version:   version,
		DecidedAt: time.Now().UTC(),
	})
	if err != nil {
		return
	}
	p := consentPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, data, 0o600)
}

// HasDecided reports whether an engineer has already answered, so `login` and
// `start` can ask exactly once and stay quiet on every later run.
func HasDecided() bool { return LoadConsent() != ConsentUnknown }
