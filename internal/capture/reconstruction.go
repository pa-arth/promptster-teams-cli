package capture

import (
	"os"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
)

// HISTORY RECONSTRUCTION IS THE LONGEST-RUNNING THING THIS DAEMON DOES, AND
// UNTIL NOW IT WAS THE ONLY ONE WITH NO WAY TO ASK ABOUT IT.
//
// A schema bump re-reads up to 28 days of transcripts on every device. On the
// #140 episode that was ~10.4 hours of replay on one machine, during which
// `status` said "capture running", `doctor` said "delivery queue draining", and
// the only honest description of the machine — "it is two thirds of the way
// through re-reading three weeks of history" — was available nowhere. An
// engineer asking "why is my laptop uploading so much" had no answer, and
// neither did anyone supporting them.
//
// #155 made the daemon ANNOUNCE the replay when it starts. This makes the replay
// OBSERVABLE while it runs, which is a different question: the announcement
// scrolls past in a log, and the operator arrives an hour later.
//
// WHAT COUNTS AS RECONSTRUCTION. A transcript with unread bytes is not enough —
// the live tail of an active session has unread bytes for a few hundred
// milliseconds every time someone types. Reconstruction is unread bytes in files
// whose newest content is ALREADY OLD, and "old" here is deliberately
// outbox.LiveHorizon: the same boundary the outbox uses to decide the backfill
// lane, so what this reports as reconstruction is exactly what lands there.
// Sharing the constant is the point — two nearby thresholds that drift apart
// would make the two reports contradict each other about the same file.

// ReconstructionState is what `status` and `doctor` say about history replay.
type ReconstructionState struct {
	// Running is true when at least one already-old transcript still has unread
	// bytes. False on a machine that is merely tailing live sessions.
	Running bool

	// Files and Bytes are what REMAINS, not what has been done. "3 files, 40 MB
	// left" is actionable; "read 900 MB" is trivia, and on a resumed replay it is
	// not even recoverable.
	Files int
	Bytes int64

	// Oldest is the mtime of the oldest transcript still holding unread bytes —
	// how far back in the window the replay currently is. Zero when nothing is
	// pending.
	//
	// This is the number that answers "how much of the window remains", and it
	// answers it better than a percentage would: the window is bounded by DATE
	// (transcriptHistoryWindow), so a date says both how far along the replay is
	// and how far it still has to go, without pretending to a precision the byte
	// counts do not have.
	Oldest time.Time
}

// reconstructionScan is one watcher's contribution, before the two are merged.
type reconstructionScan struct {
	files  int
	bytes  int64
	oldest time.Time
}

// ReconstructionNow reports history-replay progress across both transcript
// watchers.
//
// Read-only and cheap by construction: one Walk per watcher over files already
// bounded by the history window, and a Stat each. It is called from `status` and
// `doctor`, which are one-shot, never from the poll loop.
func ReconstructionNow() ReconstructionState {
	cutoff := transcriptHistoryCutoff(time.Now().UTC())
	// outbox.LiveHorizon itself, read here rather than copied into a local
	// constant — see reconstructionLiveGrace.
	stale := time.Now().Add(-reconstructionLiveGrace())

	scans := []reconstructionScan{
		scanClaudeReconstruction(cutoff, stale),
		scanCodexReconstruction(cutoff, stale),
	}

	out := ReconstructionState{}
	for _, s := range scans {
		out.Files += s.files
		out.Bytes += s.bytes
		if s.oldest.IsZero() {
			continue
		}
		if out.Oldest.IsZero() || s.oldest.Before(out.Oldest) {
			out.Oldest = s.oldest
		}
	}
	out.Running = out.Files > 0
	return out
}

// reconstructionLiveGrace is how recently a transcript must have been written to
// count as LIVE rather than as history awaiting replay.
//
// It RETURNS outbox.LiveHorizon rather than restating its value, and review
// caught the difference. The first draft was `const reconstructionLiveGrace = 30
// * time.Minute` under a comment claiming it was deliberately the same boundary
// — which is exactly the drift the comment warned about, one edit away: moving
// the outbox horizon would have left this at 30 minutes with nothing failing, and
// the two reports would then disagree about the same file, one calling it live
// while the other counted it as history awaiting replay.
//
// A function and not `var x = outbox.LiveHorizon`, because that is an init-time
// copy: LiveHorizon is a var, so anything that sets it after init (a test, a
// future flag) would diverge from the copy silently. Reading it per call is the
// only form that cannot drift.
//
// Kept as a named wrapper rather than inlining outbox.LiveHorizon at the use
// site so the dependency stays visible where the value is used.
func reconstructionLiveGrace() time.Duration { return outbox.LiveHorizon }

func scanClaudeReconstruction(cutoff, stale time.Time) reconstructionScan {
	progress := loadClaudeWatchProgress()
	var out reconstructionScan
	for _, path := range candidateClaudeTranscripts(cutoff) {
		key := claudeProgressKey(path)
		// Only files this device decided to capture. An unclassified or
		// cwd-mismatched transcript is not work in progress; counting it would
		// report a replay that is never going to happen.
		if progress.Match[key] != "yes" {
			continue
		}
		accumulateReconstruction(&out, path, progress.Offsets[key], stale)
	}
	return out
}

func scanCodexReconstruction(cutoff, stale time.Time) reconstructionScan {
	progress := loadCodexWatchProgress()
	var out reconstructionScan
	for _, path := range candidateCodexRollouts(codexSessionsDir(), cutoff) {
		if progress.Match[path] != "yes" {
			continue
		}
		accumulateReconstruction(&out, path, progress.Offsets[path], stale)
	}
	return out
}

// accumulateReconstruction adds one transcript's unread tail, if it is history
// rather than a live tail.
func accumulateReconstruction(out *reconstructionScan, path string, offset int64, stale time.Time) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	// A file whose newest content is recent is being TAILED, not reconstructed.
	// Its unread bytes are seconds old and will be gone on the next poll.
	if fi.ModTime().After(stale) {
		return
	}
	remaining := fi.Size() - offset
	if remaining <= 0 {
		return
	}
	out.files++
	out.bytes += remaining
	if out.oldest.IsZero() || fi.ModTime().Before(out.oldest) {
		out.oldest = fi.ModTime()
	}
}
