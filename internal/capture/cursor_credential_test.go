package capture

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func makeCursorStateDB(t *testing.T, values map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for k, v := range values {
		if _, err = db.Exec(`INSERT INTO ItemTable(key,value) VALUES(?,?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func jwtWithExpiry(t *testing.T, expires time.Time, marker string) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":"user_test","exp":%d,"marker":%q}`, expires.Unix(), marker)))
	return "eyJhbGciOiJub25lIn0." + payload + ".signature"
}

func TestReadCursorAuthKeysOpensDisposableCloneAndOnlyAuthKeys(t *testing.T) {
	live := makeCursorStateDB(t, map[string]string{
		cursorAuthAccessTokenKey: "access", cursorAuthRefreshTokenKey: "refresh",
		"cursorAuth/cachedEmail": "private@example.com", "composer.planRegistry": "/private/project",
	})
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	values, emailHMAC, opened, err := readCursorAuthKeys(live)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(emailHMAC) {
		t.Fatalf("cachedEmail must come back only as a 64-hex HMAC, got %q", emailHMAC)
	}
	if opened == live || filepath.Dir(opened) == filepath.Dir(live) {
		t.Fatalf("opened live state store: %q", opened)
	}
	if _, err := os.Stat(opened); !os.IsNotExist(err) {
		t.Fatalf("temporary clone was not deleted: %v", err)
	}
	if len(values) != 2 || values[cursorAuthAccessTokenKey] != "access" || values[cursorAuthRefreshTokenKey] != "refresh" {
		t.Fatalf("unexpected selected keys: %#v", values)
	}
	for _, forbidden := range []string{"private@example.com", "/private/project"} {
		for _, v := range values {
			if strings.Contains(v, forbidden) {
				t.Fatalf("neighbouring value escaped: %q", forbidden)
			}
		}
	}
}

func TestReadCursorCredentialRereadsStoreEveryCycle(t *testing.T) {
	first := jwtWithExpiry(t, time.Now().Add(time.Hour), "first-secret")
	live := makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: first, cursorAuthRefreshTokenKey: first})
	t.Setenv(cursorStateDBEnv, live)
	c1, err := readCursorCredential()
	if err != nil {
		t.Fatal(err)
	}
	second := jwtWithExpiry(t, time.Now().Add(2*time.Hour), "second-secret")
	db, _ := sql.Open("sqlite", live)
	_, err = db.Exec(`UPDATE ItemTable SET value=? WHERE key IN (?,?)`, second, cursorAuthAccessTokenKey, cursorAuthRefreshTokenKey)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := readCursorCredential()
	if err != nil {
		t.Fatal(err)
	}
	if c1.token == c2.token || c2.token != second {
		t.Fatal("credential was cached across cycles")
	}
}

// PD-45: credential_absent alone could not say which step failed. Each failure
// in the IDE store read carries its own fixed code, and that code (never a
// path or a token) reaches the absence event as absenceDetail.
func TestCredentialAbsenceNamesTheFailedStep(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	live := jwtWithExpiry(t, time.Now().Add(time.Hour), "detail-canary-token")
	noSub := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800}`)) + ".sig"
	notSQLite := filepath.Join(t.TempDir(), "state.vscdb")
	if err := os.WriteFile(notSQLite, []byte("not a database, at all, not even close to one"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		want cursorCredentialDetail
	}{
		{"missing store", filepath.Join(t.TempDir(), "absent", "state.vscdb"), cursorDetailStoreMissing},
		{"not sqlite", notSQLite, cursorDetailStoreUnreadable},
		// The key-rename / token-moved case: a healthy store with no cursorAuth rows.
		{"no auth rows", makeCursorStateDB(t, map[string]string{"other/key": "v"}), cursorDetailStoreNoAuthRows},
		{"empty token", makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: " "}), cursorDetailTokenMissing},
		{"no sub", makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: noSub}), cursorDetailTokenNoSub},
		{"expired", makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: jwtWithExpiry(t, time.Now().Add(-time.Hour), "x")}), cursorDetailTokenExpired},
	}
	codeShape := regexp.MustCompile(`^[a-z][a-z_]{0,47}$`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readCursorCredentialAt(tc.path)
			if got := cursorCredentialAbsenceDetail(err); got != tc.want {
				t.Fatalf("detail = %q, want %q (err %v)", got, tc.want, err)
			}
			ev := buildCursorCredentialAbsenceEvent("dev", cursorAccountRefUnknown, err, time.Now())
			data := ev.Data.(map[string]interface{})
			got, _ := data["absenceDetail"].(string)
			if got != string(tc.want) || !codeShape.MatchString(got) {
				t.Fatalf("absenceDetail on the wire = %q, want %q", got, tc.want)
			}
			if data["absenceReason"] != string(cursorCredentialAbsence(err)) {
				t.Fatalf("absenceReason changed: %v", data["absenceReason"])
			}
			for _, v := range data {
				if s, ok := v.(string); ok && (strings.Contains(s, tc.path) || strings.Contains(s, "/")) {
					t.Fatalf("a path reached the absence: %q", s)
				}
			}
		})
	}
	// A readable store with a live token is not an absence at all.
	if _, err := readCursorCredentialAt(makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: live})); err != nil {
		t.Fatalf("live token read as absent: %v", err)
	}
	// An error this file did not build carries no detail, and the field is omitted.
	ev := buildCursorCredentialAbsenceEvent("dev", cursorAccountRefUnknown, fmt.Errorf("other"), time.Now())
	if _, ok := ev.Data.(map[string]interface{})["absenceDetail"]; ok {
		t.Fatal("absenceDetail emitted for an unclassified error")
	}
}

func TestCredentialSecretMutationCannotReachErrorsOrPayload(t *testing.T) {
	secret := "mutation-canary-cursor-bearer"
	for _, text := range []string{credentialErr(CursorVendorAbsenceCredentialAbsent, cursorDetailTokenMissing, "no token").Error(), buildCursorVendorAbsenceEvent("device", "unknown", CursorVendorAbsenceCredentialAbsent, time.Now(), time.Time{}, time.Time{}, cursorVendorShapeRecord{}).Data.(map[string]interface{})["absenceReason"].(string)} {
		if err := assertNoCredentialInText(text, secret); err != nil {
			t.Fatal(err)
		}
	}
	// Mutation guard: replacing the harmless reason with the bearer must turn red.
	if assertNoCredentialInText("failure: "+secret, secret) == nil {
		t.Fatal("secret leak predicate did not detect injected bearer")
	}
}
