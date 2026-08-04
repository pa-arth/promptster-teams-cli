package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/policy"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const claudeWatchInterval = 3 * time.Second

// claudeAccumFlushAge is how long an accumulated assistant message may sit
// without new lines before it is force-flushed (covers the final message of a
// turn when no boundary line follows promptly).
const claudeAccumFlushAge = 10 * time.Second

// claudeWatcherStaleAfter is how old the watcher heartbeat may be before hooks
// consider transcript capture dead and fall back to full hook emission. The
// watcher heartbeats every poll (3s), so 30s means ~10 missed polls.
const claudeWatcherStaleAfter = 30 * time.Second

// claudeDegradedByteThreshold: if the watcher consumes this many transcript
// bytes WITHOUT parsing a single event, the transcript format has likely
// changed under us — mark the watcher degraded so hooks resume full capture.
// Measured since the LAST parsed event (not session-total), so a mid-session
// format break is caught too, not just a never-worked parser. Set high enough
// that legitimately skipped content (file-history-snapshot lines can be
// large) between events doesn't flap the channel — and a false positive
// self-heals anyway via the recovery handoff in runClaudeWatcher.
const claudeDegradedByteThreshold = 256 * 1024

// claudeWatchMaxBytesPerPoll bounds how many transcript bytes ONE poll reads,
// across all matched transcripts combined — the byte analogue of
// gitWatchMaxCommitsPerPollTotal, and for the same reason.
//
// Ordinary tailing never approaches it: a 3s poll sees kilobytes. It exists for
// the bounded-history backfill, where the first poll after an install or an
// upgrade would otherwise read every in-window transcript to EOF in one
// synchronous pass. Three things break at that size, and the budget fixes all
// three because the loop's exit is what they all hang off:
//
//   - The signal select sits AFTER the poll, so `stop` and every supervisor
//     teardown block for the whole pass. Bounded poll, bounded shutdown.
//   - Offsets are only durable once saved. A SIGKILL escalation or a crash
//     mid-pass used to discard the entire pass's progress, so a restart loop
//     could never finish the backfill — it replayed from byte zero forever.
//     Progress is now saved after every file that consumed bytes.
//   - A single burst can outrun delivery and push the outbox to its cap, where
//     Append DROPS. See the outbox.UnderPressure check in pollClaudeTranscripts.
//
// Deferring by NOT advancing the offset is the whole point: the transcript file
// is itself the durable buffer, so a deferred read is never a lost read. 8 MiB
// per 3s poll drains a heavy 28-day history in a few minutes. A var, not a
// const, so a test can lower it.
var claudeWatchMaxBytesPerPoll int64 = 8 << 20

// transcriptMaxRecordBytes is the largest JSONL record the transcript readers
// support. A larger record cannot be parsed safely within one bounded poll, so
// it is discarded in bounded chunks rather than making every future poll reread
// the same prefix forever.
//
// It is the yardstick oversizedness is measured against — never the remaining
// per-poll budget, which depends on which files the walk reached first (see
// readTranscriptRecords), and never a scanner's own literal, which is what let
// CLASSIFICATION drift from the tail path and stall on a record the tail path
// would have escaped (see nextClassifyRecord). Both paths measure against THIS.
// A var, not a const, so a test can lower it.
var transcriptMaxRecordBytes int64 = 8 << 20

// claudeDegradationStep advances the degraded-detection state machine for one
// poll: any parsed event proves the parser works (reset); otherwise consumed
// bytes accumulate toward the threshold.
func claudeDegradationStep(degraded bool, parsed int, consumed, bytesSinceEvent int64) (bool, int64) {
	if parsed > 0 {
		return false, 0
	}
	bytesSinceEvent += consumed
	return degraded || bytesSinceEvent > claudeDegradedByteThreshold, bytesSinceEvent
}

// claudeConfigDir returns Claude Code's config root (CLAUDE_CONFIG_DIR or
// ~/.claude) — transcripts, skills, plugins, and settings all live under it.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// claudeProjectsDir returns where Claude Code writes per-session transcript
// JSONL files: <config>/projects/<munged-cwd>/<session-uuid>.jsonl.
func ClaudeProjectsDir() string {
	return filepath.Join(claudeConfigDir(), "projects")
}

