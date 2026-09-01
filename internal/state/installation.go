package state

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// InstallationID is the stable, anonymous identity of one Promptster state
// directory. A physical machine can have several user homes, and therefore
// several independent daemons, outboxes, signing keys, and progress ledgers.
// Giving those independent chains one machine-derived device id makes one
// silent installation indistinguishable from a healthy sibling.
//
// The value never contains a username or path. It is generated once, stored
// beside the state it identifies, and survives upgrades. PROMPTSTER_STATE_DIR
// naturally gives tests and deliberately isolated installations independent
// identities.
func InstallationID() string {
	path := filepath.Join(StateDir(), "installation-id")
	if value := readInstallationID(path); value != "" {
		return value
	}

	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		value := "ins-" + hex.EncodeToString(raw[:])
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			// O_EXCL makes concurrent first starts converge on one persisted value.
			if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
				_, writeErr := f.WriteString(value + "\n")
				closeErr := f.Close()
				if writeErr == nil && closeErr == nil {
					return value
				}
				_ = os.Remove(path)
			} else if existing := readInstallationID(path); existing != "" {
				return existing
			}
		}
	}

	// Persistence failures must not collapse every home on a machine onto the
	// same identity. Hashing this local path happens at the caller; the path
	// itself never leaves the process.
	return "ins-path-" + filepath.Clean(StateDir())
}

func readInstallationID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(data))
	if strings.HasPrefix(value, "ins-") && len(value) <= 80 {
		return value
	}
	return ""
}
