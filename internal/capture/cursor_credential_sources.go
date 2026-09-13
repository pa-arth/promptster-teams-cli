package capture

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// openspec cursor-vendor-multi-account, Phase B §2.1 — EVERY CURSOR LOGIN ON
// THIS DEVICE, read fresh each cycle (D1 = a: read what is present, store
// nothing).
//
// A device has at most two readable logins, and they can differ:
//
//   - the IDE's, in globalStorage/state.vscdb (readCursorCredential, Phase A);
//   - cursor-agent's. Bundle 2026.08.25-3e8eec8, `cli-credentials`: a keychain
//     store with account `${domain}-user` and service `${domain}-access-token`,
//     or a file store at `~/.${domain}/auth.json` ({accessToken, refreshToken,
//     apiKey, bedrockCredentials}), both built with domain "cursor"
//     (`jo({domain:"cursor",store})`).
//
// Each store holds ONE login. Two sources with the same `sub` are one account
// and are collected once (see pollCursorVendorAccounts).
//
// THE KEYCHAIN IS READ BY SHELLING OUT TO /usr/bin/security, NOT A GO KEYCHAIN
// LIBRARY. Release binaries are ad-hoc signed (Identifier=a.out), so a keychain
// ACL granted to "this binary" is lost on every auto-update and would re-prompt
// the engineer. `security` has a stable Apple signature. Anything short of a
// clean exit with output — not found, locked, timeout, denied — means "this
// source is absent this cycle", never a failed cycle.
//
// cursor-agent's EMAIL is not in its credential store. The only local copy is
// `authInfo.email` in cli-config.json, written by cursor-agent's own
// `auth-info-sync` from a GetMe call. We read that file and make no network
// call for it. The email is used only when `authInfo.authId` equals the token's
// `sub`: that file is a cache refreshed separately from login, and a stale
// email would stamp a turn with the WRONG account. An unmatched turn reads
// `unreadable:`, which can be counted. A wrong ref can't be told apart from a
// right one.

type cursorCredentialSourceKind string

const (
	cursorSourceIDE           cursorCredentialSourceKind = "ide"
	cursorSourceAgentKeychain cursorCredentialSourceKind = "cursor-agent-keychain"
	cursorSourceAgentFile     cursorCredentialSourceKind = "cursor-agent-file"

	cursorSecurityBinary             = "/usr/bin/security"
	cursorAgentKeychainAccount       = "cursor-user"
	cursorAgentKeychainAccessService = "cursor-access-token" // #nosec G101 -- a keychain SERVICE name, not a credential.
)

// cursorCredentialSource is one store's reading this cycle. err non-nil means
// the store is present but unusable (e.g. expired); cred.accountRef still names
// the login when the token had a parseable sub.
type cursorCredentialSource struct {
	kind cursorCredentialSourceKind
	cred cursorCredential
	err  error
}

var (
	// cursorAgentSourcesSupported gates the cursor-agent sources to macOS, the
	// only platform this rail supports (design §10). A var so tests on the CI
	// matrix exercise them.
	cursorAgentSourcesSupported = runtime.GOOS == "darwin"

	// cursorKeychainReadTimeout bounds one `security` call. A var for tests.
	cursorKeychainReadTimeout = 3 * time.Second

	// cursorSecurityRead runs /usr/bin/security and returns stdout. Tests
	// replace it; nothing in the test suite reads a real keychain.
	cursorSecurityRead = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, cursorSecurityBinary, args...).Output() // #nosec G204 -- fixed absolute binary, constant args.
	}
)

