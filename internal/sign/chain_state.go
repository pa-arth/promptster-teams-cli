package sign

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// chainState is a DERIVED INDEX of buffer.jsonl: the tip (last signature) of
// each session's hash chain, so a new event can link to the previous event of
// its OWN session rather than to whatever happened to be appended last.
//
// It is authoritative only as a cache. It can always be rebuilt from the
// ledger (rebuildChainStateFromBuffer), so losing or corrupting it costs a
// chain segment boundary, never an event. That property is what makes the
// upgrade path and the corruption-recovery path the same code path.
//
// LOCKING: every function in this file is lock-naked. Callers MUST already
// hold WithBufferLock(state.HookBufferPath()). Do not take a lock in here —
// flock is per open-file-description, so a nested WithBufferLock on the same
// path deadlocks against its own caller inside a single process.
type chainEntry struct {
	LastSig string `json:"lastSig"`
	TsMs    int64  `json:"tsMs"`
}

type chainState struct {
	V        int                   `json:"v"`
	Sessions map[string]chainEntry `json:"sessions"`
}

const (
	chainStateVersion = 1
	// chainStateTTL bounds the index to sessions seen in the last month. The
	// worst it can cost is a resumed session (`claude --resume` on a month-old
	// transcript) starting a fresh segment with prevSig="" — cosmetic, never
	// data loss.
	chainStateTTL = 30 * 24 * time.Hour
	// chainStateMaxEntries is a safety net for a device spawning sessions in a
	// loop between TTL sweeps. At the cap the file is ~100KB, still far cheaper
	// to parse than the full-ledger scan this index replaces.
	chainStateMaxEntries = 512
	// chainRebuildMaxBytes stops a pathological ledger from stalling an append
	// for seconds. Past this we start empty rather than block capture.
	chainRebuildMaxBytes = 64 << 20
)

// readChainState loads the index. ok=false means missing OR corrupt — both
// callers should treat identically, since "rebuild from the ledger" is the
// correct repair for either.
func readChainState(path string) (chainState, bool) {
	// #nosec G304 -- path is state.ChainStatePath(), derived from HookBufferPath, not user input.
	data, err := os.ReadFile(path)
	if err != nil {
		return chainState{}, false
	}
	var cs chainState
	if err := json.Unmarshal(data, &cs); err != nil {
		return chainState{}, false
	}
	if cs.V != chainStateVersion || cs.Sessions == nil {
		return chainState{}, false
	}
	return cs, true
}

// rebuildChainStateFromBuffer scans the ledger once and keeps the last
// well-formed signature per sessionId.
//
// On a pre-upgrade buffer this degenerates to exactly the old
// readLastChainSig result: every legacy event carries sessionId == DeviceID(),
// so grouping by session yields one group in file order and the device-wide
// chain continues unbroken. The new rule is a strict generalization of the old
// one, which is why there is no migration discontinuity to reason about.
func rebuildChainStateFromBuffer(bufferPath string) chainState {
	cs := chainState{V: chainStateVersion, Sessions: map[string]chainEntry{}}

	fi, err := os.Stat(bufferPath)
	if err != nil {
		return cs // no ledger yet == empty chain, not an error
	}
	if fi.Size() > chainRebuildMaxBytes {
		state.HookDebugf("chain state: ledger is %d bytes, over the %d rebuild cap — starting empty", fi.Size(), chainRebuildMaxBytes)
		return cs
	}
	// #nosec G304 -- bufferPath is state.HookBufferPath(), derived from state.StateDir(), not user input.
	f, err := os.Open(bufferPath)
	if err != nil {
		return cs
	}
	defer f.Close()

	nowMs := time.Now().UnixMilli()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var parsed struct {
			SessionID string `json:"sessionId"`
			Sig       string `json:"sig"`
			Ts        string `json:"ts"`
		}
		// A torn or unparseable line is skipped, never fatal: the tip we want is
		// the last WELL-FORMED event of each session. The old readLastChainSig
		// returned an error here, which the caller swallowed into prevSig="" —
		// silently forking the chain on a single truncated write.
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}
		if parsed.SessionID == "" || parsed.Sig == "" {
			continue
		}
		cs.Sessions[parsed.SessionID] = chainEntry{
			LastSig: parsed.Sig,
			TsMs:    chainEntryTsMs(parsed.Ts, nowMs),
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		state.HookDebugf("chain state: rebuild scan: %v", err)
	}
	return cs
}

// chainEntryTsMs prefers the event's own timestamp so a rebuilt index prunes by
// real recency rather than by rebuild time. Falls back to now, which errs
// toward retention.
func chainEntryTsMs(ts string, fallbackMs int64) int64 {
	if ts == "" {
		return fallbackMs
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return fallbackMs
	}
	return parsed.UnixMilli()
}

// loadOrRebuildChainState is the single entry point for the append path.
func loadOrRebuildChainState(bufferPath, statePath string) chainState {
	if cs, ok := readChainState(statePath); ok {
		return cs
	}
	return rebuildChainStateFromBuffer(bufferPath)
}

// prevSigFor returns the tip of sessionID's chain, or "" to start a new
// segment. "" is a normal, expected value — it means "first event of this
// session", not "tampering".
func (cs chainState) prevSigFor(sessionID string) string {
	if cs.Sessions == nil {
		return ""
	}
	return cs.Sessions[sessionID].LastSig
}

func (cs *chainState) setTip(sessionID, sig string, tsMs int64) {
	if cs.Sessions == nil {
		cs.Sessions = map[string]chainEntry{}
	}
	cs.V = chainStateVersion
	cs.Sessions[sessionID] = chainEntry{LastSig: sig, TsMs: tsMs}
}

// prune bounds the index by TTL, then by entry count (oldest first).
//
// It MUST run after setTip. setTip stamps the emitting session with a fresh
// tsMs, so an active session can never evict itself no matter how long it
// lives — a session's TTL clock only starts once it goes quiet. That ordering
// is the entire reason long-lived and resumed sessions are safe here.
func (cs *chainState) prune(nowMs int64) {
	ttlMs := chainStateTTL.Milliseconds()
	for sid, e := range cs.Sessions {
		if nowMs-e.TsMs > ttlMs {
			delete(cs.Sessions, sid)
		}
	}
	if len(cs.Sessions) <= chainStateMaxEntries {
		return
	}
	type aged struct {
		sid  string
		tsMs int64
	}
	all := make([]aged, 0, len(cs.Sessions))
	for sid, e := range cs.Sessions {
		all = append(all, aged{sid: sid, tsMs: e.TsMs})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].tsMs != all[j].tsMs {
			return all[i].tsMs < all[j].tsMs
		}
		return all[i].sid < all[j].sid // deterministic on tie
	})
	for _, e := range all[:len(all)-chainStateMaxEntries] {
		delete(cs.Sessions, e.sid)
	}
}

// writeChainState commits the index via temp+rename so a crash mid-write can
// never leave a half-parsed file (and a half-parsed file would only cost a
// rebuild anyway).
func writeChainState(path string, cs chainState) error {
	cs.V = chainStateVersion
	data, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
