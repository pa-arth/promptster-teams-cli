//go:build !windows

package sign

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func WithBufferLock(bufferPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(bufferPath), 0o700); err != nil {
		return err
	}
	// #nosec G304 -- bufferPath is always state.StateDir()-derived across all callers (HookBufferPath + dedup ledger locks), not user input.
	f, err := os.OpenFile(bufferPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}
