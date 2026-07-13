package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// expectedSum finds the hex sha256 for asset in a SHA256SUMS body (lines of
// "<hex>  <name>"). Returns an error when the asset has no line, so a checksum
// file that omits our platform is a hard failure, never a silent skip.
func expectedSum(sums []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: "<64-hex>  <filename>". Split on whitespace so one-or-two
		// spaces both parse; the filename is the last field.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("selfupdate: no checksum for %q in SHA256SUMS", asset)
}

// verifyFileSum computes path's sha256 and checks it equals wantHex. Any
// mismatch — or a file that can't be read — is an error, so a corrupted or
// swapped-in binary is rejected before it can be made executable.
func verifyFileSum(path, wantHex string) error {
	// #nosec G304 -- path is a temp file the updater just downloaded into the
	// install dir; hashed read-only to verify it before any swap.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, wantHex) {
		return fmt.Errorf("selfupdate: sha256 mismatch: got %s want %s", got, wantHex)
	}
	return nil
}
