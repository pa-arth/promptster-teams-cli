package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
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
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
	LogPath       string `json:"logPath,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	EventsSent    int    `json:"eventsSent,omitempty"`
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
}

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
		PID: os.Getpid(), StartedAt: now, LogPath: codexWatcherLogPath(), LastHeartbeat: now,
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
	eventsSent := 0

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
		sent := pollCodexRollouts(session, workspace, startCutoff, processors, client, captureProse)
		eventsSent += sent

		_ = saveCodexWatcherState(codexWatcherState{
			PID: os.Getpid(), StartedAt: now, LogPath: codexWatcherLogPath(),
			LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano), EventsSent: eventsSent,
		})

		select {
		case <-signals:
			fmt.Fprintf(os.Stderr, "codex-watcher: shutting down (sent %d events)\n", eventsSent)
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
	client *http.Client,
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
		// Skip files last modified before this capture session started.
		if info.ModTime().Before(startCutoff) {
			progress.Match[path] = "no"
			return nil
		}

		switch progress.Match[path] {
		case "no":
			return nil
		case "yes":
			// proceed to tail
		default:
			if codexRolloutMatchesWorkspace(path, workspace, startCutoff) {
				progress.Match[path] = "yes"
			} else {
				progress.Match[path] = "no"
				return nil
			}
		}

		proc := processors[path]
		if proc == nil {
			proc = normalize.NewCodexRolloutProcessor(codexSessionIDFromPath(path))
			processors[path] = proc
		}
		n := tailCodexRollout(path, progress, proc, session, client, captureProse)
		sent += n
		return nil
	})

	saveCodexWatchProgress(progress)
	return sent
}

// codexRolloutMatchesWorkspace reads a rollout file's session_meta header and
// reports whether its cwd is inside the workspace and its timestamp is recent
// enough to belong to this capture session.
func codexRolloutMatchesWorkspace(path, workspace string, startCutoff time.Time) bool {
	// #nosec G304 -- path is a Codex rollout file discovered under the Codex sessions dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	if !scanner.Scan() {
		return false
	}
	var rec struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
		return false
	}
	if rec.Type != "session_meta" || rec.Payload.Cwd == "" {
		return false
	}
	if t, err := time.Parse(time.RFC3339, rec.Timestamp); err == nil && t.Before(startCutoff) {
		return false
	}
	return pathWithin(resolvePath(rec.Payload.Cwd), workspace)
}

// tailCodexRollout reads new complete lines from path (starting at the stored
// offset), processes them, ingests resulting events, and advances the offset.
// A trailing partial line (no newline yet) is left for the next poll.
func tailCodexRollout(
	path string,
	progress codexWatchProgress,
	proc *normalize.CodexRolloutProcessor,
	session Session,
	client *http.Client,
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
	sent := 0
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
			// Idempotency: skip a file_diff whose resulting content the git
			// watcher (or another channel) has already emitted, so an apply_patch
			// edit isn't double-counted when the working-tree poll sees it later.
			if !dedupeFileDiff(session.TaskRoot, &ev) {
				continue
			}
			if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
				fmt.Fprintf(os.Stderr, "codex-watcher: buffer error: %v\n", err)
			}
			if err := ingest.IngestEventWithClient(client, ev, session.SessionToken); err != nil {
				// Schema/kind rejections (4xx) are dropped without retry and
				// logged quietly — the channel is healthy, the deployed
				// backend just doesn't accept this event yet (see
				// ingestClaudeWatchEvent for the rationale).
				if ingest.IsIngestRejection(err) {
					state.HookDebugf("codex-watcher: event rejected by backend (%s): %v", ev.Kind, err)
					sent++
					continue
				}
				fmt.Fprintf(os.Stderr, "codex-watcher: send error (%s): %v\n", ev.Kind, err)
				continue
			}
			sent++
		}
	}

	if consumed > 0 {
		progress.Offsets[path] = offset + consumed
		if sent > 0 && verboseWatch() {
			fmt.Fprintf(os.Stderr, "codex-watcher: sent %d event(s) from %s\n", sent, filepath.Base(path))
		}
	}
	return sent
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
