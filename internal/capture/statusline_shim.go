package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
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
// to draw a line, so we must never hang and never error. Every step is
// best-effort; a spool failure, a parse failure, or a prior-command failure all
// still exit 0. The prior command runs under a hard timeout.
//
// "Fail open" means THEIR line or NO line — never ours over theirs. When we
// wrapped an existing statusline, a failed tick falls back to that command's
// last good output, then to nothing; our own compact line is reserved for the
// case where the slot was empty and we filled it. See runPriorStatusline.
//
// PRIVACY: only the window scalars leave the device (via the spool → watcher →
// projection). The blob may contain a transcript path and model id; those are
// used only to feed the prior command's stdin and are never written to the spool,
// logged, or included in any error text.

// priorCommandTimeout bounds how long we wait on the wrapped statusline command
// so a genuinely hung script can never wedge Claude Code's render.
//
// It is a WEDGE GUARD, not a latency budget, and the difference is the whole
// point. It was 2500ms, which is BELOW what a real statusline costs: claude-hud
// (node, plus a plugin-cache glob, plus a user extra-cmd subprocess) measured
// 0.6s-4.5s over eight consecutive runs on an idle laptop and exceeded 2.5s on
// five of them. Each of those ticks killed the engineer's statusline and drew
// ours in its place. Wrapping is only defensible if the wrapped line still
// renders; a wrapper that replaces its host most of the time is a replacement.
//
// Unwrapped, Claude Code runs that same command with no timeout of ours. So any
// value here that a real statusline can reach makes US the cause of a regression
// the engineer would not otherwise have had. Ten seconds is far past every
// statusline we have measured and still bounds a hung process.
//
// WHAT CLAUDE CODE ITSELF DOES, read out of the 2.1.246 binary — do not
// re-derive this from the docs, which do not say it:
//
//   - There is NO per-command timeout on the statusLine path. The executor
//     forwards the CALLER's abort signal to the runner. The sibling
//     `fileSuggestion` executor in the same module wraps its call in an explicit
//     `AbortSignal.timeout(5000)`; the statusLine one does not. Where a hard
//     bound was wanted it was written, and it was not written here.
//   - The bound that DOES exist is cancellation: a new tick aborts an in-flight
//     script (docs, "Claude Code cancels the in-flight script"), with a 300ms
//     debounce between triggers.
//   - `statusLineHealthLatches` is NOT a circuit breaker. It is literally
//     `class { okLogged = false; badLogged = false }`, one per host, and its only
//     job is to emit the `status_line_command` telemetry event once. Nothing
//     reads it to skip execution: a slow or failing statusline is re-run on the
//     next tick forever. So a long tick costs us a stale line, never capture.
//   - Non-zero exit AND empty stdout both render blank, identically. We exit 0
//     always; the empty-render rung below is therefore a blank line, not an error.
//
// Which is why 10s is safe: there is no hidden penalty to stay under, and our
// spools are already written ~15ms in — before the prior command starts, and so
// before any abort or overrun can reach us.
//
// A var, not a const, only so tests can shrink it and exercise the timeout path.
var priorCommandTimeout = 10 * time.Second

// statuslineStdin is the MINIMAL projection of Claude Code's status-line blob we
// parse. Only rate_limits, the context window, and the session id that keys it
// are named; the surrounding transcript path, workspace and cost fields are
// deliberately not lifted into our struct.
type statuslineStdin struct {
	// SessionID keys the per-session context spool. It is the SAME id the
	// transcript filename carries — Claude Code builds both from one value
	// (`session_id: e.id, transcript_path: J4(e.id)`), which is what makes the
	// join to a tailed transcript exact rather than heuristic.
	SessionID string `json:"session_id"`
	// ContextWindow is the ONLY channel that reports a Claude session's window;
	// the transcript carries no such field. Documented by Claude Code itself as
	// "Context window size for current model (e.g., 200000)".
	ContextWindow *struct {
		ContextWindowSize *float64 `json:"context_window_size"`
	} `json:"context_window"`
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

	now := time.Now().Unix()

	// (2) Spool the rate-limit window reading — best-effort, never blocks the render.
	if reading, ok := parseClaudeStatuslineBlob(blob, now); ok {
		_ = writeClaudeWindowSpool(reading)
	}

	// (2b) Spool the CONTEXT window, keyed by session. Separate spool, separate
	// key, separate lifecycle — see claude_context_window.go. Also best-effort:
	// a missing denominator degrades a downstream tile to "ceiling unknown",
	// which is a far cheaper failure than a status line that does not render.
	if sessionID, reading, ok := parseClaudeContextWindow(blob, now); ok {
		_ = writeClaudeContextSpool(sessionID, reading)
	}

	// (3) Run the prior command with the same blob and pass its stdout through.
	out := runPriorStatusline(blob)
	_, _ = os.Stdout.Write(out)
	return 0
}

