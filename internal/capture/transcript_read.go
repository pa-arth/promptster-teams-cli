package capture

import (
	"bufio"
	"bytes"
	"io"
)

// transcriptReadOutcome is one bounded tail pass's result, shared by the Claude
// and Codex rails so the two cannot drift on the stop conditions.
//
// consumed and discarded are deliberately separate numbers. consumed is how far
// the durable offset advances and what the caller subtracts from the shared
// per-poll budget; discarded is the subset of it that reached no parser. Feeding
// discarded bytes to a PARSER-health detector is what once flipped the Claude
// watcher to degraded off a single unsupported record — see
// claudeDegradationStep, whose threshold is two orders of magnitude below one
// discard.
type transcriptReadOutcome struct {
	consumed  int64
	discarded int64
	// truncated reports readable bytes left behind for the next poll.
	truncated bool
	// probedOversize reports that this pass spent the poll's single
	// oversized-record allowance (see readTranscriptRecords).
	probedOversize bool
}

// readTranscriptRecords reads complete newline-terminated records from src,
// handing each trimmed record to handle, and reports how far the caller's
// durable offset may advance.
//
// budget is the caller's REMAINING shared per-poll budget, and it is a hard
// boundary: a record that does not fit in it is left intact for the next poll
// rather than half-read, because the file is the durable buffer and a deferred
// read is never a lost read.
//
// That boundary alone cannot resolve a record LARGER than the supported
// maximum: it never fits, so its file would stall at one offset forever. The
// escape used to key off `budget == transcriptMaxRecordBytes`, which only holds
// while the stalled file is the first budget-consuming file in walk order — one
// live session tailing a few kilobytes ahead of it in the walk was enough to
// keep the escape from ever firing. So oversizedness is decided against the
// SUPPORTED MAXIMUM instead of against whatever budget happens to be left:
// oversizeProbe lets a pass that has consumed nothing scan up to
// transcriptMaxRecordBytes for the record's newline regardless of the remaining
// budget. Total work stays bounded because the caller grants that allowance to
// at most one pass per poll (probedOversize reports when it was spent), and
// because the probe only ever DISCARDS — a record that terminates inside the
// supported maximum is still deferred intact, so no complete record is ever
// consumed past the shared budget.
func readTranscriptRecords(src io.Reader, budget int64, oversizeProbe bool, handle func(record []byte)) transcriptReadOutcome {
	out := transcriptReadOutcome{}
	if budget <= 0 {
		out.truncated = true
		return out
	}

	limited := &io.LimitedReader{R: src, N: budget}
	reader := bufio.NewReader(limited)
	for {
		if out.consumed >= budget {
			out.truncated = true
			break
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// A genuine partial record at the file's own EOF is retried intact
			// next poll; only a budget-exhausted read can be an oversized one.
			if err != io.EOF || limited.N != 0 {
				break
			}
			out.truncated = true
			if out.consumed == 0 && int64(len(line)) == budget {
				out.consumed, out.discarded, out.probedOversize =
					resolveOversizedRecord(reader, limited, budget, oversizeProbe)
			}
			break
		}
		out.consumed += int64(len(line))
		record := bytes.TrimSpace(line)
		if len(record) == 0 {
			continue
		}
		handle(record)
	}
	return out
}

// resolveOversizedRecord decides what to do with a record that swallowed the
// whole remaining budget without terminating, having consumed nothing before
// it. reader/limited are the in-flight readers, positioned at scanned bytes
// into the record.
//
// It returns bytes to advance over, bytes discarded unparsed, and whether the
// caller's single per-poll oversize allowance was spent.
func resolveOversizedRecord(reader *bufio.Reader, limited *io.LimitedReader, scanned int64, oversizeProbe bool) (int64, int64, bool) {
	// The record already outran the supported maximum, so no further reading
	// can change the answer and no allowance is spent.
	if scanned >= transcriptMaxRecordBytes {
		return transcriptMaxRecordBytes, transcriptMaxRecordBytes, false
	}
	if !oversizeProbe {
		// Another file already spent this poll's allowance. Defer intact; the
		// record is still here next poll.
		return 0, 0, false
	}
	limited.N = transcriptMaxRecordBytes - scanned
	if _, err := reader.ReadBytes('\n'); err == nil {
		// Terminates within the supported maximum: an ordinary record that is
		// merely bigger than this poll's leftover budget. Defer it intact.
		return 0, 0, true
	} else if err != io.EOF || limited.N != 0 {
		// Still growing on disk, or a read error — nothing to decide yet.
		return 0, 0, true
	}
	// No newline within the supported maximum. No future poll can complete this
	// record however much budget it has, so advance over this bounded prefix and
	// let subsequent polls discard the rest up to its newline.
	return transcriptMaxRecordBytes, transcriptMaxRecordBytes, true
}
