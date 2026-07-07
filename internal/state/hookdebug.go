package state

import (
	"fmt"
	"os"
	"path/filepath"
)

func hookDebugEnabled() bool {
	if os.Getenv("PROMPTSTER_DEBUG") == "1" {
		return true
	}
	_, err := os.Stat(filepath.Join(StateDir(), "debug-hooks"))
	return err == nil
}

func HookDebugf(format string, args ...interface{}) {
	if !hookDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "promptster-teams: "+format+"\n", args...)
}

func HookBufferPath() string {
	if p := os.Getenv("PROMPTSTER_BUFFER_PATH"); p != "" {
		return p
	}
	return filepath.Join(StateDir(), "buffer.jsonl")
}
