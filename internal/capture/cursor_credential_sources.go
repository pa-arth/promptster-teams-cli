package capture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// openspec cursor-vendor-multi-account §2.1 — EVERY CURSOR LOGIN ON THE DEVICE.
//
// The IDE store holds one login at a time, and so does cursor-agent. They can
// differ, so each cycle reads every source present and collects one snapshot
// per distinct accountRef. Nothing is retained between cycles (D1 = a).
//
// cursor-agent's stores, read from the 2026.09.10-fd3934a bundle code (no store
// opened):
//   - `AGENT_CLI_CREDENTIAL_STORE` picks the store: "file", "memory", or default.
//     The default is the macOS KEYCHAIN on darwin and the auth.json file
//     elsewhere. So on macOS the file exists only when cursor-agent runs with
//     AGENT_CLI_CREDENTIAL_STORE=file, or if a file was left behind from before.
//   - auth.json: darwin `~/.cursor/auth.json`, otherwise
//     `$XDG_CONFIG_HOME/cursor/auth.json` (default `~/.config`). The name is the
//     constant "cursor" ("cursor-dev" on dev builds). Schema: `{accessToken,
//     refreshToken, apiKey, bedrockCredentials}`. It holds tokens and NO email.
//   - cli-config.json (`$CURSOR_CONFIG_DIR`, else `$XDG_CONFIG_HOME/cursor`, else
//     `~/.cursor`) carries `authInfo {authId, email, ...}` from cursor-agent's
//     GetMe. authId is the JWT `sub`: on the owner's Mac, 2026-09-15,
//     sha256(authId)[:16] equalled that login's accountRef in the login map. So
//     it is an ATTRIBUTION-ONLY source: every cycle the login map learns
//     {HMAC(email), sha256(authId)[:16]} from it, even when the login's token
//     sits in the keychain and is not read. It never creates a snapshot or an
//     absence, because collection needs a token.
//
// TODO(openspec cursor-vendor-multi-account tasks §2.1 step 2): the keychain
// (acct "cursor-user", svce "cursor-access-token"/"cursor-refresh-token") is
// NOT read. It waits on the owner's check that a `/usr/bin/security` read is
// prompt-free. It becomes a third cursorCredentialSourceKind with its item name
// in `path`.

type cursorCredentialSourceKind string

const (
	cursorSourceIDEStateDB       cursorCredentialSourceKind = "ide_state_vscdb"
	cursorSourceAgentAuthFile    cursorCredentialSourceKind = "cursor_agent_auth_json"
	cursorAgentDirEnv                                       = "PROMPTSTER_CURSOR_AGENT_DIR" // TEST-ONLY: a dir holding auth.json and cli-config.json.
	cursorAgentFileMaxBytes                                 = 1 << 20
	cursorAgentAuthFileName                                 = "auth.json"
	cursorAgentCLIConfigFileName                            = "cli-config.json"
)

// errCursorAgentFileOversized marks a cursor-agent file past
// cursorAgentFileMaxBytes. It is skipped as oversized rather than read
// truncated, which would look malformed and hide the cause.
var errCursorAgentFileOversized = errors.New("cursor-agent file oversized")

// cursorCredentialSource is one store read this cycle. cred.accountRef is the
// source's accountRef, set on success and on an expired token. path names the
// store and is the only part of a source that may be logged.
type cursorCredentialSource struct {
	kind cursorCredentialSourceKind
	path string
	cred cursorCredential
	err  error
}

// readCursorCredentialSources reads every source present, fresh. The IDE source
// is always first and always present, so its absence reason is the one reported
// when no source identifies a login, exactly as before this list existed.
func readCursorCredentialSources() []cursorCredentialSource {
	idePath, _ := cursorStateDBPath()
	cred, err := readCursorCredential()
	sources := []cursorCredentialSource{{kind: cursorSourceIDEStateDB, path: idePath, cred: cred, err: err}}
	if agent, ok := readCursorAgentAuthFile(); ok {
		sources = append(sources, agent)
	}
	return sources
}

// cursorAccountsToCollect keeps one source per identified accountRef, preferring
// a usable credential over an expired one. Sources that identified no login are
// dropped. The caller reports those only when nothing else was identified.
func cursorAccountsToCollect(sources []cursorCredentialSource) []cursorCredentialSource {
	var out []cursorCredentialSource
	index := map[string]int{}
	for _, s := range sources {
		ref := s.cred.accountRef
		if ref == "" || ref == cursorAccountRefUnknown {
			continue
		}
		if i, seen := index[ref]; seen {
			if out[i].err != nil && s.err == nil {
				out[i] = s
			}
			continue
		}
		index[ref] = len(out)
		out = append(out, s)
	}
	return out
}

