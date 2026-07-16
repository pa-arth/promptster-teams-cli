package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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

// The desktop watcher covers Claude Code's DESKTOP-APP (GUI) session store,
// which the terminal-CLI watcher (~/.claude/projects/**/*.jsonl) never sees.
// The desktop app persists each session as ONE JSON blob (not JSONL) under the
// platform config dir:
//
//	~/Library/Application Support/Claude/claude-code-sessions/<workspace-id>/<session-id>/local_<uuid>.json   (macOS)
//
// plus a sibling local-agent-mode-sessions/ store. Because the file is a single
// growing blob rewritten in place (not append-only), this watcher uses
// mtime+size change-detection rather than byte offsets: on each poll it re-stats
// every local_*.json, and only re-reads the whole file when its (mtime, size)
// differs from the version it last processed. A per-session processor keeps a
// cursor so a re-read only emits messages appended since the previous read.

const desktopWatchInterval = 3 * time.Second

// ClaudeDesktopSessionsDir returns the desktop app's session store root. The
// desktop app writes one JSON blob per session under
// <store>/<workspace-id>/<session-id>/local_<uuid>.json. Exported so the `cli`
// package (doctor) can stat it. Mirrors ClaudeProjectsDir's shape.
func ClaudeDesktopSessionsDir() string {
	return filepath.Join(claudeDesktopBaseDir(), "claude-code-sessions")
}

// ClaudeDesktopAgentModeDir returns the sibling agent-mode session store, which
// holds desktop-app "agent mode" sessions in the same local_*.json shape.
func ClaudeDesktopAgentModeDir() string {
	return filepath.Join(claudeDesktopBaseDir(), "local-agent-mode-sessions")
}

// claudeDesktopBaseDir resolves the per-OS "Claude" application-support root the
// desktop app writes under. Prefers os.UserConfigDir() (darwin →
// ~/Library/Application Support, windows → %APPDATA%, linux → ~/.config) and
// falls back to the documented per-OS path if that lookup fails.
func claudeDesktopBaseDir() string {
	if cfg, err := os.UserConfigDir(); err == nil && cfg != "" {
		return filepath.Join(cfg, "Claude")
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Claude")
		}
		return filepath.Join(home, "AppData", "Roaming", "Claude")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude")
	default: // linux and friends
		return filepath.Join(home, ".config", "Claude")
	}
}

