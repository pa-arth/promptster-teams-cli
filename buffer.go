package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func hookDebugEnabled() bool {
	if os.Getenv("PROMPTSTER_DEBUG") == "1" {
		return true
	}
	_, err := os.Stat(filepath.Join(stateDir(), "debug-hooks"))
	return err == nil
}

func hookDebugf(format string, args ...interface{}) {
	if !hookDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "promptster-teams: "+format+"\n", args...)
}

func hookDebugLogPath() string {
	return filepath.Join(stateDir(), "hook-debug.log")
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

func hookBufferPath() string {
	if p := os.Getenv("PROMPTSTER_BUFFER_PATH"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "buffer.jsonl")
}

// appendEventToLocalBuffer scrubs secrets at the choke point, signs the event
// (if a keypair exists), chains it to the previous buffered event's signature,
// and appends it under an exclusive lock. It mutates the event with Sig/PrevSig
// so the caller POSTs the signed version unchanged.
//
// The local buffer + signature chain is a trust feature, not surveillance: it
// lets a team independently verify the event stream wasn't tampered with.
func appendEventToLocalBuffer(event *Event) error {
	// Defense-in-depth scrub at the choke point every capture path funnels
	// through, before the event is signed or persisted anywhere.
	scrubEvent(event)
	p := hookBufferPath()
	return withBufferLock(p, func() error {
		priv, err := loadSessionKeypair()
		if err != nil {
			hookDebugf("load session keypair: %v", err)
			// Fall through — still append unsigned so we don't drop events.
		}

		if priv != nil {
			prevSig, err := readLastChainSig(p)
			if err != nil {
				hookDebugf("read last chain sig: %v", err)
				prevSig = ""
			}
			sig, _, err := signEvent(*event, prevSig, priv)
			if err != nil {
				return fmt.Errorf("sign event: %w", err)
			}
			event.Sig = sig
			event.PrevSig = prevSig
		}

		b, err := json.Marshal(event)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
		return nil
	})
}
