package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// The statusline SHIM runtime — `promptster-teams statusline run`.
//
// Claude Code invokes this on every status-line tick, piping a JSON blob to our
// stdin and rendering our stdout. We: (1) read the whole blob, (2) lift the
// `rate_limits` window scalars and spool them for the watcher (latest-wins), then
// (3) run the engineer's PRIOR command with the SAME blob on its stdin and pass
// its stdout straight through — so their existing statusline keeps rendering.
//
// FAIL-OPEN + FAST is the contract with Claude Code: it calls this synchronously
// to draw a line, so we must ALWAYS emit something and never hang. Every step is
// best-effort; a spool failure, a parse failure, or a prior-command failure still
// results in a rendered line. The prior command runs under a hard timeout.
//
// PRIVACY: only the window scalars leave the device (via the spool → watcher →
// projection). The blob may contain a transcript path and model id; those are
// used only to feed the prior command's stdin and are never written to the spool,
// logged, or included in any error text.

// priorCommandTimeout bounds how long we wait on the wrapped statusline command
// so a slow third-party script can never wedge Claude Code's render.
const priorCommandTimeout = 2500 * time.Millisecond

// statuslineStdin is the MINIMAL projection of Claude Code's status-line blob we
// parse. Only rate_limits is named; the surrounding session/model/workspace
// fields are deliberately not lifted into our struct.
type statuslineStdin struct {
	RateLimits *struct {
		FiveHour *struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       *int64   `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       *int64   `json:"resets_at"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
}

// RunStatuslineShim is the `statusline run` entry point. It never returns an
// error to the caller path that matters (the exit code is always 0) so a broken
// tick cannot make Claude Code show an error line. Returns the process exit code.
func RunStatuslineShim() int {
	// Read the whole blob up front: we need it twice (parse + feed the prior
	// command), and it is small. Cap the read so a pathological producer can't
	// balloon memory.
	blob, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))

	// (2) Spool the window reading — best-effort, never blocks the render.
	if reading, ok := parseClaudeStatuslineBlob(blob, time.Now().Unix()); ok {
		_ = writeClaudeWindowSpool(reading)
	}

	// (3) Run the prior command with the same blob and pass its stdout through.
	out := runPriorStatusline(blob)
	_, _ = os.Stdout.Write(out)
	return 0
}

// parseClaudeStatuslineBlob lifts the 5h/weekly window scalars from a Claude Code
// status-line blob. observedAt is the tick time (contract.md §3: Claude
// observedAt ~= tick time). Absent/malformed fields are omitted (absent != zero;
// NaN/Inf dropped). ok=false when nothing usable was present.
func parseClaudeStatuslineBlob(blob []byte, observedAt int64) (windowReading, bool) {
	var in statuslineStdin
	if err := json.Unmarshal(blob, &in); err != nil {
		return windowReading{}, false
	}
	if in.RateLimits == nil {
		return windowReading{}, false
	}
	r := windowReading{ObservedAt: observedAt}
	if fh := in.RateLimits.FiveHour; fh != nil {
		if p, ok := sanePctPtr(fh.UsedPercentage); ok {
			r.FiveHourPct = &p
		}
		if t, ok := saneResetPtr(fh.ResetsAt); ok {
			r.FiveHourResetsAt = &t
		}
	}
	if sd := in.RateLimits.SevenDay; sd != nil {
		if p, ok := sanePctPtr(sd.UsedPercentage); ok {
			r.WeeklyPct = &p
		}
		if t, ok := saneResetPtr(sd.ResetsAt); ok {
			r.WeeklyResetsAt = &t
		}
	}
	if r.empty() {
		return windowReading{}, false
	}
	return r, true
}

// sanePctPtr validates an optional percentage: present, finite, non-negative.
func sanePctPtr(p *float64) (float64, bool) {
	if p == nil {
		return 0, false
	}
	v := *p
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}

// saneResetPtr validates an optional absolute reset epoch: present and positive.
// Claude's resets_at is already absolute epoch seconds (contract.md §3), so no
// countdown conversion is needed here.
func saneResetPtr(p *int64) (int64, bool) {
	if p == nil || *p <= 0 {
		return 0, false
	}
	return *p, true
}

// runPriorStatusline runs the engineer's wrapped statusline command with blob on
// its stdin and returns its stdout. When no prior command was stored (we
// installed ours), it renders our OWN compact line from the blob's window
// scalars. Fail-open: on any error or timeout it returns whatever it has —
// falling back to our own render — so a line is always drawn.
func runPriorStatusline(blob []byte) []byte {
	rec, ok := loadStatuslinePrior()
	if !ok || rec.Prior == nil || rec.Prior.Command == "" {
		// We installed ours — render the engineer's own usage line.
		return renderOwnStatusline(blob)
	}

	ctx, cancel := context.WithTimeout(context.Background(), priorCommandTimeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", rec.Prior.Command) // #nosec G204 -- the command is the engineer's OWN previously-configured statusLine, restored verbatim; we run exactly what Claude Code would have.
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", rec.Prior.Command) // #nosec G204 -- see above.
	}
	cmd.Stdin = bytes.NewReader(blob)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		// Prior command failed or timed out — still draw a line rather than let
		// Claude Code show nothing. Prefer the prior's partial stdout; fall back
		// to our own render if it produced nothing.
		if stdout.Len() > 0 {
			return stdout.Bytes()
		}
		return renderOwnStatusline(blob)
	}
	return stdout.Bytes()
}

// renderOwnStatusline draws a compact one-line usage readout from the blob's
// window scalars — what an engineer sees when we installed the statusline
// ourselves. Content-free apart from the two percentages. Never errors.
func renderOwnStatusline(blob []byte) []byte {
	reading, ok := parseClaudeStatuslineBlob(blob, time.Now().Unix())
	if !ok {
		return []byte("promptster: usage —\n")
	}
	five := "—"
	if reading.FiveHourPct != nil {
		five = fmt.Sprintf("%.0f%%", *reading.FiveHourPct)
	}
	week := "—"
	if reading.WeeklyPct != nil {
		week = fmt.Sprintf("%.0f%%", *reading.WeeklyPct)
	}
	return []byte(fmt.Sprintf("promptster · 5h %s · wk %s\n", five, week))
}
