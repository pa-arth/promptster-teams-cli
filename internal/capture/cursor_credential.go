package capture

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; keeps the 6-platform cross-build CGO-free.
)

// cursor-vendor-usage-rail §1.1/§1.3 — reading the engineer's own Cursor
// credential, on their own device.
//
// THIS IS THE ONE RAIL IN THIS CLI THAT DOES NOT MERELY READ WHAT THE ENGINEER
// PRODUCED. Every other collector reads a transcript or a hook payload the
// engineer's tool wrote. This one AUTHENTICATES AS THE ENGINEER to a third
// party, and the consequences differ in kind rather than degree: a bug in the
// others loses data, a bug here can cost the engineer their Cursor account
// mid-workday. Everything below is shaped by that.
//
// THE CREDENTIAL NEVER LEAVES THIS PROCESS. It is returned as a value, handed
// to one Authorization header, and dropped. It appears in no emitted field, no
// log line, no error message, and no panic — cursorCredential deliberately has
// no String() and every error constructed in this file is built from a constant
// and a key NAME, never a value. `cursor_credential_leak_test.go` is the
// mutation-tested guard on that.

const (
	// cursorAuthAccessTokenKey / cursorAuthRefreshTokenKey are the ONLY two keys
	// this collector reads out of the store.
	//
	// THE SCOPE IS THE POINT, NOT A COURTESY. Measured on a live store
	// 2026-08-27, `cursorAuth/cachedEmail` sits three rows away from these two,
	// and `composer.planRegistry` elsewhere in the same file carries plan titles
	// and absolute paths for every plan on the machine. A `SELECT key, value FROM
	// ItemTable` with client-side filtering would pull all of that into this
	// process's memory before discarding it; the query below never asks for it.
	cursorAuthAccessTokenKey  = "cursorAuth/accessToken"  // #nosec G101 -- a STORE KEY name, not a credential.
	cursorAuthRefreshTokenKey = "cursorAuth/refreshToken" // #nosec G101 -- a STORE KEY name, not a credential.

	// cursorStateDBEnv is a TEST-ONLY override, ours rather than a documented
	// Cursor variable (same status as PROMPTSTER_CURSOR_HOME). It exists so the
	// unsupported-platform, absent-key and expiry branches are exercised on a
	// machine that ships the supported one.
	cursorStateDBEnv = "PROMPTSTER_CURSOR_STATE_DB"

	// cursorCredentialReadTimeout bounds the whole store read. The measured read
	// is 8ms against a 1GB store; anything approaching this bound means the
	// database is in a state we should decline rather than wait on, because this
	// runs inside a capture loop the engineer never asked to think about.
	cursorCredentialReadTimeout = 5 * time.Second
)

// cursorApplicationVersion reads installation metadata, never application
// state. Missing metadata is represented as an omitted version on the wire.
func cursorApplicationVersion() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	b, err := os.ReadFile("/Applications/Cursor.app/Contents/Resources/app/package.json")
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Version)
}

// cursorCredential is one reading of the store. It is deliberately NOT a
// long-lived object: see readCursorCredential's contract.
//
// NO String() OR MarshalJSON METHOD, EVER. A credential that can be printed by
// accident will eventually be printed by accident — %v on a struct is one
// debugging line away, and this type exists specifically so that line is
// harmless.
type cursorCredential struct {
	// token is the session bearer. Unexported, and nothing in this package
	// copies it anywhere but an Authorization header.
	token string
	// expiresAt is the `exp` claim, zero when unparseable. A PRE-FLIGHT ONLY —
	// see readCursorCredential.
	expiresAt time.Time
}

// cursorCredentialError names a failure without carrying a value.
type cursorCredentialError struct {
	reason CursorVendorAbsenceReason
	detail string
}

func (e *cursorCredentialError) Error() string {
	return "cursor credential: " + string(e.reason) + " (" + e.detail + ")"
}

// AbsenceReason lets the collector emit the right absence instead of silence.
func (e *cursorCredentialError) AbsenceReason() CursorVendorAbsenceReason { return e.reason }

func credentialErr(reason CursorVendorAbsenceReason, detail string) error {
	return &cursorCredentialError{reason: reason, detail: detail}
}

// cursorStateDBPath returns the platform path of Cursor's global state store.
//
// macOS ONLY IN V1 (design §10). The store's location and protection differ per
// platform and are unverified elsewhere — Windows may not use `state.vscdb` at
// all — and a collector that guesses a path and finds nothing emits an absence
// that reads exactly like an engineer who stopped using Cursor. So the
// unsupported platforms are named as unsupported rather than probed blindly.
func cursorStateDBPath() (string, error) {
	if override := os.Getenv(cursorStateDBEnv); override != "" {
		return override, nil
	}
	if !CursorVendorPlatformSupported(runtime.GOOS) {
		return "", credentialErr(CursorVendorAbsencePlatformUnsupported, runtime.GOOS)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", credentialErr(CursorVendorAbsenceCredentialAbsent, "no home directory")
	}
	return filepath.Join(home, "Library", "Application Support", "Cursor",
		"User", "globalStorage", "state.vscdb"), nil
}

