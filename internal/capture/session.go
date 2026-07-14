package capture

import "time"

// Session is the teams capture context. Unlike the hiring CLI, it is NOT a
// redeemed assessment: it is built from environment configuration at watch
// start and carries ONLY what the transcript watchers need — where to send,
// with what token, and which directory to watch. No assessment identity, no
// trap markers, no behavioral-timing state, no time limit — by design, so a
// customer security review sees only capture plumbing.
type Session struct {
	// DeviceID is a stable per-device capture identity (hash of machine id).
	//
	// It is NOT a session id, and was named SessionID until it turned out that
	// every caller handing it to a normalizer stamped it on events as one —
	// collapsing every concurrent AI-tool session on a machine into a single
	// reported session, permanently. Real session ids come from the transcript
	// each watcher tails, never from here.
	DeviceID string
	// SessionToken is the ingest auth token (PROMPTSTER_TEAMS_TOKEN).
	SessionToken string
	// TaskRoot is the directory whose AI-tool transcripts are captured.
	TaskRoot string
	// ApiURL is the teams ingest base URL (PROMPTSTER_TEAMS_API_URL).
	ApiURL string
	// CaptureMode is always "transcript" — the only capture channel teams uses.
	CaptureMode string
	// StartedAt bounds which transcripts are in scope (those modified at/after).
	StartedAt time.Time
}
