package normalize

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// Claude Code's DESKTOP APP stores each GUI session as a SINGLE JSON blob (not
// the append-only JSONL the terminal CLI writes), at
//
//	~/Library/Application Support/Claude/claude-code-sessions/<workspace-id>/<session-id>/local_<uuid>.json
//
// plus a sibling local-agent-mode-sessions/ store. The desktop watcher
// (internal/capture/cmd_desktop_watch.go) re-reads a blob whenever its
// mtime+size changes and hands the whole scrubbed byte slice here; this
// processor is the schema-parser for that blob.
//
// SCHEMA NOT YET KNOWN. The internal shape of local_*.json (its message array,
// per-message discriminators, tool-call encoding, token-usage fields) has not
// been reverse-engineered — a redacted sample is pending. So this is a
// deliberate STUB: it validates the blob is JSON, fires a loud drift guard on an
// unrecognized shape, tracks a cursor for future incremental emit, and emits
// ZERO events. The surrounding read/redact/queue plumbing in the watcher is
// complete and correct; only the message extraction below is stubbed.

// desktopDriftGuardMinBytes is the size floor above which an unparseable /
// unrecognized blob is treated as real schema drift worth a loud warning. Tiny
// or empty files (a freshly-created session, a truncated write caught mid-flush)
// are skipped silently — they are expected and not evidence the schema changed.
const desktopDriftGuardMinBytes = 512

// DesktopSessionProcessor converts a Claude desktop-app session blob into
// canonical Events. It is stateful across re-reads of the SAME blob: cursor is
// the count of messages already emitted, so a later read (the desktop app
// rewrites the whole file as the session grows) only emits the newly-appended
// tail rather than re-emitting the entire history. Mirrors the
// CodexRolloutProcessor / ClaudeTranscriptProcessor construction contract.
type DesktopSessionProcessor struct {
	sessionID string
	// cursor is the number of messages already turned into events for this
	// session. TODO(desktop-schema) advances it as the message array is parsed so
	// a re-read of a grown blob only emits messages[cursor:].
	cursor int
	// warned suppresses repeat drift-guard spam: once a blob's shape has been
	// flagged unrecognized, don't re-warn on every 3s re-read of the same file.
	warned bool
}

// NewDesktopSessionProcessor builds a processor for one desktop session blob.
func NewDesktopSessionProcessor(sessionID string) *DesktopSessionProcessor {
	return &DesktopSessionProcessor{sessionID: sessionID}
}

// stableEventID derives a deterministic event id from a STABLE per-message
// source key scoped by session and kind, so re-reading a rewritten blob collapses
// to ONE backend row per message instead of duplicating on every poll. Empty
// sourceKey falls back to a random id. Mirrors ClaudeTranscriptProcessor and
// CodexRolloutProcessor.stableEventID — kept for the schema implementation to
// use once messages are parsed.
func (p *DesktopSessionProcessor) stableEventID(sourceKey, kind string) string {
	if sourceKey == "" {
		return event.NewUUID()
	}
	return event.DeterministicUUID(p.sessionID + "\x1f" + kind + "\x1f" + sourceKey)
}

// Process parses one desktop-app session blob and returns zero or more canonical
// events. Signature mirrors CodexRolloutProcessor.Process (byte slice in, event
// slice out) so the watcher's per-file loop is identical across channels.
//
// Currently a STUB: it validates the blob unmarshals and has a recognizable
// message array; on a malformed or unrecognized non-trivial blob it fires the
// drift guard and returns nil; on a recognized shape it STILL returns nil until
// the schema is implemented. It never panics and never emits garbage.
func (p *DesktopSessionProcessor) Process(blob []byte) []event.Event {
	// Empty / whitespace / tiny partial writes: nothing to do, and too small to
	// be evidence of schema drift.
	if len(blob) < desktopDriftGuardMinBytes {
		var probe interface{}
		if json.Unmarshal(blob, &probe) != nil {
			// Small AND unparseable — almost certainly a truncated mid-flush write.
			// Silent: the next poll re-reads the completed blob.
			return nil
		}
		return nil
	}

	// Unmarshal the top-level blob. The desktop store writes a JSON OBJECT per
	// session; a decode failure on a non-trivial file is schema drift (or
	// corruption), not a normal state.
	var top map[string]interface{}
	if err := json.Unmarshal(blob, &top); err != nil {
		p.fireDriftGuard(fmt.Sprintf("blob is not a JSON object (%v)", err))
		return nil
	}

	// Look for a recognizable messages array. The exact key is unknown until the
	// sample lands, so probe the plausible candidates; any array-valued match
	// counts as "recognized shape" and suppresses the drift guard.
	if !desktopHasRecognizableMessages(top) {
		p.fireDriftGuard("no recognizable messages array in top-level object")
		return nil
	}

	// TODO(desktop-schema): once a redacted sample local_*.json is available, parse
	// its message array and emit the same event.Event kinds the JSONL processor
	// does (prompt, ai_response, command via normalizePostToolUseByTool, etc.),
	// using stableEventID(sessionID, kind, sourceKey) with a stable per-message
	// sourceKey (each message's own id, or its index if the store gives none) and
	// advancing p.cursor so re-reads only emit messages[cursor:]. Keep
	// Source="claude-code". Until then, emit nothing.
	return nil
}

// desktopHasRecognizableMessages reports whether the decoded top-level object
// carries anything that looks like a message array under the plausible keys.
// Intentionally permissive — its only job is to tell "a Claude desktop session
// blob we simply can't parse yet" (recognized shape, no drift warning) apart
// from "not a session blob at all / schema changed" (fire the guard). The real
// key gets pinned when the sample lands.
func desktopHasRecognizableMessages(top map[string]interface{}) bool {
	for _, key := range []string{"messages", "events", "history", "turns", "conversation"} {
		if v, ok := top[key]; ok {
			if _, isArray := v.([]interface{}); isArray {
				return true
			}
		}
	}
	return false
}

// fireDriftGuard emits a single loud warning that a local_*.json blob did not
// match any recognized shape, so a silent schema change under us surfaces in the
// daemon log instead of just producing zero events forever. Warns at most once
// per processor (per session file) to avoid flooding the log on every 3s
// re-read.
func (p *DesktopSessionProcessor) fireDriftGuard(reason string) {
	if p.warned {
		return
	}
	p.warned = true
	fmt.Fprintf(os.Stderr,
		"desktop-watcher: WARN unrecognized local_*.json schema (session %s): %s — emitting no events; a redacted sample is needed to update the parser\n",
		p.sessionID, reason)
}
