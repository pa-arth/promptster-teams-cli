package capture

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const cursorVendorRepairInterval = 6 * time.Hour

type cursorVendorSeenPool struct {
	Hashes                map[string]bool      `json:"hashes"`
	LastSnapshotByAccount map[string]string    `json:"lastSnapshotByAccount"`
	LastManifestByAccount map[string]string    `json:"lastManifestByAccount"`
	LastRepairByAccount   map[string]time.Time `json:"lastRepairByAccount"`
	LastSeenAt            time.Time            `json:"lastSeenAt"`
}
type cursorVendorSeenState struct {
	Pools map[string]cursorVendorSeenPool `json:"pools"`
}

func cursorVendorSeenPath() string {
	return filepath.Join(state.StateDir(), "cursor-vendor-seen-v2.json")
}
func loadCursorVendorSeen() cursorVendorSeenState {
	b, err := os.ReadFile(cursorVendorSeenPath())
	var found cursorVendorSeenState
	if err != nil || json.Unmarshal(b, &found) != nil || found.Pools == nil {
		return cursorVendorSeenState{Pools: map[string]cursorVendorSeenPool{}}
	}
	return found
}
func saveCursorVendorSeen(found cursorVendorSeenState) error {
	path := cursorVendorSeenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(found)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cursor-vendor-seen-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// queueCursorVendorSnapshotV2 stores each distinct row once per device/cycle.
// The completion carries an ordered manifest of 128-bit SHA-256 prefixes. A
// correction changes that manifest, not every stored row; a repeated identical
// row appears twice in the manifest but only once in the row pool. The full
// content digest still validates the reconstructed snapshot before publication.
func queueCursorVendorSnapshotV2(snapshot cursorVendorSnapshot, deviceID string,
	capturedAt time.Time, cursorVersion string, enqueue func(event.Event) bool) bool {
	poolSum := sha256.Sum256([]byte("cursor-vendor-pool/v2\x00" + deviceID + "\x00" +
		snapshot.CycleStart.UTC().Format(cursorVendorCycleTimeFormat) + "\x00" +
		snapshot.CycleEnd.UTC().Format(cursorVendorCycleTimeFormat)))
	poolID := hex.EncodeToString(poolSum[:])
	stored := loadCursorVendorSeen()
	pool := stored.Pools[poolID]
	if pool.Hashes == nil {
		pool.Hashes = map[string]bool{}
	}
	if pool.LastSnapshotByAccount == nil {
		pool.LastSnapshotByAccount = map[string]string{}
	}
	if pool.LastManifestByAccount == nil {
		pool.LastManifestByAccount = map[string]string{}
	}
	if pool.LastRepairByAccount == nil {
		pool.LastRepairByAccount = map[string]time.Time{}
	}
	lastRepair := pool.LastRepairByAccount[snapshot.AccountRef]
	repairAge := capturedAt.Sub(lastRepair)
	// A new account can reuse already stored pool rows. It still gets its own
	// repair clock; genuinely unseen rows are queued regardless of this flag.
	repair := (lastRepair.IsZero() && len(pool.Hashes) == 0) ||
		(!lastRepair.IsZero() && (repairAge < 0 || repairAge >= cursorVendorRepairInterval))
	if lastRepair.IsZero() || repair {
		pool.LastRepairByAccount[snapshot.AccountRef] = capturedAt.UTC()
	}

	events := snapshot.rowEvents(deviceID)
	manifest := make([]byte, 0, 16*len(events))
	queuedAll := true
	attempted := map[string]bool{}
	for _, ev := range events {
		data := ev.Data.(map[string]interface{})
		content := make(map[string]interface{}, len(data))
		for key, value := range data {
			if key != "snapshotId" && key != "ordinal" && key != "accountRef" {
				content[key] = value
			}
		}
		// Hash exactly the emitted projected fields. Adding an un-emitted
		// vendor struct field must not mint a new pool row on CLI upgrade.
		raw, err := json.Marshal(content)
		if err != nil {
			return false
		}
		rowSum := sha256.Sum256(raw)
		rowHash := hex.EncodeToString(rowSum[:])
		manifest = append(manifest, rowSum[:16]...)
		if attempted[rowHash] {
			continue
		}
		attempted[rowHash] = true
		if !repair && pool.Hashes[rowHash] {
			continue
		}
		data["snapshotId"] = poolID
		data["ordinal"] = 0 // v2 identity is rowHash, not an ordinal
		data["rowHash"] = rowHash
		delete(data, "accountRef") // attribution is on the completion
		ev.ID = event.DeterministicUUID("cursorVendorUsage/v2:" + poolID + ":" + rowHash)
		if !enqueue(ev) {
			queuedAll = false
		}
	}
	completion := snapshot.completionEvent(deviceID, capturedAt, cursorVersion)
	data := completion.Data.(map[string]interface{})
	data["protocolVersion"] = 2
	data["poolId"] = poolID
	manifestSum := sha256.Sum256(manifest)
	manifestHash := hex.EncodeToString(manifestSum[:])
	data["manifestSha256"] = manifestHash
	// A changed snapshot needs a manifest. An unchanged poll can reference the
	// earlier one by snapshotId; the periodic repair makes that reference safe
	// after transient storage loss.
	if repair || pool.LastSnapshotByAccount[snapshot.AccountRef] != snapshot.SnapshotID ||
		pool.LastManifestByAccount[snapshot.AccountRef] != manifestHash {
		data["manifest"] = base64.StdEncoding.EncodeToString(manifest)
	}
	if !enqueue(completion) {
		queuedAll = false
	}
	if queuedAll {
		pool.LastSeenAt = capturedAt.UTC()
		pool.LastSnapshotByAccount[snapshot.AccountRef] = snapshot.SnapshotID
		pool.LastManifestByAccount[snapshot.AccountRef] = manifestHash
		for hash := range attempted {
			pool.Hashes[hash] = true
		}
		stored.Pools[poolID] = pool
		for id, old := range stored.Pools {
			if id != poolID && capturedAt.Sub(old.LastSeenAt) > 40*24*time.Hour {
				delete(stored.Pools, id)
			}
		}
		if err := saveCursorVendorSeen(stored); err != nil {
			fmt.Fprintf(os.Stderr, "cursor-vendor: cannot save queued row pool: %v\n", err)
		}
	}
	return queuedAll
}
