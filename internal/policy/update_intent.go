package policy

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// The org's self-update intent is mirrored to its own file, separate from
// teams-policy.json, because the two have incompatible durability requirements.
//
// teams-policy.json is a CACHE: TTL'd, refetchable, and discarded wholesale by
// readDiskCache on any read error, any JSON error, or a zero FetchedAt. That is
// correct for capture flags, which fail closed — losing the cache costs a little
// capture until the next refresh, and the conservative direction is the safe one.
//
// It is the wrong shape for the update switch, whose safe direction is reversed.
// autoUpdate defaults to TRUE, so discarding the cache does not degrade to a
// conservative value — it re-enables self-update on a fleet an org had
// deliberately set to false. One corrupt or deleted file was enough to put
// unreviewed builds back on a company's laptops, silently, which is precisely
// what a security team sets that switch to prevent.
//
// So the intent gets a file that is written only when an org states one, is
// never aged out, and is never discarded for being stale. It is a mirror, not a
// second source of truth: a successful fetch always overwrites it, and it only
// decides anything when the cache cannot.
const updateIntentFileName = "update-intent.json"

// updateIntent is the durable copy of the org's stated update intent. AutoUpdate
// is a pointer for the same unknown-vs-false reason as the API and cache shapes:
// nil means "the org has not said", which is NOT the same as "the org said no"
// and must not collapse into it.
type updateIntent struct {
	AutoUpdate       *bool  `json:"autoUpdate"`
	PinnedCliVersion string `json:"pinnedCliVersion"`
}

func updateIntentPath() string {
	return filepath.Join(state.StateDir(), updateIntentFileName)
}

// readUpdateIntent returns the mirrored intent, if one was ever written. ok is
// false when no org has stated an intent on this machine, which is the ordinary
// case for a solo install and must not be confused with an org saying no.
func readUpdateIntent() (updateIntent, bool) {
	data, err := os.ReadFile(updateIntentPath())
	if err != nil {
		return updateIntent{}, false
	}
	var m updateIntent
	if err := json.Unmarshal(data, &m); err != nil {
		return updateIntent{}, false
	}
	if m.AutoUpdate == nil && m.PinnedCliVersion == "" {
		return updateIntent{}, false
	}
	return m, true
}

// writeUpdateIntent mirrors a stated intent. A response carrying NEITHER field
// is not written and — importantly — does not erase an existing mirror: a
// backend that stops sending the fields (a rollback, a partial outage, an older
// deployment) has not withdrawn the org's decision, and treating it as though it
// had would re-enable self-update on a fleet configured off. Withdrawal must be
// explicit, which for autoUpdate means sending true.
//
// The write is best-effort and atomic-by-rename, matching writeDiskCache: a
// unique temp file in the same directory, so concurrent watchers cannot clobber
// each other's write or trip Windows' rename-onto-existing failure.
func writeUpdateIntent(autoUpdate *bool, pin string) {
	if autoUpdate == nil && pin == "" {
		return
	}
	data, err := json.Marshal(updateIntent{AutoUpdate: autoUpdate, PinnedCliVersion: pin})
	if err != nil {
		return
	}
	dir := state.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "update-intent-*.json.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Rename(name, updateIntentPath()); err != nil {
		_ = os.Remove(name)
	}
}
