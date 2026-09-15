package capture

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// previousCursorBillingCycle derives the preceding cycle from the current one.
// The current-period endpoint gives exact current bounds; monthly recurrence
// gives the preceding bounds without inventing a period for a changed plan.
// Start days 29-31 have no unambiguous previous-month equivalent, so refuse.
func previousCursorBillingCycle(start, reset time.Time) (time.Time, time.Time, bool) {
	if start.Day() > 28 || !start.AddDate(0, 1, 0).Equal(reset) {
		return time.Time{}, time.Time{}, false
	}
	return start.AddDate(0, -1, 0), start, true
}

func cursorVendorHistoryCheckpointPath() string {
	return filepath.Join(state.StateDir(), "cursor-vendor-history.json")
}

type cursorVendorHistoryCheckpoint struct {
	SnapshotID string    `json:"snapshotId"`
	LastFullAt time.Time `json:"lastFullAt"`
}

// queueHistoricalCursorVendorSnapshot stages a closed prior cycle through the
// same v2 path as the current one (its pool is keyed by the prior bounds, so
// the two never share rows or manifests). A closed cycle rarely changes, so
// an unchanged snapshotId is not re-sent until cursorVendorRepairInterval has
// passed; a vendor correction changes the snapshotId and is sent at once. A
// failed queue leaves the checkpoint untouched, so the next poll retries.
func queueHistoricalCursorVendorSnapshot(
	snapshot cursorVendorSnapshot, deviceID string, capturedAt time.Time,
	cursorVersion string, enqueue func(event.Event) bool,
) error {
	path := cursorVendorHistoryCheckpointPath()
	return sign.WithBufferLock(path+".lock", func() error {
		seen := map[string]cursorVendorHistoryCheckpoint{}
		// #nosec G304 -- fixed cursor-vendor-history.json under state.StateDir(), never an event field.
		if raw, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(raw, &seen)
		}
		key := deviceID + "\x1f" + snapshot.AccountRef + "\x1f" + snapshot.CycleStart.UTC().Format(time.RFC3339Nano)
		prior := seen[key]
		age := capturedAt.Sub(prior.LastFullAt)
		if prior.SnapshotID == snapshot.SnapshotID && age >= 0 && age < cursorVendorRepairInterval {
			return nil
		}
		if !queueCursorVendorSnapshotV2(snapshot, deviceID, capturedAt, cursorVersion, enqueue) {
			return errors.New("historical vendor snapshot queue failed")
		}
		// Keep the file bounded: only cycles still inside the v2 pool horizon.
		for k, v := range seen {
			if capturedAt.Sub(v.LastFullAt) > 40*24*time.Hour {
				delete(seen, k)
			}
		}
		seen[key] = cursorVendorHistoryCheckpoint{SnapshotID: snapshot.SnapshotID, LastFullAt: capturedAt}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		raw, err := json.Marshal(seen)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".cursor-vendor-history-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if err := tmp.Chmod(0o600); err != nil {
			return errors.Join(err, tmp.Close())
		}
		if _, err := tmp.Write(raw); err != nil {
			return errors.Join(err, tmp.Close())
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), path)
	})
}
