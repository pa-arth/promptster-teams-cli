package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
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

const codexWatchInterval = 3 * time.Second

// codexWatchMaxBytesPerPoll bounds how many rollout bytes ONE poll reads across
// all matched rollouts combined. Mirrors claudeWatchMaxBytesPerPoll — see that
// comment for why a bounded poll is what keeps shutdown responsive, keeps
// backfill progress durable across a SIGKILL, and keeps a first-boot burst from
// racing the outbox to the cap where Append drops. A var so a test can lower it.
var codexWatchMaxBytesPerPoll int64 = 8 << 20

// codexHome returns the codex state dir (CODEX_HOME or ~/.codex), where session
// rollout files live under sessions/YYYY/MM/DD/rollout-*.jsonl.
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// codexRolloutSessionID matches the uuid Codex tails onto a rollout filename:
// rollout-2026-06-11T11-24-52-019eb780-3081-7ce0-9ba0-8a0bad13b532.jsonl. The
// leading timestamp also contains dashes, so anchor on the uuid shape at the
// end rather than splitting on "-".
var codexRolloutSessionID = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// codexSessionIDFromPath derives the rollout's THREAD uuid from its filename,
// so a processor has an id before reading a line. It equals
// session_meta.payload.id (verified).
//
// It is a SEED, not the answer: one rollout file is one Codex thread, and since
// 0.145 one conversation spans several threads (the user's, plus one per
// subagent it delegates to). The filename cannot tell them apart — only
// session_meta carries the conversation root — so normalize.CodexConversationID
// overrides this on line 1 whenever the rollout names a root other than its own
// thread. Trusting the filename alone is what fragmented one hour of one
// engineer's work into a session of prompts and six sessions of orphaned work.
func codexSessionIDFromPath(path string) string {
	if m := codexRolloutSessionID.FindStringSubmatch(filepath.Base(path)); m != nil {
		return m[1]
	}
	return ""
}

func codexSessionsDir() string {
	return filepath.Join(codexHome(), "sessions")
}