// readCursorCredential reads the CURRENT credential out of the store.
//
// CALL THIS EVERY CYCLE. NEVER CACHE THE RESULT ACROSS CYCLES. That rule is the
// opposite of the one this rail was originally drafted with, and the reason is
// measured (2026-08-27):
//
//	cursorAuth/accessToken   sha256 0c200b378fc6…
//	cursorAuth/refreshToken  sha256 0c200b378fc6…   IDENTICAL, 424 bytes each
//	claims: type="session", iss=authentication.cursor.sh,
//	        issued 2026-08-26 02:42Z, expires 2026-10-25 02:42Z
//
// THE TWO KEYS HOLD THE SAME BYTES. There is no refresh token, no grant to
// exchange, and no refresh lifecycle for this collector to own — so do not build
// one. Both keys are read anyway, because "they are identical" is an observation
// on one machine at one moment and the cheap way to keep it honest is to notice
// when it stops being true.
//
// Rotation is driven by RE-AUTHENTICATION, not by expiry: the lifetime is 60
// days and the observed token was 1.9 days old. So the store's value changes
// often and `exp` is almost never the reason. That inverts the staleness risk —
// the danger is not "we failed to refresh before expiry", it is "we cached the
// credential and stopped looking at the store", which 401s on a rotated-out
// token while a perfectly good one sits unread three inches away. Absence-vs-zero
// one layer down, and self-inflicted.
//
// `exp` is therefore a cheap PRE-FLIGHT and nothing more. credential_expired
// means the store's CURRENT value is expired or rejected, and its remedy is the
// engineer re-authenticating in Cursor — which no code here can do.
func readCursorCredential() (cursorCredential, error) {
	path, err := cursorStateDBPath()
	if err != nil {
		return cursorCredential{}, err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return cursorCredential{}, credentialErr(CursorVendorAbsenceCredentialAbsent, "no state store on this device")
	}

	values, _, err := readCursorAuthKeys(path)
	if err != nil {
		return cursorCredential{}, err
	}

	// The access token is the bearer. The refresh key is read for the drift
	// check below and never used to authenticate anything.
	token := strings.TrimSpace(values[cursorAuthAccessTokenKey])
	if token == "" {
		token = strings.TrimSpace(values[cursorAuthRefreshTokenKey])
	}
	if token == "" {
		return cursorCredential{}, credentialErr(CursorVendorAbsenceCredentialAbsent, "no token in the state store")
	}

	cred := cursorCredential{token: token, expiresAt: cursorTokenExpiry(token)}
	if !cred.expiresAt.IsZero() && time.Now().After(cred.expiresAt) {
		// PRE-FLIGHT, not the authority. A 401 from the vendor is the other, and
		// both resolve to the same emitted absence — the engineer must re-login
		// in Cursor either way.
		return cursorCredential{}, credentialErr(CursorVendorAbsenceCredentialExpired, "the store's current token is past its exp claim")
	}
	return cred, nil
}

// readCursorAuthKeys pulls exactly the two auth keys out of the store.
//
// THE LIVE FILE IS NEVER OPENED. Not read-write, not read-only, not with
// `immutable=1`. Cursor holds this database open and writes it continuously
// (journal_mode=wal, a 4.9MB -wal alongside a 1GB main file at the time of
// writing); the consistency and locking behaviour of reading it underneath a
// running Electron app is not a property this collector gets to assume, and the
// failure it would buy is a torn or stale read — a quiet wrong number, which is
// the exact class of defect this whole change exists to remove. A read-only
// connection is still a connection.
//
// SO: SNAPSHOT FIRST, OPEN THE SNAPSHOT, DELETE IT.
//
// The obvious objection to that rule is cost — a gigabyte copied every 15
// minutes to read 848 bytes is ~96GB of writes a day on an engineer's laptop,
// which is its own kind of user-visible behaviour. macOS answers it directly:
// clonefile(2) makes a copy-on-write clone that shares the extents and writes
// no data. Measured 2026-08-27 against the live 1GB store with Cursor running —
// clone 5ms, both keys back off the clone in 23ms. The byte copy remains as the
// fallback for a temp dir on another volume or a non-APFS filesystem, so the
// rule holds even where the cheap path does not exist.
//
// Returns the path that was actually opened, so a test can assert it is the
// snapshot and never the live store. That assertion is the guard on the whole
// paragraph above: the rule is invisible in the type system and one refactor
// away from being lost.
func readCursorAuthKeys(path string) (map[string]string, string, error) {
	snapshot, cleanup, err := snapshotCursorStateDB(path)
	if err != nil {
		return nil, "", credentialErr(CursorVendorAbsenceCredentialAbsent, "state store could not be snapshotted")
	}
	defer cleanup()

	values, queryErr := queryCursorAuthKeys("file:" + escapeSQLiteURI(snapshot) + "?mode=ro")
	if queryErr != nil {
		return nil, snapshot, credentialErr(CursorVendorAbsenceCredentialAbsent, "state store unreadable")
	}
	return values, snapshot, nil
}

