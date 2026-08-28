//go:build !darwin

package capture

import "errors"

// cloneFile has no portable equivalent. Every non-darwin platform falls through
// to the byte copy — which is moot in v1, since the collector refuses to run
// anywhere but macOS (CursorVendorSupportedPlatforms). It exists so the copy
// path stays compilable and testable on the six-platform cross-build.
func cloneFile(_, _ string) error { return errors.New("clonefile: unsupported platform") }