// parseClaudeStatuslineBlob lifts the 5h/weekly window scalars from a Claude Code
// status-line blob. observedAt is the tick time (contract.md §3: Claude
// observedAt ~= tick time). Absent/malformed fields are omitted (absent != zero;
// NaN/Inf dropped).
//
// A blob with NO `rate_limits` is an OBSERVED ABSENCE, not a nothing: the shim
// demonstrably ran and Claude reported no window, which is the one and only
// evidence that earns "this account is usage-billed". It used to return
// ok=false, which made that indistinguishable from a shim that never ran — five
// causes collapsed into one observation, and the surface printed the least
// likely of them as fact. The absence carries a state and a time and NO
// percentage, ever.
//
// ok=false is now reserved for "this is not a statusline blob at all" (the JSON
// did not parse). We assert nothing from a payload we could not read.
//
// THE PRE-FIRST-RESPONSE TICK IS NOT SPECIAL-CASED HERE, DELIBERATELY.
// A flat-fee subscriber's status line renders before their session's first API
// response, and Claude legitimately reports no `rate_limits` at that moment —
// there is no window to report until a request has been made. So this function
// emits `provider_absent` for a subscriber, and one tick of that must never
// become "this person is billed per token" on their manager's screen (parent
// spec task 1.1, "confirm the pre-first-response absence").
//
// The shim is nonetheless RIGHT to emit it, and suppressing it here would be the
// wrong fix twice over:
//
//   - "we asked and Claude reported nothing" is a true observation whatever the
//     cause, and making it expressible is the entire point of this change.
//     Withholding it would put a subscriber's absence and a shim that never ran
//     back into one indistinguishable silence — the defect we just removed.
//   - the shim cannot tell the two apart cheaply or honestly. The blob's cost
//     fields would be an inference about first-response state, and they are
//     deliberately not lifted into `statuslineStdin` (see the struct's doc).
//     Guessing here is the same over-reading, moved upstream where it is harder
//     to see.
//
// The CONCLUSION is what gets corroborated, and it is corroborated where the
// evidence actually lives — the backend's `deriveNoSignalReason`, which grants
// `usage_billed` only when the absence RECURRED and the engineer was observed
// doing Claude work at or after it began. A startup tick is one observation with
// no work behind it and earns nothing. Keep those two facts together: this
// emission is only safe because that derivation is corroborated, and a reader who
// changes one should read the other.
func parseClaudeStatuslineBlob(blob []byte, observedAt int64) (windowReading, bool) {
	var in statuslineStdin
	if err := json.Unmarshal(blob, &in); err != nil {
		return windowReading{}, false
	}
	if in.RateLimits == nil {
		return absenceReading(signalProviderAbsent, observedAt), true
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
		// `rate_limits` was present but carried nothing usable — no percentage and
		// no reset on either window. Claude reports only the two windows this
		// contract already names, so there is no third span hiding here: the
		// provider was asked and answered with nothing. Same fact as a missing
		// object, same state.
		return absenceReading(signalProviderAbsent, observedAt), true
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

// --- last-good output cache -------------------------------------------------
//
// The wrapped command's most recent SUCCESSFUL stdout, so a tick that fails or
// times out can redraw the engineer's OWN line instead of ours. One tick stale
// is invisible on a status line; a brand swap is not.
//
// PRIVACY: this file holds a RENDERED statusline, which is third-party content —
// a branch name, a repo name, whatever the engineer's script chose to draw. It
// is written 0600 under the state dir, read by exactly ONE function (the shim's
// own fallback), and is never parsed, spooled, logged, or emitted. The egress
// path drains the window spools and does not know this file exists. Keep it that
// way: the moment anything downstream reads it, we are shipping rendered content
// off the device.

// maxLastGoodBytes caps what we are willing to remember. A status line is one or
// two rendered lines; anything larger is a misbehaving command, not a line.
const maxLastGoodBytes = 64 << 10

func statuslineLastGoodPath() string {
	return filepath.Join(state.StateDir(), "statusline-lastgood")
}

// statuslineLastGood is the cache record. It carries a FINGERPRINT of the
// command whose output it holds, and that is a correctness mechanism, not
// bookkeeping.
//
// The shim takes NO lock — it is a per-tick hot path in every open session, and
// making it contend would be the wrong trade. So a tick can be mid-run with the
// OLD wrapped command while a heal swaps the prior underneath it, and then
// publish that old command's output into a cache the new command will read.
// Clearing the cache on re-wrap does not help: the clear happens first and the
// stale write lands after it. The result is precisely the quiet substitution the
// cache was introduced to prevent — a failed NEW statusline rendering the OLD
// one's line.
//
// Fingerprinting closes it without a lock: a cache entry is only ever served to
// the command that produced it, so a late write from a superseded tick is inert
// rather than wrong. The clear-on-rewrap is now hygiene, not the guarantee.
//
// The fingerprint is a HASH, not the command: the prior record already holds the
// command, and statusline commands carry inline credentials, so there is no
// reason for a second file to hold a copy.
type statuslineLastGood struct {
	Fingerprint string `json:"fp"`
	Output      []byte `json:"out"`
}

// commandFingerprint identifies a wrapped command without reproducing it.
func commandFingerprint(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:16])
}

