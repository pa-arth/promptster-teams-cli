package capture

import "time"

// WatcherStat is a point-in-time view of one transcript watcher, read from its
// pidfile under StateDir(). The zero value (Running=false) means there is no
// live watcher of that kind.
type WatcherStat struct {
	Name          string // "claude" | "codex"
	Running       bool
	Degraded      bool // running but parsing nothing from bytes it consumed
	PID           int
	EventsSent    int
	BytesConsumed int64
	LastHeartbeat time.Time // zero if unknown
	StartedAt     time.Time // zero if unknown
}

// CaptureSnapshot is a point-in-time view of background capture for the status
// dashboard. Everything is read from this state dir's pidfiles, so it is cheap
// enough to call on every UI tick.
type CaptureSnapshot struct {
	// Live is true when capture is actually running — either the supervisor is
	// alive (started via `start`) or at least one watcher is (a daemon launched
	// as a bare `watch`, e.g. the npm binary, writes no supervisor.json but does
	// write the watcher pidfiles).
	Live bool
	// DaemonPID is the supervisor pid, or the live watcher pid when no supervisor
	// pidfile exists (they are the same process — the watchers are goroutines).
	DaemonPID int
	Claude    WatcherStat
	Codex     WatcherStat
}

// watcherStaleGrace bounds how long after its last heartbeat a watcher pidfile
// is still trusted as live. A crashed watcher's pidfile can linger with a PID
// the OS later reuses; requiring a recent heartbeat keeps that reused PID (whose
// heartbeat is old) from being reported as live capture. Generous relative to
// the ~3s poll cadence so a couple of missed polls don't flap the display.
const watcherStaleGrace = 2 * time.Minute

// watcherLive reports whether a watcher pidfile represents live capture. The PID
// must exist AND the heartbeat must be present and recent. Both watchers write a
// heartbeat at startup and refresh it every poll, so a missing/unparsable (zero)
// heartbeat means a stale or malformed pidfile — not a fresh watcher — and a
// future heartbeat (negative age) is likewise untrusted; either way we must not
// report a reused PID as active.
func watcherLive(pid int, heartbeat, now time.Time) bool {
	if !processExists(pid) || heartbeat.IsZero() {
		return false
	}
	age := now.Sub(heartbeat)
	return age >= 0 && age <= watcherStaleGrace
}

func parseWatchTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func claudeWatcherStat(now time.Time) WatcherStat {
	st, err := loadClaudeWatcherState()
	if err != nil || st.PID <= 0 {
		return WatcherStat{Name: "claude"}
	}
	hb := parseWatchTime(st.LastHeartbeat)
	return WatcherStat{
		Name:          "claude",
		Running:       watcherLive(st.PID, hb, now),
		Degraded:      st.Degraded,
		PID:           st.PID,
		EventsSent:    st.EventsSent,
		BytesConsumed: st.BytesConsumed,
		LastHeartbeat: hb,
		StartedAt:     parseWatchTime(st.StartedAt),
	}
}

func codexWatcherStat(now time.Time) WatcherStat {
	st, err := loadCodexWatcherState()
	if err != nil || st.PID <= 0 {
		return WatcherStat{Name: "codex"}
	}
	hb := parseWatchTime(st.LastHeartbeat)
	return WatcherStat{
		Name:          "codex",
		Running:       watcherLive(st.PID, hb, now),
		PID:           st.PID,
		EventsSent:    st.EventsSent,
		LastHeartbeat: hb,
		StartedAt:     parseWatchTime(st.StartedAt),
	}
}

// Snapshot reads the current background-capture state for the status dashboard.
// Safe to call repeatedly (once per UI tick); it only reads small JSON pidfiles.
func Snapshot() CaptureSnapshot {
	now := time.Now()
	claude := claudeWatcherStat(now)
	codex := codexWatcherStat(now)
	pid, running := DaemonStatus()

	effPID := pid
	if effPID == 0 {
		if claude.Running {
			effPID = claude.PID
		} else if codex.Running {
			effPID = codex.PID
		}
	}
	return CaptureSnapshot{
		Live:      running || claude.Running || codex.Running,
		DaemonPID: effPID,
		Claude:    claude,
		Codex:     codex,
	}
}

// StartedAt returns the earliest known watcher start time, for uptime display.
// Zero if neither watcher recorded a start time.
func (s CaptureSnapshot) StartedAt() time.Time {
	c, x := s.Claude.StartedAt, s.Codex.StartedAt
	switch {
	case c.IsZero():
		return x
	case x.IsZero():
		return c
	case x.Before(c):
		return x
	default:
		return c
	}
}
