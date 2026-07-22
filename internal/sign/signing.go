package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const signingVersion = "PST-EVT-V1"

// sessionKeyPath returns the path to the per-session Ed25519 private key.
// Stored alongside session.json (workspace-local, 0600 perms). Kept in a
// separate file so it never appears in session.json (which gets printed by
// `promptster status` / similar).
func sessionKeyPath() string {
	return filepath.Join(state.StateDir(), "session.key")
}

// generateSessionKeypair creates a new Ed25519 keypair, persists the private
// key to sessionKeyPath() with 0600 perms, and returns the base64 pubkey.
func GenerateSessionKeypair() (pubB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate ed25519 key: %w", err)
	}
	path := sessionKeyPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("mkdir state dir: %w", err)
	}
	// Store seed only (32 bytes) — ed25519.NewKeyFromSeed reconstructs the full 64-byte key.
	if err := os.WriteFile(path, priv.Seed(), 0o600); err != nil {
		return "", fmt.Errorf("write session.key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// loadSessionKeypair reads the seed file and returns the full private key.
// Returns nil (without error) if no key has been provisioned for this session.
func LoadSessionKeypair() (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(sessionKeyPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) != ed25519.SeedSize {
		return nil, fmt.Errorf("session.key has invalid length %d", len(data))
	}
	return ed25519.NewKeyFromSeed(data), nil
}

// canonicalJSON emits a deterministic JSON encoding: object keys sorted
// ascending, no whitespace, array order preserved. Matches the TS
// `canonicalJson` in packages/event-schema/src/signing.ts byte-for-byte so
// signatures verify across languages.
func canonicalJSON(v interface{}) ([]byte, error) {
	var buf strings.Builder
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func writeCanonical(buf *strings.Builder, v interface{}) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		if err := writeJSONString(buf, x); err != nil {
			return err
		}
	case float64:
		// json.Unmarshal decodes all numbers as float64. Emit integer form when
		// the value has no fractional component — matches TS JSON.stringify.
		if x == float64(int64(x)) {
			buf.WriteString(strconv.FormatInt(int64(x), 10))
		} else {
			b, err := json.Marshal(x)
			if err != nil {
				return err
			}
			buf.Write(b)
		}
	case int:
		buf.WriteString(strconv.Itoa(x))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case []interface{}:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		first := true
		for _, k := range keys {
			val := x[k]
			if !first {
				buf.WriteByte(',')
			}
			first = false
			if err := writeJSONString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, val); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		// Fallback: marshal through encoding/json, then re-parse into
		// interface{} and recurse. Handles typed structs like map[string]string.
		raw, err := json.Marshal(x)
		if err != nil {
			return err
		}
		var reparsed interface{}
		if err := json.Unmarshal(raw, &reparsed); err != nil {
			return err
		}
		return writeCanonical(buf, reparsed)
	}
	return nil
}

// writeJSONString mirrors JSON.stringify's string escaping. encoding/json's
// default HTML-safe mode rewrites &, < and > as \u0026/\u003c/\u003e. Those are
// semantically equivalent JSON but NOT byte-equivalent inside the SHA-256 data
// hash, so shell commands such as `a && b > out` signed in Go failed verification
// in the TypeScript backend. Disable HTML escaping for both values and object
// keys; trim Encoder's one framing newline.
func writeJSONString(buf *strings.Builder, s string) error {
	var encoded strings.Builder
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.WriteString(strings.TrimSuffix(encoded.String(), "\n"))
	return nil
}

// buildSigningMessage returns the exact UTF-8 byte string that the Ed25519
// signature covers. Format is fixed and mirrored in TS — do not reorder fields.
func BuildSigningMessage(e event.Event, prevSigHex string) ([]byte, error) {
	dataBytes, err := canonicalJSON(e.Data)
	if err != nil {
		return nil, err
	}
	dataHash := sha256.Sum256(dataBytes)
	sourceIntegration := ""
	if e.Source != "" {
		sourceIntegration = e.Source
	}
	parts := []string{
		signingVersion,
		e.ID,
		e.SessionID,
		e.Ts,
		e.Kind,
		sourceIntegration,
		strconv.Itoa(e.V),
		hex.EncodeToString(dataHash[:]),
		prevSigHex,
		"",
	}
	return []byte(strings.Join(parts, "\n")), nil
}

// signEvent returns the hex-encoded Ed25519 signature and sha256 event hash
// for the given event, given the previous event's signature (hex, or "" for
// the first event in the chain).
func signEvent(e event.Event, prevSigHex string, priv ed25519.PrivateKey) (sigHex, eventHashHex string, err error) {
	msg, err := BuildSigningMessage(e, prevSigHex)
	if err != nil {
		return "", "", err
	}
	sig := ed25519.Sign(priv, msg)
	hash := sha256.Sum256(msg)
	return hex.EncodeToString(sig), hex.EncodeToString(hash[:]), nil
}

// withBufferLock acquires an exclusive lock on the buffer file for the
// duration of fn. Multiple hook binaries (Claude hook, shell hook, git
// watcher, decision capture) all append to the same buffer, so the lock
// serializes chain-append operations across processes. Implementation is
// platform-specific (flock on Unix, LockFileEx on Windows).
