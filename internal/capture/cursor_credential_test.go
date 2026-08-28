package capture

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
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
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"marker":%q}`, expires.Unix(), marker)))
	return "eyJhbGciOiJub25lIn0." + payload + ".signature"
}

func TestReadCursorAuthKeysOpensDisposableCloneAndOnlyAuthKeys(t *testing.T) {
	live := makeCursorStateDB(t, map[string]string{
		cursorAuthAccessTokenKey: "access", cursorAuthRefreshTokenKey: "refresh",
		"cursorAuth/cachedEmail": "private@example.com", "composer.planRegistry": "/private/project",
	})
	values, opened, err := readCursorAuthKeys(live)
	if err != nil {
		t.Fatal(err)
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

func TestCredentialSecretMutationCannotReachErrorsOrPayload(t *testing.T) {
	secret := "mutation-canary-cursor-bearer"
	for _, text := range []string{credentialErr(CursorVendorAbsenceCredentialAbsent, "no token").Error(), buildCursorVendorAbsenceEvent("device", CursorVendorAbsenceCredentialAbsent, time.Now(), time.Time{}, time.Time{}, cursorVendorShapeRecord{}).Data.(map[string]interface{})["absenceReason"].(string)} {
		if err := assertNoCredentialInText(text, secret); err != nil {
			t.Fatal(err)
		}
	}
	// Mutation guard: replacing the harmless reason with the bearer must turn red.
	if assertNoCredentialInText("failure: "+secret, secret) == nil {
		t.Fatal("secret leak predicate did not detect injected bearer")
	}
}