// codexWatcherState tracks the background rollout-tailing process.
type codexWatcherState struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	// WatchDir is the workspace this watcher is scoped to — see the field of the
	// same name on claudeWatcherState for why it is recorded.
	WatchDir      string `json:"watchDir,omitempty"`
	LogPath       string `json:"logPath,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	// EventsCaptured counts events parsed and queued, not delivered — delivery
	// is asynchronous (internal/outbox). In-process counter, re-derived from
	// zero each run, so renaming the tag breaks no compatibility.
	EventsCaptured int `json:"eventsCaptured,omitempty"`
}

func codexWatcherStatePath() string { return filepath.Join(state.StateDir(), "codex-watcher.json") }
func codexWatcherLogPath() string   { return filepath.Join(state.StateDir(), "codex-watcher.log") }

// codexWatchProgress persists per-rollout-file byte offsets and the
// workspace-match decision so each line is processed exactly once across polls.
type codexWatchProgress struct {
	Offsets map[string]int64 `json:"offsets"`
	// Match: path -> "yes"|"no" classification cache so we only read+parse a
	// file's session_meta header once.
	Match map[string]string `json:"match"`
	// V is the progress-file schema version. Bumped when a change to the
	// classification rules invalidates cached decisions; loadCodexWatchProgress
	// runs a one-time migration when the stored V is behind codexProgressSchemaV.
	V int `json:"v"`
	// RootsFP fingerprints the match ROOT SET the cached decisions were made
	// against; a change drops every cached "no" so a widened set reaches files
	// already judged mismatches. Mirrors claudeWatchProgress.RootsFP.
	RootsFP string `json:"roots_fp"`
}

// codexProgressSchemaV is the current progress-file schema version. v1 dropped
// stale timestamp-based "no" decisions. v2 reopens previously matched files so
// the new bounded history policy gets exactly one chance to replay the last 28
// days. Mirrors claudeProgressSchemaV.
const codexProgressSchemaV = 2

func codexWatchProgressPath() string {
	return filepath.Join(state.StateDir(), "codex-watcher-progress.json")
}

func loadCodexWatchProgress() codexWatchProgress {
	p := codexWatchProgress{Offsets: map[string]int64{}, Match: map[string]string{}}
	data, err := os.ReadFile(codexWatchProgressPath())
	if err != nil {
		// Nothing on disk means nothing to migrate: stamp the CURRENT schema so
		// the first save doesn't write v0 and make the next load re-run the
		// one-time migration on a fresh install (which drops the whole mismatch
		// cache for no reason, and would mask a broken RootsFP check by
		// rescanning anyway).
		p.V = codexProgressSchemaV
		return p
	}
	_ = json.Unmarshal(data, &p)
	if p.Offsets == nil {
		p.Offsets = map[string]int64{}
	}
	if p.Match == nil {
		p.Match = map[string]string{}
	}
	// v1: drop cached "no" decisions written by the old timestamp gate.
	if p.V < 1 {
		for k, v := range p.Match {
			if v == "no" {
				delete(p.Match, k)
			}
		}
	}
	// v2: reset old matched offsets so classification can replay only the new
	// bounded window. Preserve genuine cwd mismatches from v1.
	if p.V < 2 {
		p.Offsets = map[string]int64{}
		for k, v := range p.Match {
			if v == "yes" {
				delete(p.Match, k)
			}
		}
	}
	p.V = codexProgressSchemaV
	return p
}

func saveCodexWatchProgress(p codexWatchProgress) {
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	tmp := codexWatchProgressPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, codexWatchProgressPath())
}

func loadCodexWatcherState() (codexWatcherState, error) {
	data, err := os.ReadFile(codexWatcherStatePath())
	if err != nil {
		return codexWatcherState{}, err
	}
	var s codexWatcherState
	if err := json.Unmarshal(data, &s); err != nil {
		return codexWatcherState{}, err
	}
	return s, nil
}

func saveCodexWatcherState(s codexWatcherState) error {
	path := codexWatcherStatePath()
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

// clearCodexWatcherState drops only the watcher's liveness state; the durable
// rollout offsets survive a clean exit so the bounded history window is
// backfilled once rather than at every restart. See clearClaudeWatcherState for
// the full reasoning — both watchers hold the identical invariant.
func clearCodexWatcherState() {
	_ = os.Remove(codexWatcherStatePath())
}

func isCodexWatcherRunning() (codexWatcherState, bool) {
	st, err := loadCodexWatcherState()
	if err != nil || st.PID <= 0 {
		return codexWatcherState{}, false
	}
	if processExists(st.PID) {
		return st, true
	}
	clearCodexWatcherState()
	return codexWatcherState{}, false
}

// runCodexWatcher is the main loop for the `promptster codex-watch` subcommand.
// It tails codex rollout JSONL files whose recorded cwd is inside the workspace,
// normalizes each new line, and ingests the resulting events.
func RunCodexWatcher() error {
	session, err := loadSession()
	if err != nil {
		return fmt.Errorf("no active session: %w", err)
	}
	if session.TaskRoot == "" {
		return fmt.Errorf("session has no task root")
	}
	if st, ok := isCodexWatcherRunning(); ok && st.PID != os.Getpid() {
		return fmt.Errorf("codex watcher already running (pid %d)", st.PID)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveCodexWatcherState(codexWatcherState{
		PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
		LogPath: codexWatcherLogPath(), LastHeartbeat: now,
	}); err != nil {
		return err
	}
	defer clearCodexWatcherState()

	if os.Getenv("PROMPTSTER_API_URL") == "" && session.ApiURL != "" {
		_ = os.Setenv("PROMPTSTER_API_URL", session.ApiURL)
	}

	// Resolve the workspace path through symlinks once (macOS /tmp -> /private/tmp)
	// so cwd comparison against rollout session_meta is reliable.
	workspace := resolvePath(session.TaskRoot)

	// SIGTERM as well as SIGINT — see the matching note in RunClaudeWatcher.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	client := &http.Client{Timeout: 5 * time.Second}
	// One processor per rollout file, kept in memory for the daemon's life so
	// tool-call/output correlation survives across polls.
	processors := map[string]*normalize.CodexRolloutProcessor{}
	eventsCaptured := 0
	// Codex rate-limit *window* emission: a throttled scan of the rollout logs
	// for the latest token_count.rate_limits, mapped to the provider-agnostic
	// windowUsage event. Independent of the per-line capture above (window state
	// is account-global, not workspace-scoped). See window_usage.go.
	var windowEmitter codexWindowEmitter

	// Delivery runs off the poll loop — see the claude watcher for the full
	// rationale. Both watchers share ONE device-wide queue and run as goroutines
	// in the same supervisor process, so StartDrain is a process-wide singleton:
	// whichever watcher gets there first starts the only drain, and it delivers
	// both watchers' events.
	outbox.StartDrain(client, session.SessionToken)

	// Org capture policy (opt-in assistant prose), fail-closed. Refreshed in the
	// background (immediate + every RefreshInterval) so the poll loop never
	// blocks on the 15s-timeout policy fetch; each iteration reads the
	// lock-guarded cached bool and threads it into every projected event via
	// tailCodexRollout -> AppendEventToLocalBuffer.
	policyResolver := policy.NewResolver(session.SessionToken)
	policyCtx, cancelPolicy := context.WithCancel(context.Background())
	defer cancelPolicy()
	policyResolver.StartBackground(policyCtx)

	if verboseWatch() {
		fmt.Fprintf(os.Stderr, "codex-watcher: started, polling %s every %s (workspace=%s)\n",
			codexSessionsDir(), codexWatchInterval, workspace)
	}

	for {
		captureProse := policyResolver.CaptureAssistantProse()
		// Backfill the same bounded history the product visualizes. The rollout's
		// session_meta timestamp, not file mtime, is the authoritative age gate.
		// Recomputed every poll so the window actually ROLLS; frozen at boot it
		// is an absolute date a long-lived daemon drifts away from.
		historyCutoff := transcriptHistoryCutoff(time.Now().UTC())
		queued := pollCodexRollouts(session, workspace, historyCutoff, processors, captureProse)
		eventsCaptured += queued
		windowEmitter.maybe(session, time.Now(), captureProse)

		_ = saveCodexWatcherState(codexWatcherState{
			PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
			LogPath:       codexWatcherLogPath(),
			LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano), EventsCaptured: eventsCaptured,
		})

		select {
		case <-signals:
			fmt.Fprintf(os.Stderr, "codex-watcher: shutting down (captured %d events)\n", eventsCaptured)
			return nil
		case <-time.After(codexWatchInterval):
		}
	}
}

// pollCodexRollouts scans for rollout files, tails matched ones from their last
// byte offset, and ingests normalized events. Returns the number sent.
func pollCodexRollouts(
	session Session,
	workspace string,
	historyCutoff time.Time,
	processors map[string]*normalize.CodexRolloutProcessor,
	captureProse bool,
) int {
	dir := codexSessionsDir()
	progress := loadCodexWatchProgress()
	sent := 0
	// Shared per-poll read budget; zeroed while the queue is under pressure so
	// the backfill defers instead of pushing the outbox to its dropping cap.
	// The rollout bytes stay on disk with the offset unmoved, so a deferred read
	// is never a lost read. See claudeWatchMaxBytesPerPoll.
	budget := codexWatchMaxBytesPerPoll
	if outbox.UnderPressure() {
		budget = 0
	}
	deferredWork := false
	// Same root set the Claude watcher matches against: the workspace, every
	// directory registered by a later `start`, and their git worktrees. Codex
	// used to compare against the single workspace, so a registered second tree
	// would have stayed invisible here even after the Claude side saw it.
	roots := workspaceMatchRoots(workspace)
	if fp, dropped, changed := syncMatchCacheToRoots(progress.Match, progress.RootsFP, roots); changed {
		if dropped > 0 {
			fmt.Fprintf(os.Stderr, "codex-watcher: capture roots changed — re-checking %d previously unmatched rollout(s)\n", dropped)
		}
		progress.RootsFP = fp
		saveCodexWatchProgress(progress)
	}

	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, ".jsonl") {
			return nil
		}
		// Cheap candidate filter: skip files last modified before the history
		// window WITHOUT caching a decision — a file touched later
		// re-enters classification. Caching "no" here is the old bug that dropped
		// long/resumed/restart-spanning rollouts forever (mirrors
		// candidateClaudeTranscripts).
		if info.ModTime().Before(historyCutoff) {
			return nil
		}

		switch progress.Match[path] {
		case "no":
			return nil
		case "yes":
			// proceed to tail
		default:
			switch classifyCodexRollout(path, roots, historyCutoff) {
			case codexMatchYes:
				progress.Match[path] = "yes"
			case codexMatchYesPreexisting:
				// Go-forward: capture ongoing activity but not out-of-window
				// history. Seed the offset to current EOF so tailing starts at new
				// content. Only when unseen — a real prior offset (a restart-spanning
				// session already being tailed) must be preserved. If the stat fails
				// transiently, DON'T cache "yes" yet: leave the match undecided and
				// retry next poll, so a later success seeds EOF instead of tailing the
				// whole old file from offset 0.
				if _, ok := progress.Offsets[path]; !ok {
					info, err := os.Stat(path)
					if err != nil {
						return nil
					}
					progress.Offsets[path] = info.Size()
				}
				progress.Match[path] = "yes"
			case codexMatchNo:
				progress.Match[path] = "no"
				return nil
			default: // undecided — line 1 not a readable session_meta yet; retry next poll
				return nil
			}
		}

		if budget <= 0 {
			// Budget spent: leave this rollout's offset untouched so its unread
			// bytes re-surface next poll. Deferred, never dropped. Checked before
			// the processor is built, because a cold processor replays the whole
			// consumed prefix and shells out to git for the repo identity.
			deferredWork = true
			return nil
		}

		proc := processors[path]
		if proc == nil {
			proc = normalize.NewCodexRolloutProcessor(codexSessionIDFromPath(path))
			// Progress offsets survive daemon restarts, while the processor's
			// call/output correlation maps do not. Replay the consumed prefix only
			// to rebuild that transient state; deterministic events are discarded
			// and tailing still begins at the persisted offset below.
			if offset := progress.Offsets[path]; offset > 0 {
				replayCodexRolloutPrefix(path, offset, proc)
			}
			// Resolve the canonical repo identity ONCE per session (this processor is
			// created once per rollout file) from the session_meta cwd, and thread it
			// in as session state so each prompt event carries repoRoot + repoHost +
			// repoTracked. All three parts come from ONE call so the host and the
			// tracked bit can never be stamped from a different resolution pass than
			// the slug — they describe one observation of one directory, and a second
			// pass could see it after a `git init`.
			proc.RepoRoot, proc.RepoHost, proc.RepoTracked = sessionRepoIdentity(codexRolloutCwd(path))
			processors[path] = proc
		}
		n, consumed, more := tailCodexRollout(path, progress, proc, session, captureProse, budget)
		sent += n
		budget -= consumed
		if more {
			deferredWork = true
		}
		if consumed > 0 {
			// Persist after every rollout that moved: an unsaved offset replays
			// from byte zero after a SIGKILL, which is what stops a restart loop
			// ever finishing a long backfill.
			saveCodexWatchProgress(progress)
		}
		return nil
	})

	if deferredWork && verboseWatch() {
		fmt.Fprintf(os.Stderr, "codex-watcher: per-poll read budget spent (%d bytes) — remaining rollout history deferred to the next poll\n",
			codexWatchMaxBytesPerPoll)
	}

	saveCodexWatchProgress(progress)
	return sent
}

// replayCodexRolloutPrefix reconstructs a fresh processor's transient state
// after a watcher restart. It applies the same pre-parse redaction as live
// tailing and intentionally discards every event, so already-consumed records
// are never queued twice.
func replayCodexRolloutPrefix(path string, offset int64, proc *normalize.CodexRolloutProcessor) {
	// #nosec G304 -- path is a Codex rollout discovered under the sessions dir and is opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	reader := bufio.NewReader(io.LimitReader(f, offset))
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" {
				_ = proc.Process(redact.RedactBytes([]byte(trimmed)))
			}
		}
		if err != nil {
			return
		}
	}
}

type codexMatchResult int

const (
	codexMatchUndecided      codexMatchResult = iota
	codexMatchYes                             // matched; inside the history window — tail from the start
	codexMatchYesPreexisting                  // matched; older than the history window — capture go-forward from EOF
	codexMatchNo
)

// classifyCodexRollout decides whether a rollout belongs to this capture
// session by reading its first line — the session_meta header, the ONLY rollout
// line carrying cwd and the session start timestamp.
//
// cwd is authoritative. The timestamp admits sessions in the bounded history
// window from byte zero; older matched sessions are returned as
// codexMatchYesPreexisting and captured go-forward from current EOF.
//
// Unlike the Claude watcher's multi-line scan, cwd + timestamp both live on line
// 1 only, so a file caught mid-creation whose first line is not yet a readable
// session_meta is returned codexMatchUndecided (retry next poll) rather than
// cached as a mismatch — caching "no" would drop it forever.
func classifyCodexRollout(path string, roots []string, historyCutoff time.Time) codexMatchResult {
	// #nosec G304 -- path is a Codex rollout file discovered under the Codex sessions dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return codexMatchUndecided
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	if !scanner.Scan() {
		return codexMatchUndecided
	}
	var rec struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
		return codexMatchUndecided
	}
	if rec.Type != "session_meta" || rec.Payload.Cwd == "" {
		return codexMatchUndecided
	}
	if !pathWithinAny(resolvePath(rec.Payload.Cwd), roots) {
		return codexMatchNo
	}
	// Replay in-window history from the start. Anything older — INCLUDING a
	// rollout whose session_meta timestamp will not parse — remains eligible for
	// go-forward capture without uploading its historical prefix. The gate fails
	// CLOSED for the same reason the Claude one does: mtime is the only other
	// bound, and a months-old session resumed today has today's mtime.
	t, err := time.Parse(time.RFC3339, rec.Timestamp)
	if err != nil || t.Before(historyCutoff) {
		return codexMatchYesPreexisting
	}
	return codexMatchYes
}

// codexRolloutCwd returns the absolute cwd recorded in a rollout file's
// session_meta header (the only rollout line carrying cwd), or "" when the first
// line is not a readable session_meta. Read-only, first line only — the body is
// never retained; the caller reduces the cwd to a privacy-safe repo identity.
func codexRolloutCwd(path string) string {
	// #nosec G304 -- path is a Codex rollout file discovered under the Codex sessions dir by the watcher, not user input; opened read-only and only the cwd field is read.
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	if !scanner.Scan() {
		return ""
	}
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
		return ""
	}
	if rec.Type != "session_meta" {
		return ""
	}
	return rec.Payload.Cwd
}

// tailCodexRollout reads new complete lines from path (starting at the stored
// offset), processes them, queues resulting events, and advances the offset.
// A trailing partial line (no newline yet) is left for the next poll. Returns
// (events queued, bytes consumed, whether readable bytes remain unread).
//
// budget caps the bytes THIS call may read so one enormous rollout cannot spend
// a whole poll before the caller's loop gets to check. Stopping early is safe
// for the same reason the offset advance is: unread bytes stay on disk and the
// offset stops short of them.
//
// As in the claude watcher, advancing the offset unconditionally is only safe
// because delivery is now durable: this loop used to POST inline and advance
// regardless, so any 429/5xx/timeout destroyed the event permanently.
func tailCodexRollout(
	path string,
	progress codexWatchProgress,
	proc *normalize.CodexRolloutProcessor,
	session Session,
	captureProse bool,
	budget int64,
) (int, int64, bool) {
	// #nosec G304 -- path is a Codex rollout file discovered under the Codex sessions dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	offset := progress.Offsets[path]
	if _, err := f.Seek(offset, 0); err != nil {
		return 0, 0, false
	}

	reader := bufio.NewReader(f)
	consumed := int64(0)
	queued := 0
	truncated := false
	emit := func(ev event.Event) int { return emitCodexEvent(ev, session, captureProse) }
	for {
		if consumed >= budget {
			truncated = true
			break
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// No trailing newline yet — leave this partial line for next poll.
			break
		}
		consumed += int64(len(line))
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		// Scrub secrets before parsing/ingest — same redaction the hook path
		// applies. Rollout lines carry prompt text, command output, and file
		// patches that may contain keys/tokens the candidate pasted or printed.
		redacted := redact.RedactBytes([]byte(trimmed))
		for _, ev := range proc.Process(redacted) {
			queued += emit(ev)
		}
	}

	// Release a recovered user turn the next line will never come for. Runs on
	// EVERY poll, including polls that consumed nothing — a rollout whose final
	// line is the human's turn produces no further lines to flush it, so an
	// end-of-read hook that only fired after new bytes would never reach it.
	// Skipped when the budget cut the read short: the lines that would complete
	// that turn are sitting unread in the file, not absent.
	if !truncated {
		for _, ev := range proc.FlushStaleUserPrompt() {
			queued += emit(ev)
		}
	}

	if consumed > 0 {
		progress.Offsets[path] = offset + consumed
		if queued > 0 && verboseWatch() {
			fmt.Fprintf(os.Stderr, "codex-watcher: queued %d event(s) from %s\n", queued, filepath.Base(path))
		}
	}
	return queued, consumed, truncated
}

// emitCodexEvent stamps, dedupes, ledgers and queues one normalized event.
// Shared by the line-by-line tail and the stale-prompt flush so a recovered
// prompt goes through the IDENTICAL path as every other event — same device
// stamping, same path relativization, same signing.
func emitCodexEvent(ev event.Event, session Session, captureProse bool) int {
	// SessionID comes from the rollout; DeviceID comes from the environment.
	// Stamped here rather than in the normalizer, which has no business knowing
	// what machine it runs on — keeping the two sourced separately is what stops
	// them collapsing into one value.
	ev.DeviceID = session.DeviceID
	normalize.RelativizeEventPaths(&ev, session.TaskRoot)
	// Record AI bash execution windows for later commit-attribution recovery —
	// same as the Claude watcher. No-op unless this is an AI-attributed
	// `command` event.
	// Codex stamps every rollout record with its own timestamp, so the bounded
	// history replay is separable from the live tail per event — same terms as
	// the Claude funnel.
	replay := transcriptEventIsHistorical(&ev)
	recordAiBashWindow(&ev, session.TaskRoot, replay)
	// Idempotency: skip a file_diff whose resulting content the git watcher (or
	// another channel) has already emitted, so an apply_patch edit isn't
	// double-counted when the working-tree poll sees it later.
	if !dedupeFileDiff(session.TaskRoot, &ev, replay) {
		return 0
	}
	// Ledger first — it projects, scrubs, and signs ev in place, so the queued
	// copy is the exact bytes to ship. See queueClaudeWatchEvent.
	if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
		fmt.Fprintf(os.Stderr, "codex-watcher: buffer error: %v\n", err)
	}
	if err := outbox.Append(ev); err != nil {
		fmt.Fprintf(os.Stderr, "codex-watcher: queue error (%s): %v\n", ev.Kind, err)
		return 0
	}
	return 1
}

// resolvePath resolves symlinks (falling back to a cleaned path) so workspace
// comparisons survive macOS's /tmp -> /private/tmp aliasing.
func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// pathWithinAny reports whether child sits at or under ANY of roots. An empty
// root list matches nothing — a watcher with no resolvable roots captures
// nothing rather than everything.
func pathWithinAny(child string, roots []string) bool {
	for _, r := range roots {
		if r != "" && pathWithin(child, r) {
			return true
		}
	}
	return false
}

// PathWithin exports pathWithin for callers outside this package (the status
// row folds registered roots that the daemon root already covers).
func PathWithin(child, parent string) bool { return pathWithin(child, parent) }

// pathWithin reports whether child is the same as or nested under parent.
func pathWithin(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}
