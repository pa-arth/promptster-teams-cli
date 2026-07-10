package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// daemonState records the detached background-capture supervisor so `stop` and
// `status` can find it. The supervisor is a single `promptster-teams watch`
// process — it owns presence, census, and both transcript watchers as in-process
// goroutines, so this one PID is the whole background capture.
type daemonState struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	WatchDir  string `json:"watchDir,omitempty"`
	LogPath   string `json:"logPath,omitempty"`
}

func daemonStatePath() string { return filepath.Join(state.StateDir(), "supervisor.json") }
func daemonLogPath() string   { return filepath.Join(state.StateDir(), "daemon.log") }

func loadDaemonState() (daemonState, error) {
	data, err := os.ReadFile(daemonStatePath())
	if err != nil {
		return daemonState{}, err
	}
	var s daemonState
	if err := json.Unmarshal(data, &s); err != nil {
		return daemonState{}, err
	}
	return s, nil
}

func saveDaemonState(s daemonState) error {
	path := daemonStatePath()
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

func clearDaemonState() { _ = os.Remove(daemonStatePath()) }

// DaemonStatus reports whether a background-capture supervisor is alive. A stale
// state file (PID no longer running) is cleared and reported as not running, so
// callers never see a phantom daemon.
func DaemonStatus() (pid int, running bool) {
	st, err := loadDaemonState()
	if err != nil || st.PID <= 0 {
		return 0, false
	}
	if processExists(st.PID) {
		return st.PID, true
	}
	clearDaemonState()
	return 0, false
}

// StartDaemon spawns the transcript capture as a detached background process and
// returns immediately, freeing the shell. It re-runs this same binary as `watch`
// (which already owns credential export, signing-keypair bootstrap, presence,
// census, and both watchers) so the background path and the foreground `watch`
// share one code path.
//
// It does NOT print — callers render their own UX (the `start` command prints a
// plain banner; `login` prints a styled line). It is idempotent: if a supervisor
// is already alive it spawns nothing and returns that pid with alreadyRunning=true.
// `stop` tears it down.
func StartDaemon(args []string) (pid int, watchDir string, alreadyRunning bool, err error) {
	if p, running := DaemonStatus(); running {
		return p, "", true, nil
	}

	token, apiURL, watchDir, err := resolveWatchEnv(args)
	if err != nil {
		return 0, "", false, err
	}

	logPath := daemonLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, "", false, err
	}
	// #nosec G304 -- logPath is daemonLogPath() derived from StateDir(), not user input.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", false, err
	}
	defer logFile.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, "", false, err
	}
	defer devNull.Close()

	// Export the resolved values so the child (which inherits our environment)
	// observes the same credential/URL/watch-dir — a detached child can't be
	// trusted to re-derive cwd once the launching shell moves on.
	_ = os.Setenv("PROMPTSTER_TEAMS_TOKEN", token)
	_ = os.Setenv("PROMPTSTER_TEAMS_API_URL", apiURL)
	_ = os.Setenv("PROMPTSTER_API_URL", apiURL)
	_ = os.Setenv("PROMPTSTER_TEAMS_WATCH_DIR", watchDir)

	// #nosec G204 -- re-execs our own resolved install binary (state.PromptsterBin()); the subcommand is a constant.
	cmd := exec.Command(state.PromptsterBin(), "watch")
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Fully detach: a new session (unix) / process group (windows) so closing
	// the launching terminal doesn't deliver SIGHUP/Ctrl-Break to the daemon.
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		return 0, "", false, fmt.Errorf("could not start background capture: %w", err)
	}
	// Capture the PID before Release() — releasing the Process handle resets its
	// Pid to -1, and we still need it for the state file and the banner.
	pid = cmd.Process.Pid
	// The parent writes the state file synchronously so an immediately-following
	// `stop` always finds the PID (the child writes its own watcher state later).
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveDaemonState(daemonState{
		PID: pid, StartedAt: now, WatchDir: watchDir, LogPath: logPath,
	}); err != nil {
		return 0, "", false, err
	}
	_ = cmd.Process.Release()

	return pid, watchDir, false, nil
}

