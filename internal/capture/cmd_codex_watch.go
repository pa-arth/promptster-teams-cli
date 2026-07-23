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

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/policy"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const codexWatchInterval = 3 * time.Second

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

// codexSessionIDFromPath derives the rollout's session uuid from its filename,
// so a processor knows its session before reading a line. It equals
// session_meta.payload.id (verified), which the normalizer falls back to if the
// filename does not match — one rollout file is exactly one Codex session.
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
}

// codexProgressSchemaV is the current progress-file schema version. v1 drops
// every cached "no" once: the old timestamp rule cached a "no" for any rollout
// whose session_meta predated the watch cutoff (dropping long/resumed/
// restart-spanning sessions forever), and the poll loop's `case "no": continue`
// never re-evaluates them. Forcing one re-classification lets the go-forward
// rule pick them up; genuinely cwd-mismatched files simply re-cache "no" next
// poll. Mirrors claudeProgressSchemaV.
const codexProgressSchemaV = 1

func codexWatchProgressPath() string {
	return filepath.Join(state.StateDir(), "codex-watcher-progress.json")
}

func loadCodexWatchProgress() codexWatchProgress {
	p := codexWatchProgress{Offsets: map[string]int64{}, Match: map[string]string{}}
	data, err := os.ReadFile(codexWatchProgressPath())
	if err != nil {
		return p
	}
	_ = json.Unmarshal(data, &p)
	if p.Offsets == nil {
		p.Offsets = map[string]int64{}
	}
	if p.Match == nil {
		p.Match = map[string]string{}
	}
	// One-time schema upgrade: drop every cached "no" so previously-dropped
	// pre-cutoff sessions get re-classified under the go-forward rule (the poll
	// loop's `case "no": continue` would otherwise never re-evaluate them).
	// Genuinely cwd-mismatched files re-cache "no" on the next poll.
	if p.V < codexProgressSchemaV {
		for k, v := range p.Match {
			if v == "no" {
				delete(p.Match, k)
			}
		}
		p.V = codexProgressSchemaV
	}
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

func clearCodexWatcherState() {
	_ = os.Remove(codexWatcherStatePath())
	_ = os.Remove(codexWatchProgressPath())
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
	// Only consider rollout sessions that started at/after this capture session
	// began, so we never replay unrelated prior codex sessions.
	startCutoff := session.StartedAt.Add(-2 * time.Minute)

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
		queued := pollCodexRollouts(session, workspace, startCutoff, processors, captureProse)
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
	startCutoff time.Time,
	processors map[string]*normalize.CodexRolloutProcessor,
	captureProse bool,
) int {
	dir := codexSessionsDir()
	progress := loadCodexWatchProgress()
	sent := 0

	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, ".jsonl") {
			return nil
		}
		// Cheap candidate filter: skip files last modified before this capture
		// session started WITHOUT caching a decision — a file touched later
		// re-enters classification. Caching "no" here is the old bug that dropped
		// long/resumed/restart-spanning rollouts forever (mirrors
		// candidateClaudeTranscripts).
		if info.ModTime().Before(startCutoff) {
			return nil
		}

		switch progress.Match[path] {
		case "no":
			return nil
		case "yes":
			// proceed to tail
		default:
			switch classifyCodexRollout(path, workspace, startCutoff) {
			case codexMatchYes:
				progress.Match[path] = "yes"
			case codexMatchYesPreexisting:
				// Go-forward: capture ongoing activity but NOT the pre-watcher
				// history. Seed the offset to current EOF so tailing starts at new
				// content. Only when unseen — a real prior offset (a restart-spanning
				// session already being tailed) must be preserved. If the stat fails
				// transiently, DON'T cache "yes" yet: leave the match undecided and
				// retry next poll, so a later success seeds EOF instead of tailing the
				// whole pre-watcher file from offset 0.
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
		n := tailCodexRollout(path, progress, proc, session, captureProse)
		sent += n
		return nil
	})

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
	codexMatchYes                             // matched; started at/after cutoff — tail from the start
	codexMatchYesPreexisting                  // matched; started BEFORE cutoff (long/resumed/restart-spanning) — capture GO-FORWARD from EOF
	codexMatchNo
)

