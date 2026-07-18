package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// File-diff idempotency across capture channels.
//
// The same file edit can be observed by more than one channel: an AI tool's
// own instrumentation (Claude Code hooks, the codex rollout watcher) reports it
// immediately with `likely_ai` attribution, and the git watcher — which polls
// the whole working tree on an interval — sees the identical change later and
// would re-emit it as an unattributed (human-looking) diff. That double-counts
// edits and mis-attributes AI work to the candidate.
//
// To make emission idempotent we keep a small shared ledger keyed by
// (path, resulting-content-hash). Every channel calls dedupeFileDiff before
// emitting a file_diff; the first caller to claim a given (path, hash) emits,
// and any later caller observing the SAME resulting content skips. Because the
// AI channels fire within seconds while the git watcher polls every ~60s, the
// AI-attributed event normally wins and the git watcher only emits genuine
// human edits (including edits that land on top of an AI edit — those produce a
// different content hash and are therefore NOT deduped).

const diffDedupTTL = 5 * time.Minute

func diffDedupLedgerPath() string { return filepath.Join(state.StateDir(), "diff-hashes.json") }

// aiPathsLedgerPath stores the set of workspace-relative paths an AI channel
// has edited this session. Unlike the dedup ledger it has NO TTL: the git
// watcher consults it long after the 5-minute dedup window to attribute a
// later manual edit to the same file as ai_revised_by_human rather than
// likely_human. Keyed by sessionId so a reused workspace doesn't inherit a
// previous session's AI paths.
func aiPathsLedgerPath() string { return filepath.Join(state.StateDir(), "ai-paths.json") }

// aiPathsLedger is keyed BY session rather than holding one session at a time.
//
// It used to store a single sessionId and wipe itself whenever a different one
// showed up. That was survivable only because every event carried the same
// device-wide id, so the key never changed. Now that ids are per-session,
// concurrent Claude and Codex sessions would take turns invalidating each
// other's paths on every write — last writer wins, both lose.
type aiPathsLedger struct {
	V        int                     `json:"v"`
	Sessions map[string]aiPathsEntry `json:"sessions"`
}

type aiPathsEntry struct {
	Paths   map[string]bool `json:"paths"`
	TsMs    int64           `json:"tsMs"`
	RootKey string          `json:"rootKey"`
}

const (
	aiPathsLedgerVersion = 3
	// aiPathsTTL bounds the ledger to recently-active sessions. It has no TTL
	// per-entry semantics beyond eviction: the git watcher reads it long after
	// the 5-minute dedup window, so this must outlive a working session by a lot.
	aiPathsTTL = 7 * 24 * time.Hour
	// aiPathsMaxSessions is a backstop for a device that opens sessions in a loop.
	aiPathsMaxSessions = 64
)

type diffDedupEntry struct {
	Hash string `json:"hash"`
	TsMs int64  `json:"tsMs"`
}

// recordAiTouchedPath adds relPath to the session's AI-paths ledger, tagging
// the entry with the workspace rootKey so a reader can scope reads to one
// workspace and a same-named path in another repo can't bleed in.
// Best-effort: ledger I/O failures must never block event emission.
//
// The single rootKey per session entry is sound because rootKey is an invariant
// of the session, not the edit: every caller derives it from the immutable
// session.TaskRoot (see dedupeFileDiff), so a given sessionID is only ever
// recorded under one rootKey. Restamping RootKey below therefore never retags a
// prior root's paths — it re-writes the same value.
func recordAiTouchedPath(sessionID, rootKey, relPath string) {
	if sessionID == "" || relPath == "" {
		return
	}
	_ = sign.WithBufferLock(aiPathsLedgerPath()+".lock", func() error {
		ledger := aiPathsLedger{V: aiPathsLedgerVersion, Sessions: map[string]aiPathsEntry{}}
		if data, err := os.ReadFile(aiPathsLedgerPath()); err == nil {
			var onDisk aiPathsLedger
			// A v1 ledger (single sessionId + paths) unmarshals with no Sessions
			// map; drop it rather than migrate. It is a heuristic attribution
			// cache, not a record — and every v1 entry is keyed by the device id
			// this change stops using anyway.
			if json.Unmarshal(data, &onDisk) == nil && onDisk.V == aiPathsLedgerVersion && onDisk.Sessions != nil {
				ledger = onDisk
			}
		}

		nowMs := time.Now().UnixMilli()
		entry, ok := ledger.Sessions[sessionID]
		if !ok || entry.Paths == nil {
			entry = aiPathsEntry{Paths: map[string]bool{}}
		}
		entry.Paths[relPath] = true
		entry.TsMs = nowMs
		entry.RootKey = rootKey
		ledger.Sessions[sessionID] = entry
		pruneAiPaths(&ledger, nowMs)

		data, err := json.Marshal(ledger)
		if err != nil {
			return err
		}
		tmp := aiPathsLedgerPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, aiPathsLedgerPath())
	})
}

