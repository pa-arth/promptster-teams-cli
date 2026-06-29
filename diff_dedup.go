package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
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

func diffDedupLedgerPath() string { return filepath.Join(stateDir(), "diff-hashes.json") }

// aiPathsLedgerPath stores the set of workspace-relative paths an AI channel
// has edited this session. Unlike the dedup ledger it has NO TTL: the git
// watcher consults it long after the 5-minute dedup window to attribute a
// later manual edit to the same file as ai_revised_by_human rather than
// likely_human. Keyed by sessionId so a reused workspace doesn't inherit a
// previous session's AI paths.
func aiPathsLedgerPath() string { return filepath.Join(stateDir(), "ai-paths.json") }

type aiPathsLedger struct {
	SessionID string          `json:"sessionId"`
	Paths     map[string]bool `json:"paths"`
}

type diffDedupEntry struct {
	Hash string `json:"hash"`
	TsMs int64  `json:"tsMs"`
}

// recordAiTouchedPath adds relPath to the session's AI-paths ledger.
// Best-effort: ledger I/O failures must never block event emission.
func recordAiTouchedPath(sessionID, relPath string) {
	if sessionID == "" || relPath == "" {
		return
	}
	_ = withBufferLock(aiPathsLedgerPath()+".lock", func() error {
		ledger := aiPathsLedger{SessionID: sessionID, Paths: map[string]bool{}}
		if data, err := os.ReadFile(aiPathsLedgerPath()); err == nil {
			var onDisk aiPathsLedger
			if json.Unmarshal(data, &onDisk) == nil && onDisk.SessionID == sessionID && onDisk.Paths != nil {
				ledger = onDisk
			}
		}
		ledger.Paths[relPath] = true
		data, err := json.Marshal(ledger)
		if err != nil {
			return err
		}
		tmp := aiPathsLedgerPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, aiPathsLedgerPath())
	})
}

// wasAiTouchedPath reports whether an AI channel edited relPath earlier in
// this session.
func wasAiTouchedPath(sessionID, relPath string) bool {
	if sessionID == "" || relPath == "" {
		return false
	}
	data, err := os.ReadFile(aiPathsLedgerPath())
	if err != nil {
		return false
	}
	var ledger aiPathsLedger
	if json.Unmarshal(data, &ledger) != nil || ledger.SessionID != sessionID {
		return false
	}
	return ledger.Paths[relPath]
}

// fileContentSHA returns a hex sha256 of the file's current contents.
func fileContentSHA(absPath string) (string, bool) {
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
	_ = withBufferLock(diffDedupLedgerPath()+".lock", func() error {
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
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
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
func dedupeFileDiff(taskRoot string, e *Event) bool {
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
		recordAiTouchedPath(e.SessionID, rel)
	}
	return won
}
