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
	// discardingOversize reports that the durable offset still points inside an
	// unsupported record. The caller persists this bit with the offset so the
	// next poll discards the suffix instead of handing it to a parser.
	discardingOversize bool
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
func readTranscriptRecords(src io.Reader, budget int64, oversizeProbe, discardingOversize bool, handle func(record []byte) bool) transcriptReadOutcome {
	out := transcriptReadOutcome{discardingOversize: discardingOversize}
	if budget <= 0 {
		out.truncated = true
		return out
	}

	limited := &io.LimitedReader{R: src, N: budget}
	reader := bufio.NewReader(limited)
	if out.discardingOversize {
		for out.discardingOversize {
			fragment, err := reader.ReadSlice('\n')
			read := int64(len(fragment))
			out.consumed += read
			out.discarded += read
			switch err {
			case nil:
				out.discardingOversize = false
			case bufio.ErrBufferFull:
				continue
			default:
				out.truncated = true
				return out
			}
		}
	}
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
				out.consumed, out.discarded, out.probedOversize, out.discardingOversize =
					resolveOversizedRecord(reader, limited, budget, oversizeProbe)
			}
			break
		}
		out.consumed += int64(len(line))
		record := bytes.TrimSpace(line)
		if len(record) == 0 {
			continue
		}
		if !handle(record) {
			break
		}
	}
	return out
}

// classifyReadBufSize is the fixed working buffer the classification readers
// stream through. A record is accumulated up to transcriptMaxRecordBytes;
// anything larger is discarded to its newline through this same buffer, so
// skipping an unsupported record costs no memory beyond it.
const classifyReadBufSize = 64 * 1024

// newClassifyReader wraps a transcript for a classification pass.
//
// It replaces the bufio.Scanner these passes used to run, which capped a token
// at 8 MiB and had no way to say "skip this one": a record over the cap made
// Scan return false, so the classifier returned undecided, cached nothing, and
// the file was re-read from byte zero on every 3s poll FOREVER — never reaching
// the tail path, which is where the oversized-record escape lives. See
// nextClassifyRecord.
func newClassifyReader(r io.Reader) *bufio.Reader {
	return bufio.NewReaderSize(r, classifyReadBufSize)
}

// nextClassifyRecord returns the next record from a classification reader,
// trimmed. ok reports that a record BOUNDARY was reached; the returned slice
// aliases the reader's buffer and is only valid until the next call.
//
// A record longer than transcriptMaxRecordBytes is DISCARDED to its newline and
// returned as an empty record with ok true. No future poll could parse it
// however much budget it had — it is measured against the same supported
// maximum the tail path uses, never against a scanner constant that can drift
// from it — so skipping it is what lets classification reach the record it
// actually needs (the first cwd-bearing line for Claude, session_meta for
// Codex) instead of stalling on the one ahead of it. The skip stops exactly at
// the newline, so the next record is never consumed with it.
//
// An oversized record that has NOT terminated by EOF returns ok false: it may
// still be growing, and the same "defer, never drop" rule the tail path applies
// holds here. A final SUPPORTED-size record with no trailing newline is still
// returned, matching the Scanner behavior this replaced.
func nextClassifyRecord(r *bufio.Reader) ([]byte, bool) {
	var buf []byte
	var size int64
	for {
		frag, err := r.ReadSlice('\n')
		size += int64(len(frag))
		if size > transcriptMaxRecordBytes {
			switch err {
			case nil:
				return nil, true // terminated inside this fragment: already skipped
			case bufio.ErrBufferFull:
				return nil, discardRecordToNewline(r)
			default:
				return nil, false // unterminated at EOF: may still be growing
			}
		}
		switch err {
		case nil:
			if len(buf) == 0 {
				return bytes.TrimSpace(frag), true
			}
			return bytes.TrimSpace(append(buf, frag...)), true
		case bufio.ErrBufferFull:
			buf = append(buf, frag...)
		default:
			if err == io.EOF && len(buf)+len(frag) > 0 {
				return bytes.TrimSpace(append(buf, frag...)), true
			}
			return nil, false
		}
	}
}

// discardRecordToNewline streams past the remainder of an unsupported record,
// reporting whether its newline was reached. Nothing is retained.
func discardRecordToNewline(r *bufio.Reader) bool {
	for {
		switch _, err := r.ReadSlice('\n'); err {
		case nil:
			return true
		case bufio.ErrBufferFull:
		default:
			return false
		}
	}
}

// resolveOversizedRecord decides what to do with a record that swallowed the
// whole remaining budget without terminating, having consumed nothing before
// it. reader/limited are the in-flight readers, positioned at scanned bytes
// into the record.
//
// It returns bytes to advance over, bytes discarded unparsed, and whether the
// caller's single per-poll oversize allowance was spent.
func resolveOversizedRecord(reader *bufio.Reader, limited *io.LimitedReader, scanned int64, oversizeProbe bool) (int64, int64, bool, bool) {
	// The record already outran the supported maximum, so no further reading
	// can change the answer and no allowance is spent.
	if scanned >= transcriptMaxRecordBytes {
		return transcriptMaxRecordBytes, transcriptMaxRecordBytes, false, true
	}
	if !oversizeProbe {
		// Another file already spent this poll's allowance. Defer intact; the
		// record is still here next poll.
		return 0, 0, false, false
	}
	limited.N = transcriptMaxRecordBytes - scanned
	if _, err := reader.ReadBytes('\n'); err == nil {
		// Terminates within the supported maximum: an ordinary record that is
		// merely bigger than this poll's leftover budget. Defer it intact.
		return 0, 0, true, false
	} else if err != io.EOF || limited.N != 0 {
		// Still growing on disk, or a read error — nothing to decide yet.
		return 0, 0, true, false
	}
	// No newline within the supported maximum. No future poll can complete this
	// record however much budget it has, so advance over this bounded prefix and
	// let subsequent polls discard the rest up to its newline.
	return transcriptMaxRecordBytes, transcriptMaxRecordBytes, true, true
}
