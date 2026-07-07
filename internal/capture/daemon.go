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

// StartTeamsDaemon spawns the transcript capture as a detached background
// process and returns immediately, freeing the shell. It re-runs this same
// binary as `watch` (which already owns credential export, signing-keypair
// bootstrap, presence, census, and both watchers) so the background path and
// the foreground `watch` share one code path. `stop` tears it down.
func StartTeamsDaemon(args []string) error {
	if pid, running := DaemonStatus(); running {
		fmt.Fprintf(os.Stderr, "promptster-teams: background capture already running (pid %d) — stop it with `promptster-teams stop`\n", pid)
		return nil
	}

	token, apiURL, watchDir, err := resolveWatchEnv(args)
	if err != nil {
		return err
	}

	logPath := daemonLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	// #nosec G304 -- logPath is daemonLogPath() derived from StateDir(), not user input.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return err
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
		return fmt.Errorf("could not start background capture: %w", err)
	}
	// Capture the PID before Release() — releasing the Process handle resets its
	// Pid to -1, and we still need it for the state file and the banner.
	pid := cmd.Process.Pid
	// The parent writes the state file synchronously so an immediately-following
	// `stop` always finds the PID (the child writes its own watcher state later).
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveDaemonState(daemonState{
		PID: pid, StartedAt: now, WatchDir: watchDir, LogPath: logPath,
	}); err != nil {
		return err
	}
	_ = cmd.Process.Release()

	fmt.Fprintf(os.Stderr, "promptster-teams: capturing in background (pid %d) under %s → %s\n", pid, watchDir, ingest.APIHost())
	fmt.Fprintf(os.Stderr, "promptster-teams: logs at %s · stop with `promptster-teams stop`\n", logPath)
	return nil
}

// StopTeamsDaemon terminates the background supervisor (graceful SIGINT, then
// SIGKILL) and sweeps any orphaned capture processes whose state file was lost.
// Safe to run when nothing is running.
func StopTeamsDaemon() error {
	stopped := false
	if st, err := loadDaemonState(); err == nil && st.PID > 0 && processExists(st.PID) {
		signalAndWaitForExit(st.PID)
		stopped = true
	}

	// Belt-and-suspenders: catch a supervisor (or a manually-launched watcher
	// subcommand) whose state file was overwritten or never written. The real
	// process cmdline is `<bin>/promptster-teams watch`, so the pattern must
	// include `-teams` to match (a bare `promptster watch` never would).
	swept := killStalePromptsterDaemons("promptster-teams watch")
	swept += killStalePromptsterDaemons("promptster-teams claude-watch")
	swept += killStalePromptsterDaemons("promptster-teams codex-watch")
	if swept > 0 {
		stopped = true
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
	} else {
		fmt.Fprintln(os.Stderr, "promptster-teams: no background capture was running")
	}
	return nil
}
