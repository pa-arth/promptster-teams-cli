//go:build !linux && !darwin

package capture

// reapInheritedChildren is a no-op here: Windows updates respawn instead of
// exec'ing in place, so the watcher never inherits children it didn't start.
func reapInheritedChildren() {}
