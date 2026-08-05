package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// History reconstruction is the longest-running thing the daemon does and, until
// this file, the only one with no way to ask about it.
//
// During the #140 replay — ~10.4 hours on one device — `status` said "capture
// running", `doctor` said "delivery queue draining", and the one honest
// description of that machine ("two thirds of the way through re-reading three
// weeks of history") was available nowhere. The engineer asking why their laptop
// was uploading so much had no answer, and neither did anyone supporting them.
//
// The line below is deliberately an OK, not a warning. A replay is a normal,
// deliberate thing this product does; reporting it as a fault would train people
// to ignore it, and the failure being fixed is that it was INVISIBLE, not that
// it was insufficiently alarming.

// reconstructionLines describes history replay for doctor. Empty when nothing is
// being reconstructed, which is the ordinary state and deserves no line at all —
// a machine that reports "not reconstructing" on every run has taught the reader
// to skip that row by the third time they see it.
func reconstructionLines(r capture.ReconstructionState, now time.Time) []queueLine {
	if !r.Running {
		return nil
	}

	// The DATE the replay has reached, not a percentage. The window is bounded by
	// date, so a date says both how far along it is and how far it has to go —
	// and it does not pretend to a precision the byte counts do not have.
	back := ""
	if !r.Oldest.IsZero() {
		back = fmt.Sprintf(", oldest %s (%s back)",
			r.Oldest.Format("2006-01-02"), humanizeDuration(now.Sub(r.Oldest)))
	}

	return []queueLine{{queueOK, fmt.Sprintf(
		"reconstructing history — %s still to re-read across %s%s. This is a one-time replay; "+
			"the events go on the backfill delivery lane, so live capture is unaffected",
		humanizeBytes(r.Bytes), transcriptCount(r.Files), back)}}
}

// progressWriteFaultLines warns that a device cannot persist capture progress.
//
// A WARNING, unlike the reconstruction line directly above it, and the contrast
// is the point: a replay is deliberate behaviour that happens to be slow, while
// an unwritable state directory is a fault with a fix. Reporting them at the
// same level would make the actionable one indistinguishable from the one whose
// correct response is to wait.
//
// It also says the part the operator cannot see: the replay they are watching is
// not one-time after all, and will run again at the next start.
func progressWriteFaultLines(faulted []string) []queueLine {
	if len(faulted) == 0 {
		return nil
	}
	return []queueLine{{queueWarn, fmt.Sprintf(
		"cannot save capture progress (%s) — read offsets are not being recorded, so this "+
			"device re-reads its full history window on EVERY restart. Check permissions and "+
			"free space on %s",
		strings.Join(faulted, ", "), state.StateDir())}}
}

// statusProgressWriteFaultRow is the `status` panel's row for the same fault, or
// nil when progress is persisting normally.
func statusProgressWriteFaultRow(faulted []string) []string {
	if len(faulted) == 0 {
		return nil
	}
	return []string{"progress", fmt.Sprintf("NOT SAVING (%s) — replays every restart",
		strings.Join(faulted, ", "))}
}

func transcriptCount(n int) string {
	if n == 1 {
		return "1 transcript"
	}
	return fmt.Sprintf("%d transcripts", n)
}

// reconNow is capture.ReconstructionNow. A var because the dashboard refreshes
// it on a schedule rather than on every render, and a scheduling claim nobody
// can count the calls behind is a claim, not a guarantee.
var reconNow = capture.ReconstructionNow

// writeFaultsNow is capture.ProgressWriteFaulted, a var for the same reason.
var writeFaultsNow = capture.ProgressWriteFaulted

// statusReconstructionRow is the `status` panel's key/value pair for a running
// replay, or nil when none is running.
//
// Terser than doctor's line on purpose: `status` is a glanceable panel and
// doctor is where an explanation belongs.
func statusReconstructionRow() []string {
	r := reconNow()
	if !r.Running {
		return nil
	}
	v := fmt.Sprintf("%s left across %s", humanizeBytes(r.Bytes), transcriptCount(r.Files))
	if !r.Oldest.IsZero() {
		v += fmt.Sprintf(", back to %s", r.Oldest.Format("2006-01-02"))
	}
	return []string{"replaying history", v}
}