// readAiTouchedPaths returns every workspace-relative path an AI channel has
// recorded, mapped to the sessionId that touched it, across all sessions still
// within aiPathsTTL. When more than one session touched the same path the most
// recently active session wins. Mirrors the writer's lock + version/TTL
// discipline and is read-only (it never rewrites the ledger, so a pure reader
// like the git watcher can't evict a live session).
//
// Shape rationale: a relPath -> sessionId map is exactly what the later
// attribution consumer needs — given a path that changed in a new commit, it
// asks "was this AI-touched, and by which session" in one lookup, with no
// sessionId known in advance.
// rootKey scopes the read to one workspace: an entry recorded under a DIFFERENT
// known rootKey is skipped, so a same-named path in another repo cannot bleed
// in. The scoping is conservative — an entry is excluded only when both its
// rootKey and the requested rootKey are known AND differ; an unknown on either
// side (empty rootKey) falls through as a match.
func readAiTouchedPaths(rootKey string) map[string]string {
	out := map[string]string{}
	_ = sign.WithBufferLock(aiPathsLedgerPath()+".lock", func() error {
		data, err := os.ReadFile(aiPathsLedgerPath())
		if err != nil {
			return nil
		}
		var ledger aiPathsLedger
		if json.Unmarshal(data, &ledger) != nil || ledger.V != aiPathsLedgerVersion || ledger.Sessions == nil {
			return nil
		}
		nowMs := time.Now().UnixMilli()
		ttlMs := aiPathsTTL.Milliseconds()
		bestTs := map[string]int64{}
		for sid, entry := range ledger.Sessions {
			if nowMs-entry.TsMs > ttlMs {
				continue
			}
			if entry.RootKey != "" && rootKey != "" && entry.RootKey != rootKey {
				continue
			}
			for p := range entry.Paths {
				// Newer session wins; on an exact-ts tie, the lexicographically
				// smaller sessionId wins so repeated reads are deterministic.
				if ts, ok := bestTs[p]; !ok || entry.TsMs > ts || (entry.TsMs == ts && sid < out[p]) {
					out[p] = sid
					bestTs[p] = entry.TsMs
				}
			}
		}
		return nil
	})
	return out
}

// pruneAiPaths bounds the ledger by TTL, then by session count (oldest first).
// Runs AFTER the active session's entry is stamped, so an actively-editing
// session can never evict itself.
func pruneAiPaths(ledger *aiPathsLedger, nowMs int64) {
	ttlMs := aiPathsTTL.Milliseconds()
	for sid, e := range ledger.Sessions {
		if nowMs-e.TsMs > ttlMs {
			delete(ledger.Sessions, sid)
		}
	}
	for len(ledger.Sessions) > aiPathsMaxSessions {
		oldest, oldestTs := "", int64(0)
		for sid, e := range ledger.Sessions {
			if oldest == "" || e.TsMs < oldestTs || (e.TsMs == oldestTs && sid < oldest) {
				oldest, oldestTs = sid, e.TsMs
			}
		}
		delete(ledger.Sessions, oldest)
	}
}

// --- AI bash-command execution windows ledger --------------------------------
//
// PR5 (bash-attribution recovery): when an AI edits files via a Bash command
// (`sed -i`, `>>`, a codegen/formatter) instead of Edit/Write, NO file_diff is
// produced, so those paths never enter the ai-paths ledger and would commit as
// unknown — an AI edit undercounted. Since this codebase has no daemon/checkpoint
// and no pre/post stat diff, the only signal for "which files did an AI bash
// command touch" is TIMESTAMP CORRELATION: a file changed in a commit whose
// on-disk mtime falls within (± a small tolerance of) an AI bash command's
// execution window was likely written by that command. This mirrors git-ai's ±3s
// mtime solver (docs/bash-attribution-recovery-plan.md).
//
// TIMESTAMP MODEL (verified against the normalizers, not assumed): the `command`
// event is built from the tool_result / function_call_output transcript line —
// Claude's resolveToolResult and Codex's emitToolEvent both stamp the event's Ts
// with that OUTPUT line's timestamp. The tool_use / function_call START line's
// timestamp is NOT retained in the pending-call structs (claudePendingTool /
// codexPendingCall carry name+input only). So only the command END time is
// reliably available at the point a window is recorded. We therefore store the
// window as the observed END point [end, end] and apply the δ/ε tolerance at
// RECOVERY time (see commit_attribution.go) — modelling [end - δ, end + ε].
//
// This ledger is modelled EXACTLY on the ai-paths ledger above: session-keyed,
// same WithBufferLock discipline, the same 7-day aiPathsTTL prune, same
// max-sessions backstop. Windows carry only integers.

