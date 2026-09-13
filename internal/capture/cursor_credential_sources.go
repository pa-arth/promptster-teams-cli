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
	tokens := []struct {
		kind  cursorCredentialSourceKind
		token string
	}{
		{cursorSourceAgentKeychain, readCursorAgentKeychainToken()},
		{cursorSourceAgentFile, readCursorAgentAuthFileToken()},
	}
	authID, emailHMAC, authInfoRead := "", "", false
	for _, t := range tokens {
		if t.token == "" {
			continue
		}
		if !authInfoRead {
			authID, emailHMAC = readCursorAgentAuthInfo(state.CursorAttributionKey())
			authInfoRead = true
		}
		sourceEmail := ""
		if sub := strings.TrimSpace(cursorTokenClaims(t.token).Sub); sub != "" && sub == authID {
			sourceEmail = emailHMAC
		}
		cred, err := cursorCredentialFromToken(t.token, sourceEmail)
		sources = append(sources, cursorCredentialSource{kind: t.kind, cred: cred, err: err})
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
	if err != nil || home == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".cursor", "auth.json")) // #nosec G304 -- fixed path under the engineer's home.
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
	if dir == "" {
		return "", ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "cli-config.json")) // #nosec G304 -- cursor-agent's config path.
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
