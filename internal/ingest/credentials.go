package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Credential resolution for the teams CLI. A developer's ingest credential is a
// per-engineer key (PSE-XXXX-XXXX) their manager minted. It is resolved, in
// order of precedence:
//
//	--key flag  >  PROMPTSTER_TEAMS_TOKEN env  >  stored credentials file
//
// The stored file (~/.promptster-teams/credentials, mode 0600) is written by
// `promptster-teams login` so an IC pastes the key once and never thinks about
// it again. The API URL resolves the same way, falling back to the hosted
// default (see api.go).

// engineerKeyRe matches the PSE- key format minted by the backend
// (POST /v1/team/engineers): two 4-char base32 segments, charset
// ABCDEFGHJKLMNPQRSTUVWXYZ23456789 (no I/O/0/1).
var engineerKeyRe = regexp.MustCompile(`^PSE-[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$`)

func IsEngineerKey(s string) bool { return engineerKeyRe.MatchString(strings.TrimSpace(s)) }

// storedCredentials is the on-disk shape of the login credential file.
type StoredCredentials struct {
	Token  string `json:"token"`
	ApiURL string `json:"apiUrl,omitempty"`
}

// credentialsPath returns ~/.promptster-teams/credentials.
func CredentialsPath() string {
	return filepath.Join(state.GlobalPromptsterDir(), "credentials")
}

func loadStoredCredentials() (StoredCredentials, error) {
	var c StoredCredentials
	data, err := os.ReadFile(CredentialsPath())
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return StoredCredentials{}, err
	}
	return c, nil
}

// saveStoredCredentials writes the credential file with 0600 perms (0700 dir).
func SaveStoredCredentials(c StoredCredentials) error {
	dir := state.GlobalPromptsterDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	p := CredentialsPath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// resolveToken returns the ingest token and a human-readable source label,
// following flag > env > stored-file precedence. Returns ("", "none") when no
// credential is configured anywhere.
func ResolveToken(flagKey string) (token, source string) {
	if t := strings.TrimSpace(flagKey); t != "" {
		return t, "--key flag"
	}
	if t := strings.TrimSpace(os.Getenv("PROMPTSTER_TEAMS_TOKEN")); t != "" {
		return t, "PROMPTSTER_TEAMS_TOKEN env"
	}
	if c, err := loadStoredCredentials(); err == nil && strings.TrimSpace(c.Token) != "" {
		return strings.TrimSpace(c.Token), "stored credentials"
	}
	return "", "none"
}

// resolveAPIURL returns the ingest base URL following flag > env > stored-file
// precedence, falling back to the hosted default (defaultAPIURL in api.go).
func ResolveAPIURL(flagURL string) string {
	if u := strings.TrimSpace(flagURL); u != "" {
		return u
	}
	if u := strings.TrimSpace(os.Getenv("PROMPTSTER_TEAMS_API_URL")); u != "" {
		return u
	}
	if c, err := loadStoredCredentials(); err == nil && strings.TrimSpace(c.ApiURL) != "" {
		return strings.TrimSpace(c.ApiURL)
	}
	return DefaultAPIURL
}

// maskKey renders a key for display without revealing it: PSE-…-WXYZ.
func MaskKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(none)"
	}
	if len(s) <= 8 {
		return "****"
	}
	return fmt.Sprintf("%s…%s", s[:4], s[len(s)-4:])
}
