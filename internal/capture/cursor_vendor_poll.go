package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// When the Cursor vendor usage collector last completed a successful poll,
// persisted so it survives `watch` restarts and read on every heartbeat.
//
// Its own file rather than a field in cursor-generations.json: that file's lock
// is contended by the hook rail, and the collector has no business waiting on
// it. The only writer is the collector inside `watch` (one per user account, see
// watch.lock), so a tmp+rename is enough and no lock is taken.

func cursorVendorPollPath() string {
	return filepath.Join(state.StateDir(), "cursor-vendor-poll.json")
}

type cursorVendorPoll struct {
	V        int       `json:"v"`
	LastOkAt time.Time `json:"lastOkAt"`
}

const cursorVendorPollVersion = 1

// loadCursorVendorPollOkAt distinguishes a never-polled machine from broken
// persistence. Callers report errors rather than silently claiming no poll.
func loadCursorVendorPollOkAt() (time.Time, error) {
	var c cursorVendorPoll
	data, err := os.ReadFile(cursorVendorPollPath()) // #nosec G304 -- state dir path.
	if os.IsNotExist(err) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return time.Time{}, err
	}
	if c.V != cursorVendorPollVersion || c.LastOkAt.IsZero() {
		return time.Time{}, fmt.Errorf("invalid poll stamp version or timestamp")
	}
	return c.LastOkAt, nil
}

// recordCursorVendorPollOk stamps a successful poll. A persistence failure is
// returned to the collector for an explicit diagnostic; it never advances state.
func recordCursorVendorPollOk(at time.Time) error {
	data, err := json.Marshal(cursorVendorPoll{V: cursorVendorPollVersion, LastOkAt: at.UTC()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cursorVendorPollPath()), 0o700); err != nil {
		return err
	}
	tmp := cursorVendorPollPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, cursorVendorPollPath())
}
