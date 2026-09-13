package capture

import (
	"crypto/hmac"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
// accountRef of a login whose cachedEmail matches the turn's user_email, or
// `unreadable:<hmac8>` — a stable per-install token for a login this device has
// never read.
//
// NO EMAIL LEAVES THE DEVICE, AND NONE IS STORED. Both sides of the comparison
// are reduced to HMAC-SHA256(installKey, lower(trim(email))) at the moment they
// are read (cursorEmailHMAC); the key is state.CursorAttributionKey and is never
// emitted. What IS stored — an email HMAC and a sha256-of-sub ref — is not a
// credential, which is why this does not conflict with D1 (store no credential).
//
// WHY A FILE BETWEEN THE DAEMON AND THE HOOK. The hook runs inside the
// engineer's agent loop with a 2s budget, and blowing the budget abandons the
// turn's usage row entirely. Cloning and opening a ~1GB SQLite store there is a
// risk to spend data for an attribution nicety, so the daemon's vendor cycle —
// which already reads the store — records the login here, and the hook reads a
// few hundred bytes.
//
// WHY EVERY LOGIN EVER SEEN, NOT THE CURRENT ONE. A login's sub and email do not
// change, so {emailHmac -> accountRef} is a permanent fact, never a stale one.
// Gating it on freshness only manufactures false `unreadable:` refs across
// laptop sleep, a dead daemon, or the IDE being switched between logins — and
// inflates exactly the "accounts we cannot read" count Phase A exists to take.
// Consequence worth stating: `unreadable:` means "the map exists and this email
// matched no login in it", not "not readable right now". Whether a KNOWN login's
// usage is currently being collected is the snapshot's question, answered per
// accountRef on the backend.
//
// NO MAP, NO FIELD. The map is absent before the first vendor cycle and after
// the org turns the collector off (forgetCursorAccountLogins); in both cases
// nothing is being read, so the hook says nothing rather than `unreadable:`.

const (
	cursorAccountRefUnreadablePrefix = "unreadable:"
	cursorAccountLoginsVersion       = 1
	// ponytail: capped at 16 logins, evicting the least recently seen. An engineer
	// with more than 16 Cursor logins on one device would see the oldest re-read
	// login flip to unreadable until it is read again; raise the cap if that is
	// ever observed.
	cursorAccountLoginsMax = 16
)

// cursorAccountLogin is one login this device has read: a digest, a ref, times.
type cursorAccountLogin struct {
	EmailHMAC  string    `json:"emailHmac"`
	AccountRef string    `json:"accountRef"`
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`
}

type cursorAccountLogins struct {
	Version int                  `json:"version"`
	Logins  []cursorAccountLogin `json:"logins"`
}

func cursorAccountReadingPath() string {
	return filepath.Join(state.StateDir(), "cursor-account.json")
}

// forgetCursorAccountLogins deletes the map. Idempotent.
func forgetCursorAccountLogins() {
	_ = os.Remove(cursorAccountReadingPath())
}

// recordCursorAccountReading upserts the login this cycle's store read
// established. Best-effort: a failed write degrades turns to unreadable, never
// to wrong. Only the daemon writes this file, so there is no writer race; the
// rename keeps the hook from reading a torn file.
func recordCursorAccountReading(cred cursorCredential, seen time.Time) {
	if cred.emailHMAC == "" || cred.accountRef == "" {
		return
	}
	seen = seen.UTC()
	file := loadCursorAccountLogins()
	found := false
	for i := range file.Logins {
		if file.Logins[i].EmailHMAC == cred.emailHMAC {
			file.Logins[i].AccountRef = cred.accountRef
			file.Logins[i].LastSeen = seen
			found = true
		}
	}
	if !found {
		file.Logins = append(file.Logins, cursorAccountLogin{
			EmailHMAC: cred.emailHMAC, AccountRef: cred.accountRef, FirstSeen: seen, LastSeen: seen,
		})
	}
	sort.SliceStable(file.Logins, func(i, j int) bool { return file.Logins[i].LastSeen.After(file.Logins[j].LastSeen) })
	if len(file.Logins) > cursorAccountLoginsMax {
		file.Logins = file.Logins[:cursorAccountLoginsMax]
	}
	file.Version = cursorAccountLoginsVersion

	b, err := json.Marshal(file)
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

// loadCursorAccountLogins reads the map; anything missing, unparseable or of
// another version is an empty map (turns become unreadable, never misfiled).
func loadCursorAccountLogins() cursorAccountLogins {
	var f cursorAccountLogins
	b, err := os.ReadFile(cursorAccountReadingPath()) // #nosec G304 -- state dir path.
	if err != nil || json.Unmarshal(b, &f) != nil || f.Version != cursorAccountLoginsVersion {
		return cursorAccountLogins{}
	}
	return f
}

// cursorHookAccountRef derives `cursorAccountRef` for one `stop` payload.
//
// It takes the RAW payload, not the redacted one, and that is forced rather
// than chosen: redact.RedactBytes rewrites every email address to
// [REDACTED_EMAIL] before normalization, so the redacted bytes cannot be
// matched. It decodes exactly one key, holds the address only long enough to
// HMAC it, and returns a value that cannot contain it.
//
// ok=false (field omitted) when the payload has no user_email, when there is no
// usable login map (never collected, or the collector was switched off), or
// when the install key cannot be persisted — see state.CursorAttributionKey.
func cursorHookAccountRef(raw []byte) (string, bool) {
	var p struct {
		UserEmail string `json:"user_email"`
	}
	if json.Unmarshal(raw, &p) != nil || strings.TrimSpace(p.UserEmail) == "" {
		return "", false
	}
	logins := loadCursorAccountLogins()
	if logins.Version != cursorAccountLoginsVersion {
		return "", false
	}
	mac := cursorEmailHMAC(state.CursorAttributionKey(), p.UserEmail)
	if mac == "" {
		return "", false
	}
	for _, l := range logins.Logins {
		if l.AccountRef != "" && hmac.Equal([]byte(l.EmailHMAC), []byte(mac)) {
			return l.AccountRef, true
		}
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
