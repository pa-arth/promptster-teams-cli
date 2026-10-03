package capture

// reapInheritedChildren is a no-op on Windows: updates there respawn instead of
// exec'ing in place, so the watcher never inherits children it didn't start.
func reapInheritedChildren() {}