// StartTeamsDaemon is the `start` command: it spawns the detached supervisor via
// StartDaemon and prints the CLI banner. `stop` tears it down.
func StartTeamsDaemon(args []string) error {
	pid, watchDir, already, err := StartDaemon(args)
	if err != nil {
		return err
	}
	if already {
		fmt.Fprintf(os.Stderr, "promptster-teams: background capture already running (pid %d) — stop it with `promptster-teams stop`\n", pid)
		return nil
	}
	fmt.Fprintf(os.Stderr, "promptster-teams: capturing in background (pid %d) under %s → %s\n", pid, watchDir, ingest.APIHost())
	fmt.Fprintf(os.Stderr, "promptster-teams: logs at %s · stop with `promptster-teams stop`\n", daemonLogPath())
	return nil
}

// StopTeamsDaemon terminates the background capture recorded in THIS state dir's
// pidfiles (graceful SIGINT, then SIGKILL). The supervisor and both watcher
// pidfiles all live under StateDir(), so reading them to find PIDs is inherently
// scoped to this install — `stop` never reaches into another workspace's daemon.
// With `--force` it additionally runs an unscoped cmdline sweep for true orphans
// whose pidfiles were lost. Safe to run when nothing is running.
func StopTeamsDaemon(args []string) error {
	force := false
	for _, a := range args {
		if a == "--force" || a == "-f" {
			force = true
		}
	}

	// Collect candidate PIDs from every pidfile this install writes. The watchers
	// run as in-process goroutines under one `watch` PID, so the supervisor and
	// both watcher pidfiles usually point at the same process — the dedup set
	// handles that. Crucially, a daemon launched as a bare `watch` (e.g. the npm
	// binary, or an old `start`) writes only the watcher pidfiles and no
	// supervisor.json, so reading all three is the only reliable way to find it —
	// reading only supervisor.json silently misses it and `stop` becomes a no-op.
	seen := map[int]bool{}
	addPID := func(pid int) {
		if pid > 0 && pid != os.Getpid() {
			seen[pid] = true
		}
	}
	if st, err := loadDaemonState(); err == nil {
		addPID(st.PID)
	}
	if st, err := loadClaudeWatcherState(); err == nil {
		addPID(st.PID)
	}
	if st, err := loadCodexWatcherState(); err == nil {
		addPID(st.PID)
	}

	stopped := false
	for pid := range seen {
		// pidLooksLikeOurs guards against a stale pidfile whose PID the OS has
		// reused for an unrelated process — processExists only proves the number
		// is live, so without this a reused PID would get signaled by mistake.
		if processExists(pid) && pidLooksLikeOurs(pid) {
			signalAndWaitForExit(pid)
			stopped = true
		}
	}

	// Fallback for true orphans (every pidfile lost). This cmdline sweep is NOT
	// state-dir-scoped: with a different PROMPTSTER_STATE_DIR at stop time it
	// could match another workspace's daemon, so it is opt-in behind `--force`
	// and runs only when the scoped pidfile path found nothing live. The pattern
	// must match the real per-platform binary: the npm build is
	// `promptster-teams-darwin-arm64`, not `promptster-teams`, so the old exact
	// `promptster-teams watch` never matched it (pgrep -f takes a regex; `[^ ]*`
	// absorbs the `-darwin-arm64` suffix).
	if !stopped && force {
		swept := killStalePromptsterDaemons(`promptster-teams[^ ]* watch`)
		swept += killStalePromptsterDaemons(`promptster-teams[^ ]* claude-watch`)
		swept += killStalePromptsterDaemons(`promptster-teams[^ ]* codex-watch`)
		if swept > 0 {
			stopped = true
		}
	}

	// SIGINT/SIGKILL pre-empt the watchers' deferred state cleanup, so clear the
	// state files here — otherwise `status` would read stale until the next
	// liveness check self-heals them.
	clearDaemonState()
	clearClaudeWatcherState()
	clearCodexWatcherState()
	_ = os.Remove(claudeHookTakeoverPath())

	if stopped {
		fmt.Fprintln(os.Stderr, "promptster-teams: background capture stopped")
	} else if !force {
		fmt.Fprintln(os.Stderr, "promptster-teams: no tracked background capture was running — if one is running without a pidfile, retry with `promptster-teams stop --force`")
	} else {
		fmt.Fprintln(os.Stderr, "promptster-teams: no background capture was running")
	}
	return nil
}