// saveStatuslineLastGood records a successful render. Best-effort and silent:
// this runs on the status-line hot path, and a cache we could not write is a
// worse fallback next tick, never a failed render this tick.
func saveStatuslineLastGood(command string, out []byte) {
	if len(out) == 0 || len(out) > maxLastGoodBytes {
		return
	}
	data, err := json.Marshal(statuslineLastGood{
		Fingerprint: commandFingerprint(command),
		Output:      out,
	})
	if err != nil {
		return
	}
	path := statuslineLastGoodPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	// A per-write temp, not a shared `<path>.tmp`. This is the hot path of a
	// process that runs once per status-line tick, in EVERY open Claude Code
	// session at once — the single most concurrent writer in the subsystem. Two
	// shims sharing one temp name interleave their bytes and then rename the
	// result into place, so the fallback line one session publishes is a torn
	// splice of another's. Same defect the window spool already fixed.
	_ = writeFileAtomic(dir, path, data)
}

// loadStatuslineLastGood returns the cached render ONLY if it belongs to the
// command being asked about. A mismatch is a superseded entry and yields
// nothing — better a blank tick than another statusline's line.
func loadStatuslineLastGood(command string) []byte {
	data, err := os.ReadFile(statuslineLastGoodPath()) // #nosec G304 -- fixed path under the state dir.
	if err != nil {
		return nil
	}
	var rec statuslineLastGood
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil
	}
	if rec.Fingerprint != commandFingerprint(command) {
		return nil
	}
	return rec.Output
}

func clearStatuslineLastGood() { _ = os.Remove(statuslineLastGoodPath()) }

// runPriorStatusline runs the engineer's wrapped statusline command with blob on
// its stdin and returns its stdout. When no prior command was stored (we
// installed ours into an empty slot), it renders our OWN compact line from the
// blob's window scalars. Fail-open: a line is always drawn, or none is, but the
// render never errors.
//
// OUR OWN LINE IS NEVER THE FALLBACK FOR A COMMAND WE WRAPPED. That is the whole
// contract of wrapping, and breaking it is not cosmetic: the engineer chose a
// statusline, we quietly took the slot, and every slow tick handed them
// promptster branding where their own tool's output had been. It is also
// self-concealing — the line that says what went wrong is the line that was
// replaced.
//
// The degradation ladder for a wrapped command that fails is therefore:
//
//	last good output  ->  this run's partial stdout  ->  nothing at all
//
// Complete-but-one-tick-stale beats fresh-but-truncated: a killed command's
// partial stdout can end mid-escape-sequence and bleed color into the terminal,
// while the previous tick's line differs only in a token count or a clock. And
// drawing NOTHING is a blank tick the engineer can attribute to their own
// script; drawing OURS is a takeover they did not agree to.
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
	// WaitDelay is what makes priorCommandTimeout an actual bound. Because stdout
	// is a buffer rather than a file, os/exec plumbs it through a pipe and Wait
	// blocks until every writer closes it — and a killed `sh` leaves GRANDCHILDREN
	// holding that pipe open. claude-hud is exactly that shape (sh -> node -> a
	// user extra-cmd), so without this the context can expire and the render still
	// blocks for as long as the deepest child runs. Measured: a 100ms timeout took
	// 30s to return.
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Run(); err != nil {
		// Prior command failed or timed out. Walk the ladder above — never our
		// own render, which would put promptster where their statusline was.
		if cached := loadStatuslineLastGood(rec.Prior.Command); len(cached) > 0 {
			return cached
		}
		if stdout.Len() > 0 {
			return stdout.Bytes()
		}
		return nil
	}
	out := stdout.Bytes()
	saveStatuslineLastGood(rec.Prior.Command, out)
	return out
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

// parseClaudeContextWindow lifts the session id and the vendor-reported context
// window from a status-line blob. observedAt is the tick time.
//
// ok=false unless BOTH are usable: a window with no session id cannot be joined
// to a transcript, and a session id with no window has nothing to carry. The
// window is dropped rather than coerced when it is non-finite or non-positive —
// absent is never 0 here, because a 0 window downstream is a division by zero or
// a session drawn as completely full, not a small one.
func parseClaudeContextWindow(blob []byte, observedAt int64) (string, claudeContextSpool, bool) {
	var in statuslineStdin
	if err := json.Unmarshal(blob, &in); err != nil {
		return "", claudeContextSpool{}, false
	}
	if in.SessionID == "" || in.ContextWindow == nil {
		return "", claudeContextSpool{}, false
	}
	size := in.ContextWindow.ContextWindowSize
	if size == nil || math.IsNaN(*size) || math.IsInf(*size, 0) || *size <= 0 {
		return "", claudeContextSpool{}, false
	}
	return in.SessionID, claudeContextSpool{
		ContextWindowTokens: int64(*size),
		ObservedAt:          observedAt,
	}, true
}