// classifyCodexRollout decides whether a rollout belongs to this capture
// session by reading its first line — the session_meta header, the ONLY rollout
// line carrying cwd and the session start timestamp.
//
// cwd is authoritative: a cwd match belongs to this session regardless of when
// it started. The timestamp only distinguishes NEW from PRE-EXISTING. A session
// whose session_meta predates this watch start (a long/resumed session, or one
// spanning a daemon restart — the daemon resets startCutoff every launch, and
// laptop sleep/wake restarts it constantly) is returned as
// codexMatchYesPreexisting and captured GO-FORWARD from current EOF, not dropped
// (the old bug, which silently lost every restart-spanning session).
//
// Unlike the Claude watcher's multi-line scan, cwd + timestamp both live on line
// 1 only, so a file caught mid-creation whose first line is not yet a readable
// session_meta is returned codexMatchUndecided (retry next poll) rather than
// cached as a mismatch — caching "no" would drop it forever.
func classifyCodexRollout(path, workspace string, startCutoff time.Time) codexMatchResult {
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
	if !pathWithin(resolvePath(rec.Payload.Cwd), workspace) {
		return codexMatchNo
	}
	// cwd matches. A session whose session_meta predates this watch start is
	// pre-existing — capture it GO-FORWARD from current EOF rather than dropping
	// it (the old bug) or re-uploading its whole history. A session started
	// at/after the cutoff is genuinely new — tail from the start.
	if t, err := time.Parse(time.RFC3339, rec.Timestamp); err == nil && t.Before(startCutoff) {
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
// the number of events parsed and queued.
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
) int {
	// #nosec G304 -- path is a Codex rollout file discovered under the Codex sessions dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	offset := progress.Offsets[path]
	if _, err := f.Seek(offset, 0); err != nil {
		return 0
	}

	reader := bufio.NewReader(f)
	consumed := int64(0)
	queued := 0
	for {
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
			ev := ev
			// SessionID comes from the rollout; DeviceID comes from the
			// environment. Stamped here rather than in the normalizer, which has
			// no business knowing what machine it runs on — keeping the two
			// sourced separately is what stops them collapsing into one value.
			ev.DeviceID = session.DeviceID
			normalize.RelativizeEventPaths(&ev, session.TaskRoot)
			// Record AI bash execution windows for later commit-attribution
			// recovery — same as the Claude watcher. No-op unless this is an
			// AI-attributed `command` event.
			recordAiBashWindow(&ev, session.TaskRoot)
			// Idempotency: skip a file_diff whose resulting content the git
			// watcher (or another channel) has already emitted, so an apply_patch
			// edit isn't double-counted when the working-tree poll sees it later.
			if !dedupeFileDiff(session.TaskRoot, &ev) {
				continue
			}
			// Ledger first — it projects, scrubs, and signs ev in place, so the
			// queued copy is the exact bytes to ship. See queueClaudeWatchEvent.
			if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
				fmt.Fprintf(os.Stderr, "codex-watcher: buffer error: %v\n", err)
			}
			if err := outbox.Append(ev); err != nil {
				fmt.Fprintf(os.Stderr, "codex-watcher: queue error (%s): %v\n", ev.Kind, err)
				continue
			}
			queued++
		}
	}

	if consumed > 0 {
		progress.Offsets[path] = offset + consumed
		if queued > 0 && verboseWatch() {
			fmt.Fprintf(os.Stderr, "codex-watcher: queued %d event(s) from %s\n", queued, filepath.Base(path))
		}
	}
	return queued
}

// resolvePath resolves symlinks (falling back to a cleaned path) so workspace
// comparisons survive macOS's /tmp -> /private/tmp aliasing.
func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// pathWithin reports whether child is the same as or nested under parent.
func pathWithin(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}
