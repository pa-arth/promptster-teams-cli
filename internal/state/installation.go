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
		if published := publishInstallationID(path, value); published != "" {
			return published
		}
	}

	// Persistence failures must not collapse every home on a machine onto the
	// same identity. Hashing this local path happens at the caller; the path
	// itself never leaves the process.
	return "ins-path-" + filepath.Clean(StateDir())
}

// publishInstallationID writes a complete temp file, closes it, and only then
// hard-links it into the canonical name. Link is an atomic create-if-absent:
// concurrent first starts either publish their complete value or read the
// complete winner. No process can observe the empty/partial canonical file that
// O_CREATE|O_EXCL followed by Write exposed.
func publishInstallationID(path, value string) string {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	// #nosec G304 -- dir is always StateDir(), not user input.
	tmp, err := os.CreateTemp(dir, "installation-id-*.tmp")
	if err != nil {
		return ""
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return ""
	}
	_, writeErr := tmp.WriteString(value + "\n")
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ""
	}
	if err := os.Link(tmpPath, path); err == nil {
		return value
	}
	return readInstallationID(path)
}

func readInstallationID(path string) string {
	// #nosec G304 -- callers pass only StateDir()/installation-id.
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(data))
	if strings.HasPrefix(value, "ins-") && len(value) == 36 {
		if _, err := hex.DecodeString(strings.TrimPrefix(value, "ins-")); err != nil {
			return ""
		}
		return value
	}
	return ""
}