func cursorAgentAuthFilePath() string {
	if dir := os.Getenv(cursorAgentDirEnv); dir != "" {
		return filepath.Join(dir, cursorAgentAuthFileName)
	}
	if !CursorVendorPlatformSupported(runtime.GOOS) {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, ".cursor", cursorAgentAuthFileName)
	}
	cfg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	return filepath.Join(cfg, "cursor", cursorAgentAuthFileName)
}

// ponytail: env is the daemon's, not the engineer's shell. A CURSOR_CONFIG_DIR
// or XDG_CONFIG_HOME set only in a shell profile is not seen here, and that
// login collects without attribution.
func cursorAgentCLIConfigPath() string {
	if dir := os.Getenv(cursorAgentDirEnv); dir != "" {
		return filepath.Join(dir, cursorAgentCLIConfigFileName)
	}
	if !CursorVendorPlatformSupported(runtime.GOOS) {
		return ""
	}
	if dir := strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, cursorAgentCLIConfigFileName)
	}
	if cfg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); cfg != "" {
		return filepath.Join(cfg, "cursor", cursorAgentCLIConfigFileName)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cursor", cursorAgentCLIConfigFileName)
}

// readCursorAgentAuthFile reads cursor-agent's auth.json. ok=false when there is
// no file (the common case on macOS, where the keychain is the default): a
// missing optional store is not a source and emits nothing.
//
// Unlike the SQLite store, a JSON file cannot be read by key, so the whole file
// (apiKey and bedrockCredentials included) passes through memory. Only the two
// token fields are decoded, and only the token leaves this function.
func readCursorAgentAuthFile() (cursorCredentialSource, bool) {
	path := cursorAgentAuthFilePath()
	if path == "" {
		return cursorCredentialSource{}, false
	}
	src := cursorCredentialSource{kind: cursorSourceAgentAuthFile, path: path}
	b, err := readCursorAgentFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return src, false
	}
	if errors.Is(err, errCursorAgentFileOversized) {
		src.err = credentialErr(CursorVendorAbsenceCredentialAbsent, "cursor-agent auth file oversized")
		return src, true
	}
	if err != nil {
		src.err = credentialErr(CursorVendorAbsenceCredentialAbsent, "cursor-agent auth file unreadable")
		return src, true
	}
	var f struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if json.Unmarshal(b, &f) != nil {
		src.err = credentialErr(CursorVendorAbsenceCredentialAbsent, "cursor-agent auth file unparseable")
		return src, true
	}
	token := strings.TrimSpace(f.AccessToken)
	if token == "" {
		token = strings.TrimSpace(f.RefreshToken)
	}
	// No email here: the auth.json login is attributed by cursorAgentConfigLogin,
	// which learns the same {emailHmac, ref} whenever cli-config names this login.
	src.cred, src.err = cursorCredentialFromToken(token, "")
	return src, true
}

// cursorAgentConfigLogin reads cli-config.json authInfo as a login for hook
// attribution ONLY: an accountRef and email HMAC, never a token. ok=false for a
// missing or malformed file, or an authInfo without both an authId and an email.
// The address is held only long enough to HMAC it.
func cursorAgentConfigLogin() (cursorCredential, bool) {
	path := cursorAgentCLIConfigPath()
	if path == "" {
		return cursorCredential{}, false
	}
	b, err := readCursorAgentFile(path)
	if err != nil {
		if errors.Is(err, errCursorAgentFileOversized) && verboseWatch() {
			fmt.Fprintf(os.Stderr, "cursor-vendor: cursor_agent_cli_config at %s: oversized\n", path)
		}
		return cursorCredential{}, false
	}
	var c struct {
		AuthInfo struct {
			AuthID string `json:"authId"`
			Email  string `json:"email"`
		} `json:"authInfo"`
	}
	if json.Unmarshal(b, &c) != nil {
		return cursorCredential{}, false
	}
	login := cursorCredential{accountRef: cursorSubAccountRef(c.AuthInfo.AuthID),
		emailHMAC: cursorEmailHMAC(state.CursorAttributionKey(), c.AuthInfo.Email)}
	if login.accountRef == cursorAccountRefUnknown || login.emailHMAC == "" {
		return cursorCredential{}, false
	}
	return login, true
}

func readCursorAgentFile(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- a fixed cursor-agent store path, or a test override.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// One byte past the limit tells "exactly at the limit" from "truncated".
	b, err := io.ReadAll(io.LimitReader(f, cursorAgentFileMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > cursorAgentFileMaxBytes {
		return nil, errCursorAgentFileOversized
	}
	return b, nil
}
