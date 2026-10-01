package capture

import (
	"encoding/json"
	"errors"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
	"os"
	"path/filepath"
	"sort"
)

// Independent idempotence keeps working-HEAD and default-branch observations
// from duplicating receipts. Record only after durable queueing. Transient or
// partial results can improve on explicit backfill; the backend prefers complete
// receipts over later incomplete ones.
func queueLineOrigin(session Session, root, sha string, nowMs int64) bool {
	path := filepath.Join(state.StateDir(), "line-origin-commits-v1.json")
	key := workspaceKey(root) + ":" + sha
	err := sign.WithBufferLock(path+".lock", func() error {
		seen := map[string]int64{}
		if data, err := os.ReadFile(path); err == nil { // #nosec G304 -- fixed ledger filename under the CLI's own state directory; repo paths and event input never contribute to this path. Bytes are parsed locally as SHA/timestamp metadata, never emitted.
			_ = json.Unmarshal(data, &seen)
		}
		if seen == nil {
			seen = map[string]int64{}
		}
		if ts, ok := seen[key]; ok && nowMs-ts < discoveredRepoTTLMs {
			return nil
		}
		receipt, err := AnalyzeLineOrigin(root, sha)
		if err != nil {
			return err
		}
		if !emitCommitAttribution(lineOriginEvent(session, receipt)) {
			return errors.New("origin queue failed")
		}
		seen[key] = nowMs
		for k, ts := range seen {
			if nowMs-ts > discoveredRepoTTLMs {
				delete(seen, k)
			}
		}
		if len(seen) > 20000 {
			keys := make([]string, 0, len(seen))
			for k := range seen {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				if seen[keys[i]] != seen[keys[j]] {
					return seen[keys[i]] < seen[keys[j]]
				}
				return keys[i] < keys[j]
			})
			for _, k := range keys[:len(keys)-20000] {
				delete(seen, k)
			}
		}
		data, err := json.Marshal(seen)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
			return err
		}
		return os.Rename(path+".tmp", path)
	})
	if err != nil {
		state.HookDebugf("local line origin unavailable; replay with blame-backfill")
	}
	return err == nil
}
