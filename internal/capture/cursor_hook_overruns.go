package capture

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// The wall-clock budget overrun counter.
//
// WHY IT LIVES IN ITS OWN FILE, UNDER ITS OWN LOCK. This is the one counter that
// MUST NOT share a lock with cursor-generations.json, because contention on that
// lock is the leading candidate for what it is counting.
//
// RunCursorHook abandons its worker goroutine at cursorHookBudget and returns.
// The goroutine is not stopped — it is left running, and it may be running
// precisely because it is blocked inside sign.WithBufferLock, which is a
// BLOCKING flock with no timeout (signing_lock_unix.go). Recording the overrun
// through that same lock would therefore park the parent on the very wedge it is
// reporting, past the budget, for as long as the wedge lasts. The budget would
// still be enforced in name and defeated in fact, and the failure mode would be
// exactly the one the budget exists to prevent: a stalled hook stalls the
// engineer's agent.
//
// So: a separate path, a separate lock, and a critical section that reads and
// writes one integer and can block on nothing else.
//
// WHY A COUNTER AT ALL. The overrun path is silent by construction — it prints
// one line to a stderr Cursor discards, and the work it abandoned leaves no
// trace anywhere. A rail that drops work invisibly cannot be told from a rail
// with nothing to do, which is how 38% of this machine's turns came to be
// missing with no evidence of a mechanism. See recordCursorStopOutcome for the
// other half of the same argument.

func cursorHookOverrunsPath() string {
	return filepath.Join(state.StateDir(), "cursor-hook-overruns.json")
}

type cursorHookOverruns struct {
	V int `json:"v"`
	// Cumulative for this machine's lifetime, matching the cursorGenerations
	// counters it is read alongside. Every step, not just `stop`: an overrun
	// happens before the payload is parsed, so the step is not knowable here, and
	// pretending otherwise would put invented structure on the one number whose
	// whole value is that it is unconditional.
	Overruns int64 `json:"overruns"`
}

const cursorHookOverrunsVersion = 1

func loadCursorHookOverruns() cursorHookOverruns {
	c := cursorHookOverruns{V: cursorHookOverrunsVersion}
	data, err := os.ReadFile(cursorHookOverrunsPath()) // #nosec G304 -- state dir path.
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

// recordCursorHookOverrun bumps the counter, best-effort.
//
// Errors are swallowed for the same reason saveCursorGenerations swallows
// them: this runs on a path that has ALREADY overrun its budget inside the
// engineer's agent loop, and a lost count is a cheaper outcome than another
// millisecond spent here.
func recordCursorHookOverrun() {
	_ = sign.WithBufferLock(cursorHookOverrunsPath()+".lock", func() error {
		c := loadCursorHookOverruns()
		c.V = cursorHookOverrunsVersion
		c.Overruns++
		data, err := json.Marshal(c)
		if err != nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(cursorHookOverrunsPath()), 0o700); err != nil {
			return nil
		}
		tmp := cursorHookOverrunsPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return nil
		}
		_ = os.Rename(tmp, cursorHookOverrunsPath())
		return nil
	})
}
