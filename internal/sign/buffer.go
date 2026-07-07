package sign

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// appendEventToLocalBuffer scrubs secrets at the choke point, signs the event
// (if a keypair exists), chains it to the previous buffered event's signature,
// and appends it under an exclusive lock. It mutates the event with Sig/PrevSig
// so the caller POSTs the signed version unchanged.
//
// The local buffer + signature chain is a trust feature, not surveillance: it
// lets a team independently verify the event stream wasn't tampered with.
func AppendEventToLocalBuffer(ev *event.Event) error {
	// Source exclusion + secret scrub at the choke point every capture path
	// funnels through, before the event is signed or persisted anywhere.
	// projectEvent strips source-bearing fields (diffs, stdout, file contents,
	// assistant text, RawPayload) so they never reach the buffer, the signature,
	// or the wire; scrubEvent then redacts secrets from what remains.
	redact.ProjectEvent(ev)
	redact.ScrubEvent(ev)
	p := state.HookBufferPath()
	return WithBufferLock(p, func() error {
		priv, err := LoadSessionKeypair()
		if err != nil {
			state.HookDebugf("load session keypair: %v", err)
			// Fall through — still append unsigned so we don't drop events.
		}

		if priv != nil {
			prevSig, err := readLastChainSig(p)
			if err != nil {
				state.HookDebugf("read last chain sig: %v", err)
				prevSig = ""
			}
			sig, _, err := signEvent(*ev, prevSig, priv)
			if err != nil {
				return fmt.Errorf("sign event: %w", err)
			}
			ev.Sig = sig
			ev.PrevSig = prevSig
		}

		b, err := json.Marshal(ev)
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
