package main

import "time"

// Session is the teams capture context. Unlike the hiring CLI, it is NOT a
// redeemed assessment: it is built from environment configuration at watch
// start and carries ONLY what the transcript watchers need — where to send,
// with what token, and which directory to watch. No assessment identity, no
// trap markers, no behavioral-timing state, no time limit — by design, so a
// customer security review sees only capture plumbing.
type Session struct {
	// SessionID is a stable per-device capture identity (hash of machine id).
	SessionID string
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
