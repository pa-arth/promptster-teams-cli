//go:build windows

package sign

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func WithBufferLock(bufferPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(bufferPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(bufferPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	handle := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		return fmt.Errorf("LockFileEx: %w", err)
	}
	defer windows.UnlockFileEx(handle, 0, 1, 0, ol) //nolint:errcheck
	return fn()
}