// readCursorCredentials returns every readable source, IDE first. The IDE
// source is always present (with its Phase A error when the store is missing);
// cursor-agent sources appear only when their store yielded a token.
func readCursorCredentials() []cursorCredentialSource {
	ide, err := readCursorCredential()
	sources := []cursorCredentialSource{{kind: cursorSourceIDE, cred: ide, err: err}}
	if !cursorAgentSourcesSupported {
		return sources
	}
	// ONE cursor-agent store, never both. cursor-agent picks exactly one
	// (bundle 2026.08.25-3e8eec8, `cli-credentials` `jo`):
	//   "memory"===n ? memory : "file"===n ? file : darwin ? keychain : file
	// where n is `AGENT_CLI_CREDENTIAL_STORE` ("file" | "memory" | "default").
	// It never falls back from keychain to file at runtime. Our daemon cannot
	// see cursor-agent's environment, so the nearest faithful rule is: the
	// keychain wins whenever it yields a token, and auth.json is read only when
	// it yields nothing. A stale auth.json left by a store switch is therefore
	// never collected as a second, live account.
	kind, token := cursorSourceAgentKeychain, readCursorAgentKeychainToken()
	if token == "" {
		kind, token = cursorSourceAgentFile, readCursorAgentAuthFileToken()
	}
	if token != "" {
		authID, emailHMAC := readCursorAgentAuthInfo(state.CursorAttributionKey())
		sourceEmail := ""
		if sub := strings.TrimSpace(cursorTokenClaims(token).Sub); sub != "" && sub == authID {
			sourceEmail = emailHMAC
		}
		cred, err := cursorCredentialFromToken(token, sourceEmail)
		sources = append(sources, cursorCredentialSource{kind: kind, cred: cred, err: err})
	}
	for i := range sources {
		// A token with no parseable `sub` is not a usable credential: it has no
		// account to scope a snapshot to, and two such tokens would merge under
		// "unknown". Treated as that source being absent — no vendor call, no
		// login-map entry.
		if sources[i].err == nil && sources[i].cred.accountRef == cursorAccountRefUnknown {
			sources[i] = cursorCredentialSource{kind: sources[i].kind,
				err: credentialErr(CursorVendorAbsenceCredentialAbsent, "token has no parseable sub")}
		}
	}
	return sources
}

// readCursorAgentKeychainToken returns cursor-agent's access token, or "" for
// any failure. The `-w` output lives only in the returned string.
func readCursorAgentKeychainToken() string {
	ctx, cancel := context.WithTimeout(context.Background(), cursorKeychainReadTimeout)
	defer cancel()
	out, err := cursorSecurityRead(ctx, "find-generic-password",
		"-a", cursorAgentKeychainAccount, "-s", cursorAgentKeychainAccessService, "-w")
	if err != nil || ctx.Err() != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readCursorAgentAuthFileToken reads cursor-agent's file-store fallback,
// `~/.cursor/auth.json` on darwin. Only `accessToken` is decoded.
func readCursorAgentAuthFileToken() string {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	// #nosec G304 G703 -- absolute, Cleaned home dir with a constant relative path; read-only.
	b, err := os.ReadFile(filepath.Join(filepath.Clean(home), ".cursor", "auth.json"))
	if err != nil {
		return ""
	}
	var f struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	return strings.TrimSpace(f.AccessToken)
}

// readCursorAgentAuthInfo returns cursor-agent's cached `authInfo.authId` and
// the HMAC of `authInfo.email`. The raw address is dropped on return.
//
// Path resolution mirrors cursor-agent's `cursor-config/dist/paths.js`:
// $CURSOR_CONFIG_DIR, else $XDG_CONFIG_HOME/cursor, else ~/.cursor.
func readCursorAgentAuthInfo(installKey []byte) (authID, emailHMAC string) {
	dir := strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR"))
	if dir == "" {
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			dir = filepath.Join(xdg, "cursor")
		} else if home, err := os.UserHomeDir(); err == nil && home != "" {
			dir = filepath.Join(home, ".cursor")
		}
	}
	// The directory comes from the environment, so only an absolute path is
	// accepted and it is Cleaned; the basename is a constant.
	if dir == "" || !filepath.IsAbs(dir) {
		return "", ""
	}
	// #nosec G304 G703 -- absolute, Cleaned config dir (cursor-agent's own env resolution) with a constant basename; read-only.
	b, err := os.ReadFile(filepath.Join(filepath.Clean(dir), "cli-config.json"))
	if err != nil {
		return "", ""
	}
	var f struct {
		AuthInfo struct {
			AuthID string `json:"authId"`
			Email  string `json:"email"`
		} `json:"authInfo"`
	}
	if json.Unmarshal(b, &f) != nil {
		return "", ""
	}
	return strings.TrimSpace(f.AuthInfo.AuthID), cursorEmailHMAC(installKey, f.AuthInfo.Email)
}
