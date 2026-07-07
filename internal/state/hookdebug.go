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

func hookDebugLogPath() string {
	return filepath.Join(StateDir(), "hook-debug.log")
}

func hookDebugAppend(line string) {
	if !hookDebugEnabled() {
		return
	}
	p := hookDebugLogPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func HookBufferPath() string {
	if p := os.Getenv("PROMPTSTER_BUFFER_PATH"); p != "" {
		return p
	}
	return filepath.Join(StateDir(), "buffer.jsonl")
}
