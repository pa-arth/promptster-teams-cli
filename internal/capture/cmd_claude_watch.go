package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
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
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
	LogPath       string `json:"logPath,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	EventsSent    int    `json:"eventsSent,omitempty"`
	BytesConsumed int64  `json:"bytesConsumed,omitempty"`
	// Degraded means the watcher is running but parsing nothing from a
	// transcript it consumed substantial bytes from — treat as unhealthy.
	Degraded bool `json:"degraded,omitempty"`
}

func claudeWatcherStatePath() string { return filepath.Join(state.StateDir(), "claude-watcher.json") }
func claudeWatcherLogPath() string   { return filepath.Join(state.StateDir(), "claude-watcher.log") }

// claudeWatchProgress persists per-transcript byte offsets and the
// workspace-match decision so each line is processed exactly once across polls
// and watcher restarts.
type claudeWatchProgress struct {
	Offsets map[string]int64 `json:"offsets"`
	// Match: path -> "yes"|"no". Unlike codex rollouts (whose first line is
	// always session_meta), a transcript's early lines may not carry cwd yet,
	// so absence of a cached decision means "retry next poll" — only a
	// definitive cwd mismatch or a line-budget exhaustion caches "no".
	Match map[string]string `json:"match"`
}

func claudeWatchProgressPath() string {
	return filepath.Join(state.StateDir(), "claude-watcher-progress.json")
}

func loadClaudeWatchProgress() claudeWatchProgress {
	p := claudeWatchProgress{Offsets: map[string]int64{}, Match: map[string]string{}}
	data, err := os.ReadFile(claudeWatchProgressPath())
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
	return p
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

func clearClaudeWatcherState() {
	_ = os.Remove(claudeWatcherStatePath())
	_ = os.Remove(claudeWatchProgressPath())
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
		PID: os.Getpid(), StartedAt: now, LogPath: claudeWatcherLogPath(), LastHeartbeat: now,
	}); err != nil {
		return err
	}
	defer clearClaudeWatcherState()

	if os.Getenv("PROMPTSTER_API_URL") == "" && session.ApiURL != "" {
		_ = os.Setenv("PROMPTSTER_API_URL", session.ApiURL)
	}

	workspace := resolvePath(session.TaskRoot)
	startCutoff := session.StartedAt.Add(-2 * time.Minute)

	// If hooks took over while no watcher was alive, skip everything already
	// on disk — those lines' events were emitted by the hook path.
	if _, err := os.Stat(claudeHookTakeoverPath()); err == nil {
		fastForwardClaudeTranscripts(workspace, startCutoff)
		_ = os.Remove(claudeHookTakeoverPath())
		fmt.Fprintf(os.Stderr, "claude-watcher: hooks covered an outage window — fast-forwarded past existing transcript content\n")
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)

	client := &http.Client{Timeout: 5 * time.Second}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}
	eventsSent := 0
	var bytesConsumed, bytesSinceEvent int64
	degraded := false

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
		parsed, consumed := pollClaudeTranscripts(session, workspace, startCutoff, processors, client, degraded, captureProse)
		bytesConsumed += consumed
		wasDegraded := degraded
		degraded, bytesSinceEvent = claudeDegradationStep(degraded, parsed, consumed, bytesSinceEvent)
		switch {
		case wasDegraded && !degraded:
			_ = os.Remove(claudeHookTakeoverPath())
			fmt.Fprintf(os.Stderr, "claude-watcher: parser recovered — resuming event ownership (discarded %d hook-covered event(s))\n", parsed)
		case !wasDegraded && degraded:
			fmt.Fprintf(os.Stderr, "claude-watcher: degraded — %d bytes consumed since last parsed event; hooks take over\n", bytesSinceEvent)
		case !wasDegraded:
			eventsSent += parsed
		}

		_ = saveClaudeWatcherState(claudeWatcherState{
			PID: os.Getpid(), StartedAt: now, LogPath: claudeWatcherLogPath(),
			LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano),
			EventsSent:    eventsSent,
			BytesConsumed: bytesConsumed,
			Degraded:      degraded,
		})

		select {
		case <-signals:
			fmt.Fprintf(os.Stderr, "claude-watcher: shutting down (sent %d events)\n", eventsSent)
			return nil
		case <-time.After(claudeWatchInterval):
		}
	}
}

// pollClaudeTranscripts scans for transcript files, tails matched ones from
// their stored byte offset, and ingests normalized events. Returns (events
// parsed, bytes consumed). With dryRun set (degraded mode), events are parsed
// and counted — proving the parser works — but NOT ingested: hooks own
// emission for that window.
func pollClaudeTranscripts(
	session Session,
	workspace string,
	startCutoff time.Time,
	processors map[string]*normalize.ClaudeTranscriptProcessor,
	client *http.Client,
	dryRun bool,
	captureProse bool,
) (int, int64) {
	progress := loadClaudeWatchProgress()
	sent := 0
	var consumed int64
	roots := workspaceMatchRoots(workspace)

	for _, path := range candidateClaudeTranscripts(startCutoff) {
		switch progress.Match[path] {
		case "no":
			continue
		case "yes":
			// proceed to tail
		default:
			switch classifyClaudeTranscript(path, roots, startCutoff) {
			case claudeMatchYes:
				progress.Match[path] = "yes"
			case claudeMatchNo:
				progress.Match[path] = "no"
				continue
			default: // undecided — no cwd line yet; retry next poll
				continue
			}
		}

		proc := processors[path]
		if proc == nil {
			proc = normalize.NewClaudeTranscriptProcessor(claudeSessionIDFromPath(path))
			if isClaudeSidechainFile(path) {
				proc.UsageOnly = true
				// The filename is the floor for sidechain attribution: rows
				// usually repeat it (plus skill/agent names), but agentId must
				// survive even if they don't.
				proc.AgentID = claudeAgentIDFromPath(path)
			}
			processors[path] = proc
		}
		n, c := tailClaudeTranscript(path, progress, proc, session, client, dryRun, captureProse)
		sent += n
		consumed += c
	}

	// Force-flush assistant messages that stopped receiving lines (turn ended
	// without a prompt boundary yet).
	for _, proc := range processors {
		for _, ev := range proc.FlushStale(claudeAccumFlushAge) {
			if dryRun {
				sent++
				continue
			}
			if ingestClaudeWatchEvent(ev, session, client, captureProse) {
				sent++
			}
		}
	}

	saveClaudeWatchProgress(progress)
	return sent, consumed
}

// candidateClaudeTranscripts lists transcript files modified at/after the
// cutoff. Subagent transcripts (<session>/subagents/agent-*.jsonl) are
// included — their token usage is real spend — but processed in UsageOnly
// mode (see isClaudeSidechainFile): their "user" messages are agent-authored
// prompts that must not enter the candidate's timeline.
func candidateClaudeTranscripts(startCutoff time.Time) []string {
	var out []string
	_ = filepath.Walk(ClaudeProjectsDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(filepath.Base(path), ".jsonl") {
			return nil
		}
		if info.ModTime().Before(startCutoff) {
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
	claudeMatchUndecided claudeMatchResult = iota
	claudeMatchYes
	claudeMatchNo
)

// workspaceMatchRoots returns the workspace plus every git worktree
// registered to its repository. A developer who parallelizes with
// `git worktree add ../fix` runs claude processes whose cwd is OUTSIDE the
// workspace directory; those transcripts belong to this capture session and
// must be tailed. Re-read every poll so worktrees created mid-session are
// picked up before their transcripts get classified.
func workspaceMatchRoots(workspace string) []string {
	roots := []string{workspace}
	// #nosec G204 -- constant argv; workspace is the capture session's own root dir, not user input. Reads only the local worktree list.
	out, err := exec.Command("git", "-C", workspace, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return roots
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		p := resolvePath(strings.TrimSpace(strings.TrimPrefix(line, "worktree ")))
		if p != "" && p != workspace {
			roots = append(roots, p)
		}
	}
	return roots
}

// classifyClaudeTranscript decides whether a transcript belongs to this
// capture session by scanning its first lines for one carrying cwd and matching
// it against the workspace or any of its registered worktrees. Early lines
// (mode, permission-mode, ...) often lack cwd, so a file with no cwd yet stays
// undecided rather than being cached as a mismatch.
func classifyClaudeTranscript(path string, roots []string, startCutoff time.Time) claudeMatchResult {
	// #nosec G304 -- path is a Claude transcript discovered under ~/.claude/projects by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return claudeMatchUndecided
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	const maxScanLines = 50
	scanned := 0
	for scanner.Scan() {
		scanned++
		if scanned > maxScanLines {
			// A real session writes a cwd-bearing line within the first
			// prompt; a long cwd-less file is not a session transcript.
			return claudeMatchNo
		}
		var rec struct {
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Cwd == "" {
			continue
		}
		if rec.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, rec.Timestamp); err == nil && t.Before(startCutoff) {
				return claudeMatchNo
			}
		}
		resolved := resolvePath(rec.Cwd)
		for _, root := range roots {
			if pathWithin(resolved, root) {
				return claudeMatchYes
			}
		}
		return claudeMatchNo
	}
	// EOF without a cwd line: file just created, still growing — retry later.
	return claudeMatchUndecided
}

// fastForwardClaudeTranscripts sets every currently-matched transcript's
// offset to its current size, so tailing resumes at content written from now
// on. Used after a hook-takeover window.
func fastForwardClaudeTranscripts(workspace string, startCutoff time.Time) {
	progress := loadClaudeWatchProgress()
	roots := workspaceMatchRoots(workspace)
	for _, path := range candidateClaudeTranscripts(startCutoff) {
		if progress.Match[path] == "no" {
			continue
		}
		if progress.Match[path] != "yes" {
			if classifyClaudeTranscript(path, roots, startCutoff) != claudeMatchYes {
				continue
			}
			progress.Match[path] = "yes"
		}
		if info, err := os.Stat(path); err == nil {
			progress.Offsets[path] = info.Size()
		}
	}
	saveClaudeWatchProgress(progress)
}

// tailClaudeTranscript reads new complete lines from path starting at the
// stored offset, processes them, ingests resulting events, and advances the
// offset. A trailing partial line is left for the next poll.
func tailClaudeTranscript(
	path string,
	progress claudeWatchProgress,
	proc *normalize.ClaudeTranscriptProcessor,
	session Session,
	client *http.Client,
	dryRun bool,
	captureProse bool,
) (int, int64) {
	// #nosec G304 -- path is a Claude transcript discovered under ~/.claude/projects by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	offset := progress.Offsets[path]
	if _, err := f.Seek(offset, 0); err != nil {
		return 0, 0
	}

	reader := bufio.NewReader(f)
	consumed := int64(0)
	sent := 0
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break // partial line — next poll
		}
		consumed += int64(len(line))
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		// Scrub secrets before parsing/ingest — transcript lines carry prompt
		// text, command output, and file content.
		redacted := redact.RedactBytes([]byte(trimmed))
		for _, ev := range proc.Process(redacted) {
			if dryRun {
				sent++
				continue
			}
			ev := ev
			if ingestClaudeWatchEvent(ev, session, client, captureProse) {
				sent++
			}
		}
	}

	if consumed > 0 {
		progress.Offsets[path] = offset + consumed
		if sent > 0 && verboseWatch() {
			fmt.Fprintf(os.Stderr, "claude-watcher: sent %d event(s) from %s\n", sent, filepath.Base(path))
		}
	}
	return sent, consumed
}

// ingestClaudeWatchEvent runs the shared per-event funnel (path relativize,
// cross-channel file_diff dedup, local signed buffer, ingest POST). Returns
// true when the event was sent.
func ingestClaudeWatchEvent(ev event.Event, session Session, client *http.Client, captureProse bool) bool {
	// Device identity is stamped here, at the funnel, rather than inside the
	// normalizer: the normalizer's job is to read a transcript, and it has no
	// business knowing what machine it runs on. SessionID comes from the
	// transcript; DeviceID comes from the environment. Keeping the two sourced
	// separately is what stops them collapsing back into one value.
	ev.DeviceID = session.DeviceID
	normalize.RelativizeEventPaths(&ev, session.TaskRoot)
	// Cross-channel idempotency: skip a file_diff whose resulting content the
	// hook or git watcher already emitted.
	if !dedupeFileDiff(session.TaskRoot, &ev) {
		return false
	}
	if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
		fmt.Fprintf(os.Stderr, "claude-watcher: buffer error: %v\n", err)
	}
	if err := ingest.IngestEventWithClient(client, ev, session.SessionToken); err != nil {
		// A 4xx schema/kind rejection (e.g. subagent_usage/config_census before
		// the backend that accepts them deploys) is NOT a channel failure: the
		// parser and transport are fine. Count the event as handled — otherwise
		// a stream of rejected sidechain usage would accumulate bytes-without-
		// events and trip the degraded state machine, silently stopping ALL
		// capture. Rejections are dropped without retry (offsets advance
		// regardless) and logged only under debug to avoid stderr spam.
		if ingest.IsIngestRejection(err) {
			state.HookDebugf("claude-watcher: event rejected by backend (%s): %v", ev.Kind, err)
			return true
		}
		fmt.Fprintf(os.Stderr, "claude-watcher: send error (%s): %v\n", ev.Kind, err)
		return false
	}
	return true
}
