package capture

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

func jwtWithClaims(t *testing.T, claims string) string {
	t.Helper()
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
}

func TestCursorAccountRefDerivation(t *testing.T) {
	sum := sha256.Sum256([]byte("auth0|user_01ABC"))
	want := hex.EncodeToString(sum[:])[:16]
	if got := cursorAccountRef(jwtWithClaims(t, `{"sub":"auth0|user_01ABC","exp":4102444800}`)); got != want {
		t.Fatalf("accountRef = %q, want %q", got, want)
	}
	for name, token := range map[string]string{
		"no sub":       jwtWithClaims(t, `{"exp":4102444800}`),
		"empty sub":    jwtWithClaims(t, `{"sub":"  "}`),
		"not a jwt":    "opaque-token",
		"bad payload":  "a.!!!.c",
		"non-json":     "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c",
		"numeric sub":  jwtWithClaims(t, `{"sub":123}`),
		"empty string": "",
	} {
		if got := cursorAccountRef(token); got != "unknown" {
			t.Errorf("%s: accountRef = %q, want unknown", name, got)
		}
	}
}

func stopPayload(t *testing.T, email string) []byte {
	t.Helper()
	p := map[string]interface{}{
		"hook_event_name": "stop", "conversation_id": "conv-1", "generation_id": "gen-1",
		"status": "completed", "input_tokens": 100, "output_tokens": 20,
	}
	if email != "" {
		p["user_email"] = email
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seeLogin records one vendor-cycle store read of `email` under `ref` at `at`.
func seeLogin(key []byte, email, ref string, at time.Time) {
	recordCursorAccountReading(cursorCredential{accountRef: ref, emailHMAC: cursorEmailHMAC(key, email)}, at)
}

func TestCursorHookAccountRefMatchUnreadableAbsent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	key := state.CursorAttributionKey()
	seeLogin(key, "Login.One@Example.com", "0123456789abcdef", time.Now().Add(-time.Minute))

	// Match: case and surrounding whitespace do not make a different login.
	if ref, ok := cursorHookAccountRef(stopPayload(t, "  login.one@example.COM ")); !ok || ref != "0123456789abcdef" {
		t.Fatalf("match: ref=%q ok=%v", ref, ok)
	}

	// Non-match: unreadable plus 8 hex of THIS install's HMAC of the turn's email.
	ref, ok := cursorHookAccountRef(stopPayload(t, "login.two@example.com"))
	if !ok || !regexp.MustCompile(`^unreadable:[0-9a-f]{8}$`).MatchString(ref) {
		t.Fatalf("non-match: ref=%q ok=%v", ref, ok)
	}
	if want := "unreadable:" + cursorEmailHMAC(key, "login.two@example.com")[:8]; ref != want {
		t.Fatalf("non-match: ref=%q, want %q (stable per install)", ref, want)
	}

	// No email: the field is absent, not "unreadable".
	if ref, ok := cursorHookAccountRef(stopPayload(t, "")); ok || ref != "" {
		t.Fatalf("no email: ref=%q ok=%v, want omitted", ref, ok)
	}
}

// A login's {email -> sub} never changes, so a login read in an OLD cycle — the
// laptop slept, the daemon died, the IDE has since switched logins — must still
// attribute. And a second login read in a later cycle must not displace it.
func TestCursorHookAccountRefMatchesEveryLoginEverSeen(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	key := state.CursorAttributionKey()
	now := time.Now()
	seeLogin(key, "one@example.com", "1111111111111111", now.Add(-72*time.Hour))
	seeLogin(key, "two@example.com", "2222222222222222", now)

	for email, want := range map[string]string{"one@example.com": "1111111111111111", "two@example.com": "2222222222222222"} {
		if ref, _ := cursorHookAccountRef(stopPayload(t, email)); ref != want {
			t.Errorf("%s: ref=%q, want %q", email, ref, want)
		}
	}
}

// The map is capped, evicting by LAST SEEN — a login re-read recently survives
// even if it was first seen before everything else.
func TestCursorAccountLoginsCapEvictsLeastRecentlySeen(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	key := state.CursorAttributionKey()
	base := time.Now().Add(-100 * time.Hour)
	email := func(i int) string { return fmt.Sprintf("login%02d@example.com", i) }
	ref := func(i int) string { return fmt.Sprintf("%016d", i) }

	for i := 0; i < cursorAccountLoginsMax; i++ {
		seeLogin(key, email(i), ref(i), base.Add(time.Duration(i)*time.Hour))
	}
	// Re-read login 0 (first seen earliest) most recently, then add one more.
	seeLogin(key, email(0), ref(0), base.Add(50*time.Hour))
	seeLogin(key, email(cursorAccountLoginsMax), ref(cursorAccountLoginsMax), base.Add(51*time.Hour))

	if n := len(loadCursorAccountLogins().Logins); n != cursorAccountLoginsMax {
		t.Fatalf("logins = %d, want cap %d", n, cursorAccountLoginsMax)
	}
	if got, _ := cursorHookAccountRef(stopPayload(t, email(1))); !strings.HasPrefix(got, "unreadable:") {
		t.Fatalf("least recently seen login survived the cap: %q", got)
	}
	for _, i := range []int{0, 2, cursorAccountLoginsMax} {
		if got, _ := cursorHookAccountRef(stopPayload(t, email(i))); got != ref(i) {
			t.Errorf("login %d: ref=%q, want %q", i, got, ref(i))
		}
	}
}

func TestCursorAttributionKeyIsStable(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	first := state.CursorAttributionKey()
	if len(first) != 32 {
		t.Fatalf("key length = %d, want 32", len(first))
	}
	if second := state.CursorAttributionKey(); !bytes.Equal(first, second) {
		t.Fatal("install key changed between calls — every turn would look like a new unreadable login")
	}
}

// The end-to-end privacy line: the store's cachedEmail and the hook's
// user_email both pass through this path, and neither — nor the install key —
// may appear in any emitted byte, or in the file the daemon hands the hook.
func TestCursorAccountAttributionNeverEmitsEmailOrKey(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	const cachedEmail = "Private.Login@Example.com"
	const hookEmail = " private.login@example.com"
	token := jwtWithClaims(t, `{"sub":"user_private","exp":4102444800}`)
	t.Setenv(cursorStateDBEnv, makeCursorStateDB(t, map[string]string{
		cursorAuthAccessTokenKey: token, cursorAuthRefreshTokenKey: token, cursorAuthCachedEmailKey: cachedEmail,
	}))

	cred, err := readCursorCredential()
	if err != nil {
		t.Fatal(err)
	}
	recordCursorAccountReading(cred, time.Now())

	res, ok := normalize.NormalizeCursorHook(redact.RedactBytes(stopPayload(t, hookEmail)), normalize.CursorHookOptions{})
	if !ok {
		t.Fatal("stop produced no events")
	}
	// The ref's VALUE is asserted after projection, below the leak check, so a
	// ref that carried the email fails as a leak rather than as a mismatch.
	ref, ok := cursorHookAccountRef(stopPayload(t, hookEmail))
	if !ok {
		t.Fatal("a stop with user_email must yield a cursorAccountRef")
	}
	stampCursorAccountRef(res.Events, ref)

	snap := buildCursorVendorSnapshot([]cursorVendorRow{{Timestamp: "1787873137139", ConversationID: "conv-1"}},
		time.Unix(0, 0), time.Unix(1, 0), nil, cursorVendorShapeRecord{})
	snap.AccountRef = cred.accountRef
	evs := append(res.Events, snap.rowEvents("dev")...)
	evs = append(evs, snap.completionEvent("dev", time.Now(), ""),
		buildCursorVendorAbsenceEvent("dev", cred.accountRef, CursorVendorAbsenceVendorUnreachable, time.Now(), time.Time{}, time.Time{}, cursorVendorShapeRecord{}))

	keyHex := hex.EncodeToString(state.CursorAttributionKey())
	banned := []string{"private.login", "Private.Login", "example.com", "user_private", keyHex}
	for _, e := range evs {
		// Projection ONLY, deliberately before ScrubEvent: the scrubber rewrites
		// email-shaped text, and a test that passed only because of that second
		// net would not notice the first one tearing.
		redact.ProjectEvent(&e, false)
		blob, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range banned {
			if strings.Contains(string(blob), b) {
				t.Fatalf("%s leaked %q:\n%s", e.Kind, b, blob)
			}
		}
		wantKey, wantVal := "accountRef", cred.accountRef
		if e.Kind == "ai_response" {
			wantKey = "cursorAccountRef"
		}
		if got := e.Data.(map[string]interface{})[wantKey]; got != wantVal {
			t.Fatalf("%s: %s = %v after projection, want %q", e.Kind, wantKey, got, wantVal)
		}
	}

	onDisk, err := os.ReadFile(cursorAccountReadingPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range banned {
		if strings.Contains(string(onDisk), b) {
			t.Fatalf("attribution cache leaked %q: %s", b, onDisk)
		}
	}
}