func bashWindowsLedgerPath() string { return filepath.Join(state.StateDir(), "bash-windows.json") }

// bashWindowsLedger is keyed by session, exactly like aiPathsLedger, so
// concurrent Claude and Codex sessions never invalidate each other's windows.
type bashWindowsLedger struct {
	V        int                         `json:"v"`
	Sessions map[string]bashWindowsEntry `json:"sessions"`
}

type bashWindowsEntry struct {
	Windows []bashWindowSpan `json:"windows"`
	TsMs    int64            `json:"tsMs"`
}

// bashWindowSpan is one AI bash command's execution window, in Unix ms.
type bashWindowSpan struct {
	StartMs int64 `json:"startMs"`
	EndMs   int64 `json:"endMs"`
}

// bashWindow is a span joined with the session that recorded it — the shape the
// recovery consumer reads (it needs the session to tag a recovered file).
type bashWindow struct {
	SessionID string
	StartMs   int64
	EndMs     int64
}

const (
	bashWindowsLedgerVersion = 1
	// bashWindowsMaxPerSession bounds a session that runs a very large number of
	// bash commands. Oldest windows are dropped first — recovery cares about
	// recent activity, and a commit's files were written recently.
	bashWindowsMaxPerSession = 1024
)

// recordBashWindow appends one AI bash execution window to the session's ledger.
// Best-effort: ledger I/O failures must never block event emission. Mirrors
// recordAiTouchedPath's lock + version/TTL/prune discipline.
func recordBashWindow(sessionID string, startMs, endMs int64) {
	if sessionID == "" {
		return
	}
	if endMs < startMs {
		startMs, endMs = endMs, startMs
	}
	_ = sign.WithBufferLock(bashWindowsLedgerPath()+".lock", func() error {
		ledger := bashWindowsLedger{V: bashWindowsLedgerVersion, Sessions: map[string]bashWindowsEntry{}}
		if data, err := os.ReadFile(bashWindowsLedgerPath()); err == nil {
			var onDisk bashWindowsLedger
			if json.Unmarshal(data, &onDisk) == nil && onDisk.V == bashWindowsLedgerVersion && onDisk.Sessions != nil {
				ledger = onDisk
			}
		}

		nowMs := time.Now().UnixMilli()
		entry := ledger.Sessions[sessionID]
		entry.Windows = append(entry.Windows, bashWindowSpan{StartMs: startMs, EndMs: endMs})
		if len(entry.Windows) > bashWindowsMaxPerSession {
			entry.Windows = entry.Windows[len(entry.Windows)-bashWindowsMaxPerSession:]
		}
		entry.TsMs = nowMs
		ledger.Sessions[sessionID] = entry
		pruneBashWindows(&ledger, nowMs)

		data, err := json.Marshal(ledger)
		if err != nil {
			return err
		}
		tmp := bashWindowsLedgerPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, bashWindowsLedgerPath())
	})
}

// readBashWindows returns every AI bash window across all sessions still within
// aiPathsTTL, each tagged with its recording session. Read-only (never rewrites
// the ledger), so a pure reader like the git watcher can't evict a live session
// — same discipline as readAiTouchedPaths.
func readBashWindows() []bashWindow {
	var out []bashWindow
	_ = sign.WithBufferLock(bashWindowsLedgerPath()+".lock", func() error {
		data, err := os.ReadFile(bashWindowsLedgerPath())
		if err != nil {
			return nil
		}
		var ledger bashWindowsLedger
		if json.Unmarshal(data, &ledger) != nil || ledger.V != bashWindowsLedgerVersion || ledger.Sessions == nil {
			return nil
		}
		nowMs := time.Now().UnixMilli()
		ttlMs := aiPathsTTL.Milliseconds()
		for sid, entry := range ledger.Sessions {
			if nowMs-entry.TsMs > ttlMs {
				continue
			}
			for _, w := range entry.Windows {
				out = append(out, bashWindow{SessionID: sid, StartMs: w.StartMs, EndMs: w.EndMs})
			}
		}
		return nil
	})
	return out
}

// pruneBashWindows bounds the ledger by TTL, then by session count (oldest
// first). Runs AFTER the active session's entry is stamped, so an actively-active
// session can never evict itself. Mirrors pruneAiPaths.
func pruneBashWindows(ledger *bashWindowsLedger, nowMs int64) {
	ttlMs := aiPathsTTL.Milliseconds()
	for sid, e := range ledger.Sessions {
		if nowMs-e.TsMs > ttlMs {
			delete(ledger.Sessions, sid)
		}
	}
	for len(ledger.Sessions) > aiPathsMaxSessions {
		oldest, oldestTs := "", int64(0)
		for sid, e := range ledger.Sessions {
			if oldest == "" || e.TsMs < oldestTs || (e.TsMs == oldestTs && sid < oldest) {
				oldest, oldestTs = sid, e.TsMs
			}
		}
		delete(ledger.Sessions, oldest)
	}
}

