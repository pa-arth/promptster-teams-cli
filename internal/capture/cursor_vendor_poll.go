package capture

import (
	"encoding/json"
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

// loadCursorVendorPollOkAt returns the zero time when no poll has ever
// succeeded on this machine (or the file is unreadable).
func loadCursorVendorPollOkAt() time.Time {
	var c cursorVendorPoll
	data, err := os.ReadFile(cursorVendorPollPath()) // #nosec G304 -- state dir path.
	if err != nil {
		return time.Time{}
	}
	_ = json.Unmarshal(data, &c)
	return c.LastOkAt
}

// recordCursorVendorPollOk stamps a successful poll, best-effort: a lost stamp
// only makes the collector look older than it is until the next poll.
func recordCursorVendorPollOk(at time.Time) {
	data, err := json.Marshal(cursorVendorPoll{V: cursorVendorPollVersion, LastOkAt: at.UTC()})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cursorVendorPollPath()), 0o700); err != nil {
		return
	}
	tmp := cursorVendorPollPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, cursorVendorPollPath())
}
