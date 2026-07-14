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

// ChainStatePath is the per-session signature-chain index that pairs with
// HookBufferPath. It is deliberately derived from the buffer path rather than
// from StateDir so the two can never be mismatched: redirect
// PROMPTSTER_BUFFER_PATH (as the tests do) and the index follows it.
func ChainStatePath() string {
	return HookBufferPath() + ".chain.json"
}