// desktopWatcherState tracks the background desktop-session-tailing process.
// Mirrors codexWatcherState; the in-process EventsCaptured counter is re-derived
// from zero each run, so its tag carries no compatibility concern.
type desktopWatcherState struct {
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
	WatchDir      string `json:"watchDir,omitempty"`
	LogPath       string `json:"logPath,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	// EventsCaptured counts events parsed and queued, not delivered (delivery is
	// asynchronous via internal/outbox).
	EventsCaptured int `json:"eventsCaptured,omitempty"`
}

func desktopWatcherStatePath() string {
	return filepath.Join(state.StateDir(), "desktop-watcher.json")
}
func desktopWatcherLogPath() string {
	return filepath.Join(state.StateDir(), "desktop-watcher.log")
}

// desktopFileVersion is the change-detection fingerprint for one local_*.json:
// modification time (nanoseconds) and byte size. A file whose (mtime, size)
// matches the stored version was not touched since we last read it, so it is
// skipped without opening it. Any change flips at least one of the two.
type desktopFileVersion struct {
	Mtime int64 `json:"mtime"` // ModTime().UnixNano()
	Size  int64 `json:"size"`
}

// desktopWatchProgress persists a per-file change-detection map (NOT byte
// offsets — the desktop blob is rewritten whole, so an offset is meaningless
// across writes). Versions[path] records the (mtime, size) we last processed;
// the per-session processor cursor (message index) lives in memory across polls.
type desktopWatchProgress struct {
	Versions map[string]desktopFileVersion `json:"versions"`
}

func desktopWatchProgressPath() string {
	return filepath.Join(state.StateDir(), "desktop-watcher-progress.json")
}

func loadDesktopWatchProgress() desktopWatchProgress {
	p := desktopWatchProgress{Versions: map[string]desktopFileVersion{}}
	data, err := os.ReadFile(desktopWatchProgressPath())
	if err != nil {
		return p
	}
	_ = json.Unmarshal(data, &p)
	if p.Versions == nil {
		p.Versions = map[string]desktopFileVersion{}
	}
	return p
}

func saveDesktopWatchProgress(p desktopWatchProgress) {
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	tmp := desktopWatchProgressPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, desktopWatchProgressPath())
}

func loadDesktopWatcherState() (desktopWatcherState, error) {
	data, err := os.ReadFile(desktopWatcherStatePath())
	if err != nil {
		return desktopWatcherState{}, err
	}
	var s desktopWatcherState
	if err := json.Unmarshal(data, &s); err != nil {
		return desktopWatcherState{}, err
	}
	return s, nil
}

func saveDesktopWatcherState(s desktopWatcherState) error {
	path := desktopWatcherStatePath()
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

func clearDesktopWatcherState() {
	_ = os.Remove(desktopWatcherStatePath())
	_ = os.Remove(desktopWatchProgressPath())
}

func isDesktopWatcherRunning() (desktopWatcherState, bool) {
	st, err := loadDesktopWatcherState()
	if err != nil || st.PID <= 0 {
		return desktopWatcherState{}, false
	}
	if processExists(st.PID) {
		return st, true
	}
	clearDesktopWatcherState()
	return desktopWatcherState{}, false
}

// RunDesktopWatcher is the main loop for the `promptster desktop-watch`
// subcommand. It polls the Claude desktop-app session store for changed
// local_*.json blobs, normalizes each into canonical events, redacts on-device,
// signs, and ingests. Mirrors RunCodexWatcher's shape.
//
// Most machines do NOT have the desktop app installed, so the store dir is
// usually absent. This must never crash or spin hot there: if the store is
// missing at startup, it logs once and enters a cheap re-check loop that only
// wakes to stat the dir every interval until the app appears (or a signal
// arrives).
func RunDesktopWatcher() error {
	session, err := loadSession()
	if err != nil {
		return fmt.Errorf("no active session: %w", err)
	}
	if session.TaskRoot == "" {
		return fmt.Errorf("session has no task root")
	}
	if st, ok := isDesktopWatcherRunning(); ok && st.PID != os.Getpid() {
		return fmt.Errorf("desktop watcher already running (pid %d)", st.PID)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveDesktopWatcherState(desktopWatcherState{
		PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
		LogPath: desktopWatcherLogPath(), LastHeartbeat: now,
	}); err != nil {
		return err
	}
	defer clearDesktopWatcherState()

	if os.Getenv("PROMPTSTER_API_URL") == "" && session.ApiURL != "" {
		_ = os.Setenv("PROMPTSTER_API_URL", session.ApiURL)
	}

	// Only consider desktop sessions modified at/after this capture session began
	// (small back-margin), so we never replay unrelated prior desktop sessions.
	startCutoff := session.StartedAt.Add(-2 * time.Minute)

	// SIGTERM as well as SIGINT — see the matching note in RunClaudeWatcher.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	client := &http.Client{Timeout: 5 * time.Second}
	// One processor per session blob, kept in memory for the daemon's life so the
	// per-session cursor (last-emitted message index) survives re-reads and only
	// NEW tail messages emit.
	processors := map[string]*normalize.DesktopSessionProcessor{}
	eventsCaptured := 0

	// Shared device-wide delivery queue; StartDrain is a process-wide singleton
	// (see RunCodexWatcher for the full rationale).
	outbox.StartDrain(client, session.SessionToken)

	// Org capture policy (opt-in assistant prose), fail-closed, refreshed off the
	// hot path — mirrors the codex watcher.
	policyResolver := policy.NewResolver(session.SessionToken)
	policyCtx, cancelPolicy := context.WithCancel(context.Background())
	defer cancelPolicy()
	policyResolver.StartBackground(policyCtx)

	// The desktop app is usually absent. Log the idle state exactly once so the
	// daemon.log doesn't repeat it every poll, then fall through to the poll loop,
	// which re-checks existence cheaply each iteration and starts capturing the
	// moment the app appears.
	loggedIdle := false
	if !dirExists(ClaudeDesktopSessionsDir()) {
		fmt.Fprintln(os.Stderr, "desktop-watcher: desktop app not detected; desktop watcher idle")
		loggedIdle = true
	} else if verboseWatch() {
		fmt.Fprintf(os.Stderr, "desktop-watcher: started, polling %s every %s\n",
			ClaudeDesktopSessionsDir(), desktopWatchInterval)
	}

	for {
		if dirExists(ClaudeDesktopSessionsDir()) || dirExists(ClaudeDesktopAgentModeDir()) {
			captureProse := policyResolver.CaptureAssistantProse()
			queued := pollDesktopSessions(session, startCutoff, processors, captureProse)
			eventsCaptured += queued
			loggedIdle = false // the app appeared — allow a fresh idle log if it later vanishes
		} else if !loggedIdle {
			fmt.Fprintln(os.Stderr, "desktop-watcher: desktop app not detected; desktop watcher idle")
			loggedIdle = true
		}

		_ = saveDesktopWatcherState(desktopWatcherState{
			PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
			LogPath:       desktopWatcherLogPath(),
			LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano), EventsCaptured: eventsCaptured,
		})

		select {
		case <-signals:
			fmt.Fprintf(os.Stderr, "desktop-watcher: shutting down (captured %d events)\n", eventsCaptured)
			return nil
		case <-time.After(desktopWatchInterval):
		}
	}
}

// pollDesktopSessions walks the desktop session store (and the agent-mode store)
// for local_*.json blobs modified at/after the cutoff, re-reads only those whose
// (mtime, size) changed since last poll, normalizes them, and queues the
// resulting events. Returns the number queued.
//
// Workspace gating: this captures ALL desktop sessions and does NOT filter by
// cwd. Teams capture is DEVICE-scoped, and source-exclusion at the signing choke
// point guarantees no code ever leaves the machine — so there is no reason to
// scope by workspace. A desktop-app cwd is often the app's own VM/sandbox
// internal path rather than the repo the developer thinks they're in, so
// cwd-gating (as the codex/claude terminal watchers do) would wrongly drop
// essentially every desktop session.
func pollDesktopSessions(
	session Session,
	startCutoff time.Time,
	processors map[string]*normalize.DesktopSessionProcessor,
	captureProse bool,
) int {
	progress := loadDesktopWatchProgress()
	sent := 0

	walk := func(root string) {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			if !strings.HasPrefix(base, "local_") || !strings.HasSuffix(base, ".json") {
				return nil
			}
			// Skip blobs untouched since this capture session started.
			if info.ModTime().Before(startCutoff) {
				return nil
			}

			ver := desktopFileVersion{Mtime: info.ModTime().UnixNano(), Size: info.Size()}
			if prev, ok := progress.Versions[path]; ok && prev == ver {
				// Unchanged since last processed — nothing new in this blob.
				return nil
			}

			// Read the WHOLE blob (single-JSON, not append-only) and scrub secrets
			// before the schema parser sees it — same redaction the hook path
			// applies. #nosec G304 -- path is a desktop session file discovered under
			// the Claude desktop store by the watcher, not user input; read-only.
			blob, rerr := os.ReadFile(path) //nolint:gosec
			if rerr != nil {
				return nil
			}
			redacted := redact.RedactBytes(blob)

			proc := processors[path]
			if proc == nil {
				proc = normalize.NewDesktopSessionProcessor(desktopSessionIDFromPath(path))
				processors[path] = proc
			}
			for _, ev := range proc.Process(redacted) {
				queueDesktopWatchEvent(ev, session, captureProse)
				sent++
			}
			// Only advance the version once the read+emit for this blob succeeded,
			// so a transient read error retries next poll instead of silently
			// skipping the change.
			progress.Versions[path] = ver
			return nil
		})
	}

	walk(ClaudeDesktopSessionsDir())
	walk(ClaudeDesktopAgentModeDir())

	saveDesktopWatchProgress(progress)
	return sent
}

// desktopSessionIDFromPath derives the desktop session id from the blob's path.
// The store lays sessions out as <store>/<workspace-id>/<session-id>/local_*.json,
// so the session id is the parent directory name. Falls back to the local_ uuid
// in the filename if the layout is unexpected, so a processor is never left with
// an empty session id (which would pool events into a shared "unknown" chain).
func desktopSessionIDFromPath(path string) string {
	if parent := filepath.Base(filepath.Dir(path)); parent != "" && parent != "." && parent != string(filepath.Separator) {
		return parent
	}
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	return strings.TrimPrefix(base, "local_")
}

// queueDesktopWatchEvent runs the shared per-event funnel, mirroring
// queueClaudeWatchEvent EXACTLY: stamp device identity → path-relativize →
// cross-channel file_diff dedup → local signed ledger → send queue. The ordering
// is load-bearing (DeviceID before the ledger append so the audit copy carries
// it; the ledger append before the queue so source-exclusion + signing happen on
// the exact bytes shipped) — see queueClaudeWatchEvent for the full reasoning.
func queueDesktopWatchEvent(ev event.Event, session Session, captureProse bool) {
	ev.DeviceID = session.DeviceID
	normalize.RelativizeEventPaths(&ev, session.TaskRoot)
	if !dedupeFileDiff(session.TaskRoot, &ev) {
		return
	}
	if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
		fmt.Fprintf(os.Stderr, "desktop-watcher: buffer error: %v\n", err)
	}
	if err := outbox.Append(ev); err != nil {
		fmt.Fprintf(os.Stderr, "desktop-watcher: queue error (%s): %v\n", ev.Kind, err)
	}
}