// queryCursorAuthKeys asks for two rows BY NAME and nothing else.
func queryCursorAuthKeys(dsn string) (map[string]string, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), cursorCredentialReadTimeout)
	defer cancel()

	rows, err := db.QueryContext(ctx,
		`SELECT key, value FROM ItemTable WHERE key IN (?, ?)`,
		cursorAuthAccessTokenKey, cursorAuthRefreshTokenKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = string(value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no cursorAuth rows")
	}
	return out, nil
}

// snapshotCursorStateDB puts a private, disposable copy of the store in a
// 0700 temp directory and returns its path plus a cleanup that removes the
// whole directory.
//
// THE -wal IS PART OF THE SNAPSHOT, and leaving it out would be a subtle
// correctness bug rather than an optimisation: a WAL-mode database copied
// without its write-ahead log is the database AS OF THE LAST CHECKPOINT, and
// this credential rotates whenever the engineer re-authenticates. Reading a
// checkpoint-old store is precisely the stale-credential 401 that §1.3 exists to
// prevent, arriving by a different door.
//
// The -shm is deliberately NOT copied. It is a rebuildable index over the -wal,
// and a stale one is worse than none: SQLite reconstructs it from the log on
// open, which is the behaviour we want.
//
// The two clones are not atomic with respect to a live writer, so the pair can
// be torn. That degrades safely: WAL frames are checksummed, so SQLite recovers
// the valid prefix and we read a slightly older consistent state — and the next
// cycle, fifteen minutes later, reads the store again from scratch.
func snapshotCursorStateDB(path string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "promptster-cursor-state-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- directories require execute; 0700 is private.
		cleanup()
		return "", func() {}, err
	}
	dest := filepath.Join(dir, "state.vscdb")
	if err := snapshotOneFile(path, dest); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if _, statErr := os.Stat(path + "-wal"); statErr == nil {
		// Best-effort: a -wal we could not snapshot leaves us reading the
		// checkpointed state, which is stale rather than wrong, and the absence
		// resolves itself on the next cycle.
		_ = snapshotOneFile(path+"-wal", dest+"-wal")
	}
	return dest, cleanup, nil
}

// snapshotOneFile clones when the platform can and byte-copies when it cannot.
func snapshotOneFile(src, dst string) error {
	if err := cloneFile(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- a platform-derived store path, or a test override.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- destination is inside our private random temp dir.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// escapeSQLiteURI percent-escapes the characters a SQLite URI filename treats
// specially, so a home directory containing '?' or '#' cannot smuggle a
// parameter into the DSN.
func escapeSQLiteURI(path string) string {
	r := strings.NewReplacer("?", "%3f", "#", "%23", "%", "%25")
	return r.Replace(path)
}

// cursorTokenExpiry parses the `exp` claim without verifying the signature.
//
// NOT A SECURITY DECISION — we are not the audience for this token and have no
// key to verify it with. It is a cheap way to skip a request we already know
// will 401, and nothing more. An unparseable token yields a zero time, which
// means "no pre-flight opinion" and lets the vendor decide.
func cursorTokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0).UTC()
}

// cursorCredentialAbsence maps any error from this file onto the emitted
// absence vocabulary, so no failure path can reach the collector as silence.
func cursorCredentialAbsence(err error) CursorVendorAbsenceReason {
	var ce *cursorCredentialError
	if errors.As(err, &ce) {
		return ce.reason
	}
	return CursorVendorAbsenceCredentialAbsent
}

// assertNoCredentialInText is a belt-and-braces guard used by the leak test and
// by the emit path: it reports whether s contains the token. Exported within the
// package so the test asserts the SAME predicate production uses.
func assertNoCredentialInText(s, token string) error {
	if token == "" {
		return nil
	}
	if strings.Contains(s, token) {
		return fmt.Errorf("credential value appeared in %d bytes of text", len(s))
	}
	return nil
}
