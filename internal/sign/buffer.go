package sign

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// AppendEventToLocalBuffer scrubs secrets at the choke point, signs the event
// (if a keypair exists), chains it to the previous event OF THE SAME SESSION,
// and appends it under an exclusive lock. It mutates the event with Sig/PrevSig
// so the caller POSTs the signed version unchanged.
//
// Chaining is per-session: concurrent sessions interleave in one ledger, so a
// single global chain would link unrelated sessions together and be unwalkable.
// The tip of each session lives in a derived index (chain_state.go) that is
// always rebuildable from the ledger.
//
// captureAssistantProse is the org policy resolved and threaded from the watch
// loop (default false / fail-closed). It only affects ai_response.text; all
// other kinds project identically regardless. Emitters with no assistant text
// (presence, config_census) pass false.
//
// The local buffer + signature chain is a trust feature, not surveillance: it
// lets a team independently verify the event stream wasn't tampered with.
func AppendEventToLocalBuffer(ev *event.Event, captureAssistantProse bool) error {
	// Source exclusion + secret scrub at the choke point every capture path
	// funnels through, before the event is signed or persisted anywhere.
	// projectEvent strips source-bearing fields (diffs, stdout, file contents,
	// assistant text, RawPayload) so they never reach the buffer, the signature,
	// or the wire; scrubEvent then redacts secrets from what remains. With the
	// org policy on, ai_response prose survives — code-scrubbed on-device.
	redact.ProjectEvent(ev, captureAssistantProse)
	redact.ScrubEvent(ev)
	p := state.HookBufferPath()
	csPath := state.ChainStatePath()
	// One lock covers both the ledger append and the index update. flock is
	// advisory and protects whatever fn touches, not the bytes of p — so taking
	// the buffer lock and only ever touching chain-state.json from inside it
	// makes the whole read->sign->append->commit sequence atomic against the
	// four concurrent emitters (both watchers, presence, census) and against
	// other processes. Do not add a second lock or a sync.Mutex: the former
	// invents a lock-ordering invariant, the latter is redundant in-process and
	// useless across processes.
	return WithBufferLock(p, func() error {
		priv, err := LoadSessionKeypair()
		if err != nil {
			state.HookDebugf("load session keypair: %v", err)
			// Fall through — still append unsigned so we don't drop events.
		}

		var cs chainState
		var indexPersistable bool
		if priv != nil {
			cs, indexPersistable = loadOrRebuildChainState(p, csPath)
			// Link to the tip of THIS event's session, not to whatever was
			// appended last. Concurrent sessions interleave in the ledger, so a
			// global tip would chain unrelated sessions together.
			prevSig := cs.prevSigFor(ev.SessionID)
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
		// #nosec G304 -- p is state.HookBufferPath(), derived from state.StateDir(), not user input.
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}

		// Commit the tip only AFTER the event is durably in the ledger, and
		// never fail the append on an index write — the event is already
		// written and POSTed, and the index rebuilds from the ledger.
		//
		// The ordering is load-bearing. Crashing between the append and this
		// commit leaves a stale tip, so the session's next event re-links to an
		// older tip: a visible, benign fork with the ledger intact. Committing
		// first and crashing before the append would leave the tip naming an
		// event that exists nowhere — a dangling reference, indistinguishable
		// to an auditor from a deleted event. A crash must never manufacture
		// the evidence of the exact attack this chain exists to detect.
		if priv != nil {
			nowMs := time.Now().UnixMilli()
			cs.setTip(ev.SessionID, ev.Sig, nowMs)
			cs.prune(nowMs)
			switch {
			case !indexPersistable:
				// The tips came from an incomplete rebuild, so writing them back
				// would freeze a partial index in place and stop future rebuilds.
				// Leave it absent and re-derive from the ledger next append.
				// rebuildChainStateFromBuffer already warned.
			case writeChainState(csPath, cs) != nil:
				// A stale index is NOT self-healing: it stays valid JSON, so
				// readChainState keeps accepting it and every later event of this
				// session re-links to the same frozen tip — a silent star, not a
				// chain. Drop the index so the next append rebuilds from the
				// ledger, which already holds this event.
				chainWarnf("could not write index — dropping it so the next append rebuilds from the ledger")
				if err := os.Remove(csPath); err != nil && !os.IsNotExist(err) {
					chainWarnf("could not drop stale index (%v) — subsequent events in this session may re-link to a stale tip", err)
				}
			}
		}
		return nil
	})
}
