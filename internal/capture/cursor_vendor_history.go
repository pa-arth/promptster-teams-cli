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

// The current period endpoint gives exact current bounds. Monthly recurrence
// gives the preceding bounds without inventing a period for a changed plan.
// Dates 29–31 need vendor-provided historical bounds, so we refuse to infer.
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

const cursorVendorHistoryRepairInterval = 6 * time.Hour

// A prior period can overlap the 31-day chart after its reset. Its rows are
// staged under their actual billing-cycle bounds, with the same v1 digest and
// immutable event IDs as a current snapshot. Re-read it to catch corrections,
// but upload only when its content changes. A failed partial queue is retried.
func queueHistoricalCursorVendorSnapshot(
	snapshot cursorVendorSnapshot, deviceID string, capturedAt time.Time,
	cursorVersion string, enqueue func(event.Event) bool,
) error {
	path := cursorVendorHistoryCheckpointPath()
	return sign.WithBufferLock(path+".lock", func() error {
		seen := map[string]cursorVendorHistoryCheckpoint{}
		if raw, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(raw, &seen)
		}
		key := deviceID + "\x1f" + snapshot.AccountRef + "\x1f" + snapshot.CycleStart.UTC().Format(time.RFC3339Nano)
		prior := seen[key]
		if prior.SnapshotID == snapshot.SnapshotID && capturedAt.Sub(prior.LastFullAt) >= 0 && capturedAt.Sub(prior.LastFullAt) < cursorVendorHistoryRepairInterval {
			return nil
		}
		for _, ev := range snapshot.rowEvents(deviceID) {
			if !enqueue(ev) {
				return errors.New("historical vendor row queue failed")
			}
		}
		if !enqueue(snapshot.completionEvent(deviceID, capturedAt, cursorVersion)) {
			return errors.New("historical vendor completion queue failed")
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
			_ = tmp.Close()
			return err
		}
		if _, err := tmp.Write(raw); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), path)
	})
}
