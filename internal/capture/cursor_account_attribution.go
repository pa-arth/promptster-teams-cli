package capture

import (
	"crypto/hmac"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// openspec cursor-vendor-multi-account, Phase A — WHICH CURSOR LOGIN RAN A TURN.
//
// The vendor collector reads one login (the IDE global store). If the engineer
// works under two, the second login's spend is invisible and reads as a vendor
// that stopped metering. Phase A does not read the second login; it makes it
// COUNTABLE: every hook `stop` turn carries `cursorAccountRef`, either the
// accountRef of the store whose cachedEmail matches the turn's user_email, or
// `unreadable:<hmac8>` — a stable per-install token for a login we cannot read.
//
// NO EMAIL LEAVES THE DEVICE, AND NONE IS STORED. Both sides of the comparison
// are reduced to HMAC-SHA256(installKey, lower(trim(email))) at the moment they
// are read (cursorEmailHMAC); the key is state.CursorAttributionKey and is never
// emitted.
//
// WHY A FILE BETWEEN THE DAEMON AND THE HOOK. The hook runs inside the
// engineer's agent loop with a 2s budget, and blowing the budget abandons the
// turn's usage row entirely. Cloning and opening a ~1GB SQLite store there is a
// risk to spend data for an attribution nicety, so the daemon's vendor cycle —
// which already reads the store — writes the digest here, and the hook reads a
// few hundred bytes.

// cursorAccountReadingMaxAge bounds how long a store reading may attribute
// turns. Logins rotate, so a reading is good for ONE collection cycle; the
// extra minute covers the poll's own latency (the policy lookup precedes the
// store read), so a turn landing just as the next cycle starts is not
// misfiled as unreadable.
const cursorAccountReadingMaxAge = cursorVendorPollInterval + time.Minute

const cursorAccountRefUnreadablePrefix = "unreadable:"

// cursorAccountReading is the ONLY thing persisted: a digest, a ref, a time.
type cursorAccountReading struct {
	EmailHMAC  string    `json:"emailHmac"`
	AccountRef string    `json:"accountRef"`
	ObservedAt time.Time `json:"observedAt"`
}

func cursorAccountReadingPath() string {
	return filepath.Join(state.StateDir(), "cursor-account.json")
}

// recordCursorAccountReading persists what this cycle's store read established.
// Best-effort: a failed write degrades turns to unreadable, never to wrong.
func recordCursorAccountReading(cred cursorCredential, observedAt time.Time) {
	b, err := json.Marshal(cursorAccountReading{
		EmailHMAC: cred.emailHMAC, AccountRef: cred.accountRef, ObservedAt: observedAt.UTC(),
	})
	if err != nil {
		return
	}
	path := cursorAccountReadingPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func loadCursorAccountReading() (cursorAccountReading, bool) {
	var r cursorAccountReading
	b, err := os.ReadFile(cursorAccountReadingPath()) // #nosec G304 -- state dir path.
	if err != nil || json.Unmarshal(b, &r) != nil {
		return cursorAccountReading{}, false
	}
	return r, true
}

// cursorHookAccountRef derives `cursorAccountRef` for one `stop` payload.
//
// It takes the RAW payload, not the redacted one, and that is forced rather
// than chosen: redact.RedactBytes rewrites every email address to
// [REDACTED_EMAIL] before normalization, so the redacted bytes cannot be
// matched. It decodes exactly one key, holds the address only long enough to
// HMAC it, and returns a value that cannot contain it.
//
// ok=false (field omitted) when the payload has no user_email, or when the
// install key cannot be persisted — see state.CursorAttributionKey.
func cursorHookAccountRef(raw []byte, now time.Time) (string, bool) {
	var p struct {
		UserEmail string `json:"user_email"`
	}
	if json.Unmarshal(raw, &p) != nil || strings.TrimSpace(p.UserEmail) == "" {
		return "", false
	}
	mac := cursorEmailHMAC(state.CursorAttributionKey(), p.UserEmail)
	if mac == "" {
		return "", false
	}
	if r, ok := loadCursorAccountReading(); ok && r.EmailHMAC != "" && r.AccountRef != "" &&
		now.Sub(r.ObservedAt) <= cursorAccountReadingMaxAge &&
		hmac.Equal([]byte(r.EmailHMAC), []byte(mac)) {
		return r.AccountRef, true
	}
	return cursorAccountRefUnreadablePrefix + mac[:8], true
}

// stampCursorAccountRef writes the ref onto the turn's usage rows.
func stampCursorAccountRef(evs []event.Event, ref string) {
	for i := range evs {
		if evs[i].Kind != "ai_response" {
			continue
		}
		if data, ok := evs[i].Data.(map[string]interface{}); ok {
			data["cursorAccountRef"] = ref
		}
	}
}