// claudeWatcherState tracks the background transcript-tailing process.
type claudeWatcherState struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	// WatchDir is the workspace this watcher is scoped to. Recorded because it
	// decides which transcripts are captured, and a watcher started by the
	// autostart service writes no supervisor.json — without it, `status` has no
	// way to report the live scope and falls back to its own cwd, which is
	// routinely wrong.
	WatchDir      string `json:"watchDir,omitempty"`
	LogPath       string `json:"logPath,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	// EventsCaptured counts events PARSED and queued, not delivered — delivery
	// is asynchronous (internal/outbox) and may lag or retry. Formerly
	// "eventsSent", which stopped being true once sending moved off this loop.
	// It is an in-process counter re-derived from zero each run, so the tag
	// rename carries no compatibility concern.
	EventsCaptured int   `json:"eventsCaptured,omitempty"`
	BytesConsumed  int64 `json:"bytesConsumed,omitempty"`
	// Degraded means the watcher is running but parsing nothing from a
	// transcript it consumed substantial bytes from — treat as unhealthy.
	Degraded bool `json:"degraded,omitempty"`
}

func claudeWatcherStatePath() string { return filepath.Join(state.StateDir(), "claude-watcher.json") }
func claudeWatcherLogPath() string   { return filepath.Join(state.StateDir(), "claude-watcher.log") }

// claudeWatchProgress persists per-transcript byte offsets and the
// workspace-match decision so each line is processed exactly once across polls
// and watcher restarts.
//
// KEYED BY TRANSCRIPT IDENTITY, NOT PATH. Both maps are keyed by the path
// RELATIVE to the project-slug directory (claudeProgressKey), e.g.
// "<session-uuid>.jsonl" or "<session-uuid>/subagents/agent-<id>.jsonl".
//
// Absolute paths are NOT a stable identity: Claude Code files a transcript
// under a slug derived from the session's cwd, so one session is reachable
// under several slugs — a git-worktree slug
// (-Users-me-repo--claude-worktrees-x) and the bare repo slug (-Users-me-repo)
// — and the file moves between them when a worktree is removed. Path-keyed
// offsets treated each slug as a brand-new file, re-read it from 0, and
// re-emitted the whole transcript; that duplicate traffic (measured: 2,182
// events sent exactly twice, ~32% of all traffic) is what blew the ingest rate
// limit. Session UUIDs and agent IDs are globally unique, so slug-relative keys
// collapse every alias of one transcript onto ONE offset — portable, no inode
// syscalls, works on Windows.
type claudeWatchProgress struct {
	Offsets map[string]int64 `json:"offsets"`
	// Discarding marks offsets that are inside an unsupported oversized record.
	// It is durable so a restart cannot feed the remaining malformed suffix to
	// the parser and falsely trip degraded mode.
	Discarding map[string]bool `json:"discarding,omitempty"`
	// Classification uses separate durable cursors because a successful replay
	// still tails from byte zero. They bound oversized-prefix scans across polls
	// without losing the backfill offset.
	ClassifyOffsets    map[string]int64 `json:"classify_offsets,omitempty"`
	ClassifyDiscarding map[string]bool  `json:"classify_discarding,omitempty"`
	ClassifyScanned    map[string]int   `json:"classify_scanned,omitempty"`
	// Match: key -> "yes"|"no". Unlike codex rollouts (whose first line is
	// always session_meta), a transcript's early lines may not carry cwd yet,
	// so absence of a cached decision means "retry next poll" — only a
	// definitive cwd mismatch or a line-budget exhaustion caches "no".
	Match map[string]string `json:"match"`
	// V is the progress-file schema version. Bumped when a change to the
	// classification rules invalidates cached decisions; loadClaudeWatchProgress
	// runs a one-time migration when the stored V is behind claudeProgressSchemaV.
	V int `json:"v"`
	// RootsFP fingerprints the match ROOT SET the cached decisions were made
	// against. When it changes — a `start` registered another directory, a git
	// worktree appeared, or a root went away — every cached decision is dropped
	// and reclassified against the current set. Without it, a root-set change is
	// invisible to any transcript classified before it: widening would never
	// reach a cached "no", and narrowing would keep tailing a cached "yes" whose
	// directory is no longer watched.
	RootsFP string `json:"roots_fp"`
}

// claudeProgressSchemaV is the current progress-file schema version. v1 dropped
// stale timestamp-based "no" decisions. v2 reopens previously matched files so
// the new bounded history policy gets exactly one chance to replay the last 28
// days. Deterministic event ids make overlap with already-captured live tails
// idempotent at ingest.
const claudeProgressSchemaV = 2

func claudeWatchProgressPath() string {
	return filepath.Join(state.StateDir(), "claude-watcher-progress.json")
}

// claudeProgressKey reduces a transcript path to its slug-relative identity by
// stripping the <ClaudeProjectsDir()>/<project-slug>/ prefix. A path outside
// the projects dir (or with no slug component) has no such identity and falls
// back to itself, which is no worse than the old behavior.
func claudeProgressKey(path string) string {
	rel, err := filepath.Rel(ClaudeProjectsDir(), path)
	if err != nil {
		return path
	}
	slashed := filepath.ToSlash(rel)
	if slashed == ".." || strings.HasPrefix(slashed, "../") {
		return path // not under the projects dir
	}
	// rel is "<slug>/<rest>"; drop the slug — that is the whole point.
	parts := strings.SplitN(slashed, "/", 2)
	if len(parts) < 2 {
		return path // bare file directly in the projects dir: no slug to strip
	}
	return parts[1]
}

// migrateClaudeProgressKeys re-keys a progress file written by a build that
// keyed on absolute paths.
//
// On collision (the two-slug case this exists to fix — several old absolute
// keys folding onto one identity) it keeps the MAXIMUM offset. Keeping the
// minimum would re-read the difference and re-emit those events, which is
// precisely the duplicate storm being fixed; keeping the max means at worst a
// few never-parsed lines are skipped, which is the strictly safer error.
func migrateClaudeProgressKeys(p claudeWatchProgress) claudeWatchProgress {
	out := claudeWatchProgress{
		Offsets:            make(map[string]int64, len(p.Offsets)),
		Discarding:         make(map[string]bool, len(p.Discarding)),
		ClassifyOffsets:    make(map[string]int64, len(p.ClassifyOffsets)),
		ClassifyDiscarding: make(map[string]bool, len(p.ClassifyDiscarding)),
		ClassifyScanned:    make(map[string]int, len(p.ClassifyScanned)),
		Match:              make(map[string]string, len(p.Match)),
		V:                  p.V,
		RootsFP:            p.RootsFP,
	}
	// A ZERO VALUE IS NEVER STORED in the flag/counter maps. `omitempty` on a map
	// only drops it when the map is EMPTY, so writing an absent key's zero value
	// here serializes it, reloads it, and re-writes it on the next load — the
	// poll loop's deletes only cover the files it touched, so the padding is
	// permanent, self-restoring, and roughly doubles the key count of a progress
	// file the backfill rewrites repeatedly.
	for k, v := range p.ClassifyOffsets {
		nk := claudeProgressKey(k)
		if cur, ok := out.ClassifyOffsets[nk]; !ok || v > cur {
			out.ClassifyOffsets[nk] = v
			if p.ClassifyDiscarding[k] {
				out.ClassifyDiscarding[nk] = true
			} else {
				delete(out.ClassifyDiscarding, nk)
			}
		}
	}
	// ClassifyScanned is folded by MAXIMUM over every alias, which is why the
	// loop above deliberately does not seed it: a seed would be an unconditional
	// write, and the max fold already reaches every alias that has a value.
	for k, v := range p.ClassifyScanned {
		nk := claudeProgressKey(k)
		if v > out.ClassifyScanned[nk] {
			out.ClassifyScanned[nk] = v
		}
	}
	for k, v := range p.Offsets {
		nk := claudeProgressKey(k)
		if cur, ok := out.Offsets[nk]; !ok || v > cur {
			out.Offsets[nk] = v
			if p.Discarding[k] {
				out.Discarding[nk] = true
			} else {
				delete(out.Discarding, nk)
			}
		}
	}
	for k, v := range p.Match {
		nk := claudeProgressKey(k)
		// Aliases of one transcript have identical content and therefore
		// identical cwd, so a yes/no conflict is unreachable in practice. If it
		// somehow happens, "yes" wins: a wrong "no" is cached forever and
		// silently loses the whole session, while a wrong "yes" only costs a
		// re-read. Prefer the recoverable error.
		if cur, ok := out.Match[nk]; ok && cur == "yes" {
			continue
		}
		out.Match[nk] = v
	}
	return out
}

// clearClaudeClassifyState retires a transcript's classification cursor once
// its decision is settled — or abandons a partial pass so the next poll starts
// classification over with a full record budget.
//
// The three keys are one unit: the cursor is meaningless without the scan count
// that bounds it and the discard flag that says whether it points inside an
// unsupported record. Leaving any of them behind parks the cursor PAST the
// record that already decided the file while the budget stays partly spent, so
// a retry that finds no further cwd line caches "no" and drops the session for
// good.
func clearClaudeClassifyState(p claudeWatchProgress, key string) {
	delete(p.ClassifyOffsets, key)
	delete(p.ClassifyDiscarding, key)
	delete(p.ClassifyScanned, key)
}

func loadClaudeWatchProgress() claudeWatchProgress {
	p := claudeWatchProgress{
		Offsets: map[string]int64{}, Discarding: map[string]bool{}, Match: map[string]string{},
		ClassifyOffsets: map[string]int64{}, ClassifyDiscarding: map[string]bool{}, ClassifyScanned: map[string]int{},
	}
	data, err := os.ReadFile(claudeWatchProgressPath())
	if err != nil {
		// Nothing on disk means nothing to migrate: stamp the CURRENT schema so
		// the first save doesn't write v0 and make the next load re-run the
		// one-time migration on a fresh install (which drops the whole mismatch
		// cache for no reason, and would mask a broken RootsFP check by
		// rescanning anyway).
		p.V = claudeProgressSchemaV
		return p
	}
	_ = json.Unmarshal(data, &p)
	if p.Offsets == nil {
		p.Offsets = map[string]int64{}
	}
	if p.Match == nil {
		p.Match = map[string]string{}
	}
	if p.Discarding == nil {
		p.Discarding = map[string]bool{}
	}
	if p.ClassifyOffsets == nil {
		p.ClassifyOffsets = map[string]int64{}
	}
	if p.ClassifyDiscarding == nil {
		p.ClassifyDiscarding = map[string]bool{}
	}
	if p.ClassifyScanned == nil {
		p.ClassifyScanned = map[string]int{}
	}
	// Idempotent: already-relative keys map to themselves, so this runs
	// harmlessly on every load and needs no version flag.
	migrated := migrateClaudeProgressKeys(p)
	// v1: drop every cached "no" written by the old timestamp gate. Genuinely
	// cwd-mismatched files re-cache "no" on the next poll.
	if migrated.V < 1 {
		for k, v := range migrated.Match {
			if v == "no" {
				delete(migrated.Match, k)
			}
		}
	}
	// v2: old "yes" entries bypass classification, so merely resetting their
	// offsets would replay an arbitrarily old, recently-touched transcript. Drop
	// those decisions and offsets together: the timestamp gate will replay only
	// in-window sessions and seed older active sessions to EOF. Keep v1-era "no"
	// decisions because they are genuine cwd mismatches.
	if migrated.V < 2 {
		migrated.Offsets = map[string]int64{}
		migrated.Discarding = map[string]bool{}
		migrated.ClassifyOffsets = map[string]int64{}
		migrated.ClassifyDiscarding = map[string]bool{}
		migrated.ClassifyScanned = map[string]int{}
		for k, v := range migrated.Match {
			if v == "yes" {
				delete(migrated.Match, k)
			}
		}
	}
	migrated.V = claudeProgressSchemaV
	return migrated
}

func saveClaudeWatchProgress(p claudeWatchProgress) {
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	tmp := claudeWatchProgressPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, claudeWatchProgressPath())
}

func loadClaudeWatcherState() (claudeWatcherState, error) {
	data, err := os.ReadFile(claudeWatcherStatePath())
	if err != nil {
		return claudeWatcherState{}, err
	}
	var s claudeWatcherState
	if err := json.Unmarshal(data, &s); err != nil {
		return claudeWatcherState{}, err
	}
	return s, nil
}

func saveClaudeWatcherState(s claudeWatcherState) error {
	path := claudeWatcherStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// clearClaudeWatcherState drops the watcher's LIVENESS state — pid, heartbeat,
// log path — which is meaningless once the process is gone.
//
// It deliberately leaves claude-watcher-progress.json alone. Those offsets are
// the durable record of what has already been read, and the file's schema
// version is what grants the 28-day history replay exactly once. Deleting it on
// a clean exit made every restart — every login under autostart, every
// self-update re-exec (<=30m), every sleep/wake — re-read and re-ship the whole
// window. Deterministic event ids keep that idempotent at ingest, so the cost is
// invisible: repeated disk reads, repeated uploads, and repeated replay-stamped
// ledger writes with nothing to show for them. `uninstall --purge` removes the
// whole state dir, which is where discarding progress belongs.
func clearClaudeWatcherState() {
	_ = os.Remove(claudeWatcherStatePath())
}

func isClaudeWatcherRunning() (claudeWatcherState, bool) {
	st, err := loadClaudeWatcherState()
	if err != nil || st.PID <= 0 {
		return claudeWatcherState{}, false
	}
	if processExists(st.PID) {
		return st, true
	}
	clearClaudeWatcherState()
	return claudeWatcherState{}, false
}

// claudeWatcherHealthy reports whether transcript capture can be trusted RIGHT
// NOW: the daemon is alive, heartbeating, and not degraded. Hooks use this to
// decide between suppressing their overlapping events (watcher healthy) and
// full fallback emission (watcher dead/stale/degraded).
func claudeWatcherHealthy() bool {
	st, ok := isClaudeWatcherRunning()
	if !ok || st.Degraded {
		return false
	}
	hb, err := time.Parse(time.RFC3339Nano, st.LastHeartbeat)
	if err != nil {
		return false
	}
	return time.Since(hb) < claudeWatcherStaleAfter
}

// --- hook takeover marker ----------------------------------------------------
//
// When a hook emits an event the watcher would normally own (because the
// watcher was unhealthy at that moment), it touches this marker. A watcher
// that starts while the marker exists fast-forwards every matched transcript
// to EOF before tailing — otherwise it would replay lines whose events hooks
// already ingested, double-counting prompts and responses. The cost is losing
// per-request usage for the outage window (estimate becomes a slight
// undercount), which beats duplicated timeline events.

func claudeHookTakeoverPath() string { return filepath.Join(state.StateDir(), "claude-hook-takeover") }

func touchClaudeHookTakeover() {
	p := claudeHookTakeoverPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
}

// transcriptKeepKinds are hook-emitted kinds the transcript JSONL does NOT
// reliably carry — hooks keep emitting these even when the watcher is healthy.
// Everything else (prompt, ai_response, command, file_diff, file_read, ...)
// is owned by the watcher in transcript-capture mode.
var transcriptKeepKinds = map[string]bool{
	"session_start":  true,
	"session_end":    true,
	"tool_intent":    true,
	"subagent_start": true,
	"subagent_stop":  true,
	// context_compact moved to watcher ownership: the transcript's
	// system/compact_boundary line carries the auto-vs-manual trigger the
	// PreCompact hook path can't always see, so the watcher emits it now.
}

// suppressForTranscriptCapture decides whether a hook-captured Claude Code
// event should be dropped because the transcript watcher owns it. When the
// watcher is unhealthy the hook emits normally AND records a takeover marker,
// so a restarting watcher skips the lines hooks already covered instead of
// replaying them as duplicates.
func suppressForTranscriptCapture(session Session, e *event.Event) bool {
	if session.CaptureMode != "transcript" {
		return false
	}
	if e.Source != "claude-code" {
		return false
	}
	if transcriptKeepKinds[e.Kind] {
		return false
	}
	if claudeWatcherHealthy() {
		return true
	}
	touchClaudeHookTakeover()
	return false
}

// runClaudeWatcher is the main loop for the `promptster claude-watch`
// subcommand. It tails Claude Code transcript JSONL files whose recorded cwd
// is inside the workspace, normalizes each new line, and ingests the events.
func RunClaudeWatcher() error {
	session, err := loadSession()
	if err != nil {
		return fmt.Errorf("no active session: %w", err)
	}
	if session.TaskRoot == "" {
		return fmt.Errorf("session has no task root")
	}
	if st, ok := isClaudeWatcherRunning(); ok && st.PID != os.Getpid() {
		return fmt.Errorf("claude watcher already running (pid %d)", st.PID)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveClaudeWatcherState(claudeWatcherState{
		PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
		LogPath: claudeWatcherLogPath(), LastHeartbeat: now,
	}); err != nil {
		return err
	}
	defer clearClaudeWatcherState()

	if os.Getenv("PROMPTSTER_API_URL") == "" && session.ApiURL != "" {
		_ = os.Setenv("PROMPTSTER_API_URL", session.ApiURL)
	}

	workspace := resolvePath(session.TaskRoot)
	historyCutoff := transcriptHistoryCutoff(time.Now().UTC())

	// If hooks took over while no watcher was alive, skip everything already
	// on disk — those lines' events were emitted by the hook path.
	if _, err := os.Stat(claudeHookTakeoverPath()); err == nil {
		fastForwardClaudeTranscripts(workspace, historyCutoff)
		_ = os.Remove(claudeHookTakeoverPath())
		fmt.Fprintf(os.Stderr, "claude-watcher: hooks covered an outage window — fast-forwarded past existing transcript content\n")
	}

	// SIGTERM as well as SIGINT: `stop` sends SIGINT, but every supervisor-driven
	// teardown (launchctl bootout, systemctl --user stop) sends SIGTERM. Without
	// it registered, Go's default handler kills the process outright and the
	// deferred state cleanup below never runs, leaving stale pidfiles that make
	// `status` lie until the next liveness check heals them.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	client := &http.Client{Timeout: 5 * time.Second}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}
	eventsCaptured := 0
	var bytesConsumed, bytesSinceEvent int64
	degraded := false
	// Claude rate-limit *window* emission: drain the spool the statusline shim
	// writes (latest-wins) and emit the provider-agnostic windowUsage event. See
	// window_usage.go and statusline_shim.go.
	var windowEmitter claudeWindowEmitter

	// Delivery runs off the poll loop, so a slow or rate-limited backend can no
	// longer stall parsing, advance transcript offsets past undelivered events,
	// or (via the old send-derived count) masquerade as a broken parser.
	// StartDrain is a process-wide singleton — the codex watcher shares this
	// queue and calls it too (see its doc comment).
	outbox.StartDrain(client, session.SessionToken)

	// Org capture policy (opt-in assistant prose). Fail-closed: false until a
	// successful fetch says otherwise. Refreshed in the background (immediate +
	// every RefreshInterval) so the poll loop never blocks on the 15s-timeout
	// policy fetch; each iteration just reads the lock-guarded cached bool and
	// threads it into every projected event via ingestClaudeWatchEvent ->
	// AppendEventToLocalBuffer.
	policyResolver := policy.NewResolver(session.SessionToken)
	policyCtx, cancelPolicy := context.WithCancel(context.Background())
	defer cancelPolicy()
	policyResolver.StartBackground(policyCtx)

	if verboseWatch() {
		fmt.Fprintf(os.Stderr, "claude-watcher: started, polling %s every %s (workspace=%s)\n",
			ClaudeProjectsDir(), claudeWatchInterval, workspace)
	}

	for {
		captureProse := policyResolver.CaptureAssistantProse()
		// While degraded, hooks own emission — the watcher keeps PARSING (to
		// detect recovery and advance offsets) but discards events: hooks were
		// live for that window and already emitted them. The first poll that
		// parses again proves the parser works; from the NEXT poll the watcher
		// owns emission and hooks suppress again. This handoff means neither a
		// real mid-session format break nor a false-positive degradation can
		// double-emit or lose events.
		// Recomputed every poll so the window actually ROLLS. Frozen at boot it
		// is an absolute date, so a daemon up for 60 days would still admit an
		// 88-day-old transcript from byte zero.
		historyCutoff = transcriptHistoryCutoff(time.Now().UTC())
		parsed, consumed := pollClaudeTranscripts(session, workspace, historyCutoff, processors, degraded, captureProse)
		bytesConsumed += consumed
		windowEmitter.maybe(session, time.Now(), captureProse)
		wasDegraded := degraded
		degraded, bytesSinceEvent = claudeDegradationStep(degraded, parsed, consumed, bytesSinceEvent)
		switch {
		case wasDegraded && !degraded:
			_ = os.Remove(claudeHookTakeoverPath())
			fmt.Fprintf(os.Stderr, "claude-watcher: parser recovered — resuming event ownership (discarded %d hook-covered event(s))\n", parsed)
		case !wasDegraded && degraded:
			fmt.Fprintf(os.Stderr, "claude-watcher: degraded — %d bytes consumed since last parsed event; hooks take over\n", bytesSinceEvent)
		case !wasDegraded:
			eventsCaptured += parsed
		}

		_ = saveClaudeWatcherState(claudeWatcherState{
			PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
			LogPath:        claudeWatcherLogPath(),
			LastHeartbeat:  time.Now().UTC().Format(time.RFC3339Nano),
			EventsCaptured: eventsCaptured,
			BytesConsumed:  bytesConsumed,
			Degraded:       degraded,
		})

		select {
		case <-signals:
			// Drain the PROCESSORS' in-flight accumulators first. That is a
			// different thing from the outbox drain ruled out below, and the
			// distinction is the whole point: an accumulated assistant message
			// has not been QUEUED yet, so the outbox's at-least-once durability
			// cannot re-deliver it — exiting without this loses the final
			// message of every open transcript outright, along with its tokens.
			// Sidechain (subagent) usage is the most exposed, because it
			// accumulates to a message boundary and a subagent transcript's last
			// message often IS the boundary. Queuing is local (no network wait),
			// so unlike an outbox flush it adds no shutdown latency.
			for _, proc := range processors {
				for _, ev := range proc.FlushStale(0) {
					if degraded {
						continue // hooks owned this window and already emitted
					}
					eventsCaptured++
					queueClaudeWatchEvent(ev, session, captureProse)
				}
			}
			// Now return and let the drain die with the process. There is
			// deliberately no OUTBOX flush-on-exit: whatever is queued stays in
			// the outbox with its cursor unadvanced, and the next start delivers
			// it. Waiting here would only add shutdown latency to a SIGTERM
			// (which #49 now handles, and which supervisors follow with SIGKILL
			// on a timeout) to buy something durability already gives. Worst
			// case is an event POSTed but not yet cursor-committed, which is
			// re-sent once on restart — at-least-once, and the backend dedupes.
			fmt.Fprintf(os.Stderr, "claude-watcher: shutting down (captured %d events)\n", eventsCaptured)
			return nil
		case <-time.After(claudeWatchInterval):
		}
	}
}

// pollClaudeTranscripts scans for transcript files, tails matched ones from
// their stored byte offset, and QUEUES normalized events for async delivery.
// Returns (events parsed, bytes consumed).
//
// The returned count is events the PARSER produced — not events delivered.
// That distinction is the point: this number feeds claudeDegradationStep, whose
// job is to detect a broken PARSER. It used to return a send count, so a total
// network outage (429 storm, offline laptop) tripped a parser-break detector
// and handed capture to hooks, which only cover the live tail — the outage
// window then died twice. Sending is now the outbox's problem and cannot
// influence this number.
//
// With dryRun set (degraded mode), events are parsed and counted — proving the
// parser works — but NOT queued: hooks own emission for that window.
func pollClaudeTranscripts(
	session Session,
	workspace string,
	historyCutoff time.Time,
	processors map[string]*normalize.ClaudeTranscriptProcessor,
	dryRun bool,
	captureProse bool,
) (int, int64) {
	progress := loadClaudeWatchProgress()
	parsed := 0
	var consumed int64
	// Shared per-poll read budget (see claudeWatchMaxBytesPerPoll). Zeroed
	// outright while the queue is under pressure: the transcript bytes are
	// durable on disk and the offset has not moved, so declining to read defers
	// the work, whereas reading on would race the outbox to its cap where
	// Append DROPS — and take live capture down with the backfill.
	budget := claudeWatchMaxBytesPerPoll
	if outbox.UnderPressure() {
		budget = 0
	}
	deferredWork := false
	// Mid-poll checkpoints are throttled by BYTES CONSUMED, not by file count.
	// The durability rule is "a crash may replay at most one checkpoint's worth
	// of work", and bytes are what that rule is actually about; saving once per
	// drained file makes the poll's write volume O(files x progress-file size),
	// which during the backfill is tens of megabytes of whole-file rewrites
	// every interval. See transcriptProgressCheckpointBytes.
	//
	// ACCRUING AND FLUSHING ARE SEPARATE because a cursor advance and the
	// decision it produces are written at different moments, and only some of
	// the states in between survive a SIGKILL intact. accrue() records the read
	// as soon as it happens; checkpoint() is called only where what is in memory
	// is a state the next poll can resume from — cursor advanced with its
	// decision, cursor advanced with no decision and nothing consumed past it,
	// or cursor cleared. Flushing between an advance and its decision persists a
	// cursor parked PAST the record that already decided the file, which is the
	// state clearClaudeClassifyState exists to prevent.
	unsaved := int64(0)
	accrue := func(consumed int64) { unsaved += consumed }
	checkpoint := func() {
		if unsaved >= transcriptProgressCheckpointBytes {
			unsaved = 0
			saveClaudeWatchProgress(progress)
		}
	}
	// One oversized-record probe per poll: it is the only read allowed past the
	// shared budget, so granting it once keeps the poll's total work bounded
	// while making the escape independent of walk order.
	oversizeProbe := true
	roots := workspaceMatchRoots(workspace)
	// Any root-set change must revalidate cached decisions: widening can admit a
	// prior mismatch, while narrowing must revoke a prior match. See RootsFP.
	if fp, dropped, changed := syncMatchCacheToRoots(progress.Match, progress.RootsFP, roots); changed {
		if dropped > 0 {
			fmt.Fprintf(os.Stderr, "claude-watcher: capture roots changed — re-checking %d cached transcript(s)\n", dropped)
		}
		progress.RootsFP = fp
		saveClaudeWatchProgress(progress)
	}

	for _, path := range candidateClaudeTranscripts(historyCutoff) {
		key := claudeProgressKey(path)
		switch progress.Match[key] {
		case "no":
			continue
		case "yes":
			// proceed to tail
		default:
			if budget <= 0 {
				deferredWork = true
				continue
			}
			remaining := budget
			classifyBudget := budget
			if oversizeProbe && classifyBudget < transcriptMaxRecordBytes {
				classifyBudget = transcriptMaxRecordBytes
			}
			match, res, scanned := classifyClaudeTranscriptBounded(
				path, roots, historyCutoff,
				progress.ClassifyOffsets[key], progress.ClassifyScanned[key], classifyBudget,
				oversizeProbe, progress.ClassifyDiscarding[key],
			)
			progress.ClassifyOffsets[key] += res.consumed
			progress.ClassifyScanned[key] = scanned
			if res.discardingOversize {
				progress.ClassifyDiscarding[key] = true
			} else {
				delete(progress.ClassifyDiscarding, key)
			}
			budget -= res.consumed
			if res.probedOversize || res.consumed > remaining {
				oversizeProbe = false
			}
			accrue(res.consumed)
			switch match {
			case claudeMatchYes:
				progress.Match[key] = "yes"
			case claudeMatchYesPreexisting:
				// Go-forward: capture ongoing activity but not out-of-window
				// history. Seed the offset to current EOF so tailing starts at new
				// content. Only when unseen — a real prior offset must be preserved.
				// If the stat fails transiently, DON'T cache "yes" yet: leave the
				// match undecided and retry next poll, so a later success seeds EOF
				// instead of tailing the whole old file from offset 0. The retry has
				// to start classification OVER — see clearClaudeClassifyState.
				if _, ok := progress.Offsets[key]; !ok {
					info, err := os.Stat(path)
					if err != nil {
						clearClaudeClassifyState(progress, key)
						checkpoint()
						continue
					}
					progress.Offsets[key] = info.Size()
				}
				progress.Match[key] = "yes"
			case claudeMatchNo:
				progress.Match[key] = "no"
			default: // undecided — no cwd line yet; retry next poll
				checkpoint()
				continue
			}
			clearClaudeClassifyState(progress, key)
			checkpoint()
			if match == claudeMatchNo {
				continue
			}
		}

		if budget <= 0 {
			// Budget spent: leave this transcript's offset untouched so every
			// unread byte re-surfaces next poll. Deferred, never dropped. Skipped
			// before the processor is built, because building one shells out to
			// git for the repo identity.
			deferredWork = true
			continue
		}

		// Keyed by identity, like the offsets: two slugs of one transcript must
		// share a processor, or the second alias would accumulate a partial
		// assistant message against half the lines.
		proc := processors[key]
		if proc == nil {
			proc = normalize.NewClaudeTranscriptProcessor(claudeSessionIDFromPath(path))
			if isClaudeSidechainFile(path) {
				proc.UsageOnly = true
				// The filename is the floor for sidechain attribution: rows
				// usually repeat it (plus skill/agent names), but agentId must
				// survive even if they don't.
				proc.AgentID = claudeAgentIDFromPath(path)
			} else {
				// Resolve the canonical repo identity ONCE per session (this
				// processor is created once per transcript) from the transcript's
				// recorded cwd, and thread it in as session state so each prompt
				// event carries repoRoot + repoHost + repoTracked. Sidechains emit
				// no prompts, so they skip it. transcriptCwd reads only the cwd
				// field, never the body. All three parts come from ONE call so the
				// host and the tracked bit can never be stamped from a different
				// resolution pass than the slug — they describe one observation of
				// one directory, and a second pass could see it after a `git init`.
				proc.RepoRoot, proc.RepoHost, proc.RepoTracked = sessionRepoIdentity(transcriptCwd(path))
			}
			processors[key] = proc
		}
		n, res := tailClaudeTranscript(path, progress, proc, session, dryRun, captureProse, budget, oversizeProbe)
		parsed += n
		// Discarded bytes decrement the SCHEDULING budget (they were read) but
		// are withheld from the returned total, which feeds the parser-health
		// detector: a record no parser was ever offered says nothing about
		// whether the parser works.
		consumed += res.consumed - res.discarded
		budget -= res.consumed
		if res.probedOversize {
			oversizeProbe = false
		}
		if res.truncated {
			deferredWork = true
		}
		// Checkpoint mid-poll, not only at the end: an unsaved offset is replayed
		// from byte zero after a SIGKILL or a crash, which is what makes a restart
		// loop unable to ever finish a long backfill.
		accrue(res.consumed)
		checkpoint()
	}

	// Force-flush assistant messages that stopped receiving lines (turn ended
	// without a prompt boundary yet).
	for _, proc := range processors {
		for _, ev := range proc.FlushStale(claudeAccumFlushAge) {
			parsed++
			if dryRun {
				continue
			}
			queueClaudeWatchEvent(ev, session, captureProse)
		}
	}

	if deferredWork && verboseWatch() {
		fmt.Fprintf(os.Stderr, "claude-watcher: per-poll read budget spent (%d bytes) — remaining transcript history deferred to the next poll\n",
			claudeWatchMaxBytesPerPoll)
	}

	saveClaudeWatchProgress(progress)
	return parsed, consumed
}

// candidateClaudeTranscripts lists transcript files modified at/after the
// cutoff. Subagent transcripts (<session>/subagents/agent-*.jsonl) are
// included — their token usage is real spend — but processed in UsageOnly
// mode (see isClaudeSidechainFile): their "user" messages are agent-authored
// prompts that must not enter the candidate's timeline.
func candidateClaudeTranscripts(historyCutoff time.Time) []string {
	var out []string
	_ = filepath.Walk(ClaudeProjectsDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(filepath.Base(path), ".jsonl") {
			return nil
		}
		if info.ModTime().Before(historyCutoff) {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out
}

// isClaudeSidechainFile reports whether a transcript path is a subagent
// sidechain file (usage-only capture).
func isClaudeSidechainFile(path string) bool {
	return strings.HasPrefix(filepath.Base(path), "agent-") ||
		filepath.Base(filepath.Dir(path)) == "subagents"
}

// claudeAgentIDFromPath derives the sidechain's agent id from its filename
// (<session>/subagents/agent-<id>.jsonl → <id>).
func claudeAgentIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return strings.TrimPrefix(base, "agent-")
}

// claudeSessionIDFromPath derives the OWNING session uuid from a transcript
// path, so a processor knows its session before it has read a single line (an
// event stamped before then would land in a shared "unknown" chain).
//
// The two shapes differ in where the uuid lives:
//
//	<slug>/<session-uuid>.jsonl                        → the filename
//	<slug>/<session-uuid>/subagents/agent-<id>.jsonl   → the GRANDPARENT dir
//
// A subagent's own filename is its agent id, not a session — taking the
// grandparent is what rolls subagent work up to the session that spawned it,
// rather than fragmenting each subagent into a phantom session of its own.
// Every subagent row also carries the parent's sessionId in content, which the
// normalizer uses as a fallback; the two agree.
func claudeSessionIDFromPath(path string) string {
	if filepath.Base(filepath.Dir(path)) == "subagents" {
		return filepath.Base(filepath.Dir(filepath.Dir(path)))
	}
	if isClaudeSidechainFile(path) {
		// agent-*.jsonl outside a subagents/ dir: shape we don't recognise, so
		// let the normalizer fall back to the transcript's own sessionId rather
		// than guess a parent from the path.
		return ""
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

type claudeMatchResult int

const (
	claudeMatchUndecided      claudeMatchResult = iota
	claudeMatchYes                              // matched; inside the history window — tail from the start
	claudeMatchYesPreexisting                   // matched; older than the history window — capture go-forward from EOF
	claudeMatchNo
)

// workspaceMatchRoots returns every directory whose transcripts this capture
// session owns: the daemon's own workspace, every directory registered by a
// later `promptster-teams start` (see capture_roots.go), and every git worktree
// registered to any of their repositories.
//
// The worktree expansion is why a developer who parallelizes with
// `git worktree add ../fix` still gets captured — those claude processes have a
// cwd OUTSIDE the workspace directory. The registered roots are why a second
// tree that shares nothing with the workspace gets captured at all: one lock
// per user account means one daemon, so the second tree can only be reached by
// widening this list.
//
// Re-read every poll (both the file and the worktree lists) so a directory
// registered mid-session is picked up without restarting the daemon.
func workspaceMatchRoots(workspace string) []string {
	seeds := []string{workspace}
	seeds = append(seeds, RegisteredCaptureRoots()...)

	roots := make([]string, 0, len(seeds))
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		roots = append(roots, p)
	}
	for _, seed := range seeds {
		add(seed)
		// #nosec G204 -- constant argv; seed is this install's own capture root, not attacker input. Reads only the local worktree list.
		out, err := exec.Command("git", "-C", seed, "worktree", "list", "--porcelain").Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "worktree ") {
				continue
			}
			add(resolvePath(strings.TrimSpace(strings.TrimPrefix(line, "worktree "))))
		}
	}
	return roots
}

// claudeClassifyMaxScanLines bounds how many records either classifier examines
// before concluding a cwd-less file is not a session transcript. It is package
// scope for the same reason transcriptMaxRecordBytes is: the bounded poll-path
// classifier and its convenience sibling must not drift on it.
//
// The two do count it slightly differently, and deliberately.
// readTranscriptRecords never hands a blank or oversized record to the bounded
// classifier's callback, so those cost cursor bytes but no scan credit, whereas
// nextClassifyRecord returns them to the unbounded pass as empty records that
// do spend credit. Neither can run without end, which is all the bound is for.
const claudeClassifyMaxScanLines = 50

// classifyClaudeTranscript decides whether a transcript belongs to this
// capture session by scanning its first lines for one carrying cwd and matching
// it against the workspace or any of its registered worktrees. Early lines
// (mode, permission-mode, ...) often lack cwd, so a file with no cwd yet stays
// undecided rather than being cached as a mismatch.
//
// cwd is authoritative. The timestamp admits sessions in the bounded history
// window from byte zero; older matched sessions are returned as
// claudeMatchYesPreexisting and captured go-forward from current EOF.
func classifyClaudeTranscript(path string, roots []string, historyCutoff time.Time) claudeMatchResult {
	// #nosec G304 -- path is a Claude transcript discovered under ~/.claude/projects by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return claudeMatchUndecided
	}
	defer f.Close()

	// A record over the supported maximum is skipped, not stalled on: see
	// nextClassifyRecord. It still counts toward the line budget below, so the
	// pass stays bounded by the same records it always was.
	reader := newClassifyReader(f)
	scanned := 0
	for {
		line, ok := nextClassifyRecord(reader)
		if !ok {
			break
		}
		scanned++
		if scanned > claudeClassifyMaxScanLines {
			// A real session writes a cwd-bearing line within the first
			// prompt; a long cwd-less file is not a session transcript.
			return claudeMatchNo
		}
		var rec struct {
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Cwd == "" {
			continue
		}
		resolved := resolvePath(rec.Cwd)
		matched := false
		for _, root := range roots {
			if pathWithin(resolved, root) {
				matched = true
				break
			}
		}
		if !matched {
			return claudeMatchNo
		}
		// Replay in-window history from the start. Anything older — INCLUDING a
		// session whose age cannot be established — remains eligible for
		// go-forward capture without uploading its historical prefix. The gate
		// fails CLOSED because mtime is the only other bound left: a session
		// started six months ago and resumed today has today's mtime, so an
		// absent or unparseable first timestamp would otherwise replay its entire
		// history. Seeding to EOF undercounts; admitting it re-uploads months.
		t, err := time.Parse(time.RFC3339, rec.Timestamp)
		if rec.Timestamp == "" || err != nil || t.Before(historyCutoff) {
			return claudeMatchYesPreexisting
		}
		return claudeMatchYes
	}
	// EOF without a cwd line: file just created, still growing — retry later.
	return claudeMatchUndecided
}

// classifyClaudeTranscriptBounded is the poll-path classifier. Unlike the
// convenience classifier above, it advances a separate durable cursor through
// at most budget bytes and records whether that cursor ends inside an
// unsupported record. Replay offsets remain untouched, so a successful match
// still backfills from byte zero.
func classifyClaudeTranscriptBounded(
	path string,
	roots []string,
	historyCutoff time.Time,
	offset int64,
	scanned int,
	budget int64,
	oversizeProbe bool,
	discarding bool,
) (claudeMatchResult, transcriptReadOutcome, int) {
	// #nosec G304 -- candidate path discovered under the Claude projects dir.
	f, err := os.Open(path)
	if err != nil {
		return claudeMatchUndecided, transcriptReadOutcome{}, scanned
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return claudeMatchUndecided, transcriptReadOutcome{}, scanned
	}

	result := claudeMatchUndecided
	out := readTranscriptRecords(f, budget, oversizeProbe, discarding, func(line []byte) bool {
		if result != claudeMatchUndecided {
			return false
		}
		scanned++
		if scanned > claudeClassifyMaxScanLines {
			result = claudeMatchNo
			return false
		}
		var rec struct {
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Cwd == "" {
			return true
		}
		resolved := resolvePath(rec.Cwd)
		matched := false
		for _, root := range roots {
			if pathWithin(resolved, root) {
				matched = true
				break
			}
		}
		if !matched {
			result = claudeMatchNo
			return false
		}
		t, err := time.Parse(time.RFC3339, rec.Timestamp)
		if rec.Timestamp == "" || err != nil || t.Before(historyCutoff) {
			result = claudeMatchYesPreexisting
			return false
		}
		result = claudeMatchYes
		return false
	})
	return result, out, scanned
}

// fastForwardClaudeTranscripts sets every currently-matched transcript's
// offset to its current size, so tailing resumes at content written from now
// on. Used after a hook-takeover window.
func fastForwardClaudeTranscripts(workspace string, historyCutoff time.Time) {
	progress := loadClaudeWatchProgress()
	roots := workspaceMatchRoots(workspace)
	for _, path := range candidateClaudeTranscripts(historyCutoff) {
		key := claudeProgressKey(path)
		if progress.Match[key] == "no" {
			continue
		}
		if progress.Match[key] != "yes" {
			if classifyClaudeTranscript(path, roots, historyCutoff) != claudeMatchYes {
				continue
			}
			progress.Match[key] = "yes"
		}
		// Aliases of one transcript share a key, so take the furthest EOF —
		// same MAX rule as the key migration, and for the same reason: a lower
		// offset would re-read and re-emit.
		if info, err := os.Stat(path); err == nil && info.Size() > progress.Offsets[key] {
			progress.Offsets[key] = info.Size()
		}
	}
	saveClaudeWatchProgress(progress)
}

// tailClaudeTranscript reads new complete lines from path starting at the
// stored offset, processes them, queues resulting events, and advances the
// offset. A trailing partial line is left for the next poll. Returns the number
// of events parsed plus the shared read outcome (see readTranscriptRecords,
// which both rails go through so their stop conditions cannot drift).
//
// budget caps the bytes THIS call may read, so one enormous transcript cannot
// consume a whole poll on its own (the poll-wide cap alone would not stop it —
// the first file would spend the entire budget before the loop could check).
// Stopping early is safe for exactly the reason the offset advance is: the
// unread bytes stay on disk and the offset stops short of them, so the next
// poll resumes at the byte after the last complete line processed.
//
// oversizeProbe carries the poll's single allowance to read past that budget,
// and only to establish that one record exceeds transcriptMaxRecordBytes.
//
// Advancing the offset unconditionally is now SAFE, which it was not before.
// This loop used to POST inline and advance regardless of the result, so a
// 429/5xx/timeout silently and permanently destroyed the event — there was no
// retry anywhere in the CLI. Durability now lives in the outbox: once an event
// is queued it will be delivered or loudly dropped, so the transcript offset no
// longer has to double as a delivery receipt.
func tailClaudeTranscript(
	path string,
	progress claudeWatchProgress,
	proc *normalize.ClaudeTranscriptProcessor,
	session Session,
	dryRun bool,
	captureProse bool,
	budget int64,
	oversizeProbe bool,
) (int, transcriptReadOutcome) {
	// #nosec G304 -- path is a Claude transcript discovered under ~/.claude/projects by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return 0, transcriptReadOutcome{}
	}
	defer f.Close()

	key := claudeProgressKey(path)
	offset := progress.Offsets[key]
	// Seeking past EOF is not an error, so a shorter alias of an
	// already-tailed transcript reads nothing and contributes nothing. That is
	// exactly the desired outcome — the alias is not new content.
	if _, err := f.Seek(offset, 0); err != nil {
		return 0, transcriptReadOutcome{}
	}

	parsed := 0
	wasDiscarding := progress.Discarding[key]
	res := readTranscriptRecords(f, budget, oversizeProbe, wasDiscarding, func(record []byte) bool {
		// Scrub secrets BEFORE parsing and before anything is persisted or
		// queued — transcript lines carry prompt text, command output, and file
		// content. This ordering is load-bearing; do not move it.
		redacted := redact.RedactBytes(record)
		for _, ev := range proc.Process(redacted) {
			parsed++
			if dryRun {
				continue
			}
			queueClaudeWatchEvent(ev, session, captureProse)
		}
		return true
	})

	if res.discarded > 0 {
		fmt.Fprintf(os.Stderr, "claude-watcher: discarded %d bytes of an unsupported record (over %d bytes) in %s\n",
			res.discarded, transcriptMaxRecordBytes, filepath.Base(path))
	}
	if res.consumed > 0 {
		progress.Offsets[key] = offset + res.consumed
		if parsed > 0 && verboseWatch() {
			fmt.Fprintf(os.Stderr, "claude-watcher: queued %d event(s) from %s\n", parsed, filepath.Base(path))
		}
	}
	if res.discardingOversize {
		progress.Discarding[key] = true
	} else {
		delete(progress.Discarding, key)
	}
	return parsed, res
}

// queueClaudeWatchEvent runs the shared per-event funnel: stamp device
// identity, path relativize, cross-channel file_diff dedup, local signed
// ledger, then the send queue.
//
// Ordering here is load-bearing twice over, and the two rules compose:
//
//  1. The DeviceID stamp MUST precede the ledger append — but NOT because of
//     signing. BuildSigningMessage covers version/id/sessionId/ts/kind/source/
//     v/dataHash/prevSig only; DeviceID is deliberately outside the signature,
//     so stamping it later would still verify. The real reason is that
//     AppendEventToLocalBuffer writes the LEDGER copy: stamp afterwards and the
//     audit trail records an event with no device while the wire copy has one.
//     Verified: stamping after leaves deviceId absent from buffer.jsonl and
//     present in the outbox — the two disagreeing about who produced the event,
//     in the one artifact whose whole job is to be trustworthy.
//  2. The ledger append MUST precede the queue append. It applies
//     source-exclusion + secret scrubbing and mutates ev in place with
//     Sig/PrevSig, so queuing afterwards enqueues exactly the projected,
//     redacted, signed bytes the backend should receive. Queue first and you
//     would ship an unsigned, unprojected event — a source leak.
//
// Device identity is stamped at this funnel rather than inside the normalizer:
// the normalizer's job is to read a transcript, and it has no business knowing
// what machine it runs on. SessionID comes from the transcript; DeviceID comes
// from the environment. Keeping the two sourced separately is what stops them
// collapsing back into one value.
func queueClaudeWatchEvent(ev event.Event, session Session, captureProse bool) {
	ev.DeviceID = session.DeviceID
	normalize.RelativizeEventPaths(&ev, session.TaskRoot)
	// Record AI bash execution windows for later commit-attribution recovery
	// (a `sed -i`/codegen edit produces no file_diff, so its paths never enter
	// the ai-paths ledger — this is the only signal we keep for them). No-op for
	// anything that is not an AI-attributed `command` event.
	// Claude Code stamps every transcript record with its own timestamp, so age
	// here genuinely means age and the bounded-history replay is separable from
	// the live tail on a per-event basis. A resumed session mixes both inside
	// one tail, which is why this is decided per event rather than per file.
	replay := transcriptEventIsHistorical(&ev)
	recordAiBashWindow(&ev, session.TaskRoot, replay)
	// Cross-channel idempotency: skip a file_diff whose resulting content the
	// hook or git watcher already emitted.
	if !dedupeFileDiff(session.TaskRoot, &ev, replay) {
		return
	}
	if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
		fmt.Fprintf(os.Stderr, "claude-watcher: buffer error: %v\n", err)
	}
	if err := outbox.Append(ev); err != nil {
		fmt.Fprintf(os.Stderr, "claude-watcher: queue error (%s): %v\n", ev.Kind, err)
	}
}