// recordAiBashWindow records the execution window of an AI-attributed Bash
// `command` event, so the commit reconciler can later recover unknown files
// whose mtime falls in it. It is the bash-window counterpart to the
// recordAiTouchedPath call in dedupeFileDiff, and is called from the SAME watcher
// queue funnels (Claude + Codex). No-op for anything that is not an AI command:
//   - non-command kinds (file edits already flow through the ai-paths ledger),
//   - human/unknown provenance (ONLY AI bash is recorded — never misattribute).
//
// Only the command END time is observable (see the ledger header), so the stored
// window is the point [endMs, endMs]; recovery widens it by ± the δ/ε tolerance.
func recordAiBashWindow(e *event.Event) {
	if e == nil || e.Kind != "command" {
		return
	}
	if e.Provenance == nil || e.Provenance.Attribution != "likely_ai" {
		return
	}
	endMs, ok := eventTsMs(e.Ts)
	if !ok {
		return
	}
	recordBashWindow(e.SessionID, endMs, endMs)
}

// eventTsMs parses an event's RFC3339Nano Ts into Unix ms.
func eventTsMs(ts string) (int64, bool) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// fileContentSHA returns a hex sha256 of the file's current contents.
func fileContentSHA(absPath string) (string, bool) {
	// #nosec G304 -- absPath is a workspace file the capture session already touched (from a tool diff), hashed for dedup; not user input.
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// claimFileDiff records (path, hash) in the ledger and reports whether THIS
// caller won the claim. Returns true if the (path, hash) was not already
// claimed within the TTL (caller should emit); false if a prior channel already
// emitted this exact resulting content (caller should skip). Stale entries are
// pruned on every call so the ledger stays bounded to active files.
func claimFileDiff(relPath, hash string) bool {
	won := true
	_ = sign.WithBufferLock(diffDedupLedgerPath()+".lock", func() error {
		ledger := map[string]diffDedupEntry{}
		if data, err := os.ReadFile(diffDedupLedgerPath()); err == nil {
			_ = json.Unmarshal(data, &ledger)
		}

		nowMs := time.Now().UnixMilli()
		ttlMs := diffDedupTTL.Milliseconds()

		// Prune stale entries.
		for p, e := range ledger {
			if nowMs-e.TsMs > ttlMs {
				delete(ledger, p)
			}
		}

		if e, ok := ledger[relPath]; ok && e.Hash == hash && nowMs-e.TsMs <= ttlMs {
			won = false
			return nil
		}
		ledger[relPath] = diffDedupEntry{Hash: hash, TsMs: nowMs}

		data, err := json.Marshal(ledger)
		if err != nil {
			return err
		}
		tmp := diffDedupLedgerPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, diffDedupLedgerPath())
	})
	return won
}

// dedupeFileDiff reports whether a file_diff event should be emitted. It is a
// no-op (returns true) for non-file_diff events. For file_diffs it hashes the
// resulting on-disk content (falling back to the diff text when the file is
// gone, e.g. a delete) and claims it in the shared ledger.
func dedupeFileDiff(taskRoot string, e *event.Event) bool {
	if e == nil || e.Kind != "file_diff" {
		return true
	}
	data, ok := e.Data.(map[string]interface{})
	if !ok {
		return true
	}
	rel, _ := data["path"].(string)
	if rel == "" {
		return true
	}
	abs := rel
	if !filepath.IsAbs(rel) && taskRoot != "" {
		abs = filepath.Join(taskRoot, rel)
	}
	hash, ok := fileContentSHA(abs)
	if !ok {
		// File unreadable (deleted/moved) — fall back to the diff text so repeated
		// emissions of the same delete still dedupe within a channel.
		diff, _ := data["diff"].(string)
		hash = hashString("∅:" + diff)
	}
	won := claimFileDiff(rel, hash)
	// Track AI-edited paths for later git-watcher attribution
	// (ai_revised_by_human vs likely_human). Recorded regardless of claim
	// outcome — a deduped re-observation is still an AI edit to this path.
	if e.Provenance != nil && e.Provenance.Attribution == "likely_ai" {
		rootKey := ""
		if taskRoot != "" {
			rootKey = gitWatchRootKey(taskRoot)
		}
		recordAiTouchedPath(e.SessionID, rootKey, rel)
	}
	return won
}
