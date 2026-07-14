package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
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

// newServiceManager builds the autostart manager StopTeamsDaemon disarms. It is
// a var so tests can inject a manager whose Stop() really terminates a process:
// `stop`'s reporting hinges on sampling watcher liveness BEFORE the service stop
// kills it, and that ordering can't be exercised against a no-op manager.
var newServiceManager = service.New

func daemonStatePath() string { return filepath.Join(state.StateDir(), "supervisor.json") }
func daemonLogPath() string   { return filepath.Join(state.StateDir(), "daemon.log") }

// DaemonLogPath exposes the supervisor's log path so diagnostics elsewhere can
// name it. The outbox writes its delivery failures to stderr, which lands here
// in detached mode — so this is the file to send an engineer to when the send
// queue is stuck.
func DaemonLogPath() string { return daemonLogPath() }

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
	// A watcher launched some other way (foreground `watch` or the autostart
	// service) holds the single-instance lock but never wrote supervisor.json —
	// treat it as already running so we don't spawn a child that would just hit
	// the lock and exit. Idempotent, like the DaemonStatus check above; callers
	// render their own UX.
	if p, running := watchRunning(); running {
		return p, "", true, nil
	}

	token, apiURL, watchDir, noAutoUpdate, err := resolveWatchEnv(args)
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
	// The detached child re-runs a bare `watch` (no argv), so a --no-auto-update
	// passed to `start` would be lost — propagate it via the env opt-out the
	// updater also honors, which the inherited environment (and any later
	// in-place re-exec) carries forward.
	if noAutoUpdate {
		_ = os.Setenv("PROMPTSTER_TEAMS_NO_AUTO_UPDATE", "1")
	}

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
// It deliberately does NOT fall back to a global cmdline sweep: a pgrep over all
// `promptster-teams … watch` processes is not tied to this state dir, so it could
// terminate another workspace's daemon. Safe to run when nothing is running.
func StopTeamsDaemon() error {
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

	// Resolve which PIDs are live and ours BEFORE touching the service. Stopping
	// the service kills the watcher it owns, so a liveness check afterwards would
	// find nothing and `stop` would report "nothing was running" about capture it
	// had just stopped itself — sending the user off to hunt with pgrep for a
	// process that no longer exists.
	//
	// pidLooksLikeOurs guards against a stale pidfile whose PID the OS has reused
	// for an unrelated process — processExists only proves the number is live, so
	// without this a reused PID would get signaled by mistake.
	var targets []int
	for pid := range seen {
		if processExists(pid) && pidLooksLikeOurs(pid) {
			targets = append(targets, pid)
		}
	}
	// The capture flock is the authoritative liveness signal: it catches a live
	// watcher whose pidfile is missing or stale, and (unlike a PID) a dead
	// holder's lock is released by the kernel, so it can't report a phantom.
	_, watcherAlive := watchRunning()
	wasRunning := len(targets) > 0 || watcherAlive

	// Disarm the OS supervisor before signaling. When autostart is enabled the
	// watcher belongs to launchd/systemd, and their restart policies read the
	// SIGKILL escalation below as a crash — so a `stop` that escalated would
	// report success and then watch capture come back seconds later (launchd
	// respawns almost immediately; ThrottleInterval caps the restart *rate*, it
	// does not delay the first restart). Stopping the service — not disabling it
	// — unloads the job now and leaves it registered for next login. Stop is a
	// no-op when autostart isn't installed, so it needs no guard here.
	// Best-effort: a supervisor we can't reach must not block killing the process.
	if err := newServiceManager().Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "promptster-teams: warning: could not stop the autostart service (%v) — it may restart capture\n", err)
	}

	for _, pid := range targets {
		// Stopping the service usually took the watcher down already; skip the
		// ones that are already gone rather than burning the grace window on them.
		if processExists(pid) {
			signalAndWaitForExit(pid)
		}
	}

	// SIGINT/SIGKILL pre-empt the watchers' deferred state cleanup, so clear the
	// state files here — otherwise `status` would read stale until the next
	// liveness check self-heals them.
	clearDaemonState()
	clearClaudeWatcherState()
	clearCodexWatcherState()
	_ = os.Remove(claudeHookTakeoverPath())

	// Report the outcome we can observe, not the one we intended — this command
	// exists because a `stop` that reports success while capture is still running
	// (or comes back) is worse than one that admits it failed.
	switch _, stillAlive := watchRunning(); {
	case stillAlive:
		fmt.Fprintln(os.Stderr, "promptster-teams: warning: capture is STILL running after stop — find it with `pgrep -fl promptster-teams` and stop it manually")
	case wasRunning:
		fmt.Fprintln(os.Stderr, "promptster-teams: background capture stopped")
	default:
		fmt.Fprintln(os.Stderr, "promptster-teams: no tracked background capture was running — if one is running without a pidfile, find it with `pgrep -fl promptster-teams` and stop it manually")
	}
	return nil
}
