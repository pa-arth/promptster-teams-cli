package capture

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/selfupdate"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

// isOlderVersion reports whether running is a strictly older release than ours,
// using the SAME predicate that authorizes a self-update swap (selfupdate.IsNewer)
// — two answers to one question is how a diagnostic ends up arguing with the
// daemon it is diagnosing.
//
// Unstamped builds ("dev", "") are never compared. They parse as 0.0.0, so a
// release binary would read every locally-built daemon as ancient and restart a
// developer's watcher out from under them mid-test.
func isOlderVersion(running, ours string) bool {
	if running == "" || running == "dev" || ours == "" || ours == "dev" {
		return false
	}
	return selfupdate.IsNewer(running, ours)
}

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
//
// Either way it registers the resolved watch dir as a capture root, so a second
// `start` from an unrelated tree WIDENS the running daemon instead of quietly
// changing nothing. watchDir is returned on the already-running path too —
// callers need it to tell the engineer which directory is now covered.
type StartResult struct {
	PID      int
	WatchDir string
	// AlreadyRunning means a supervisor was live, so nothing was spawned — the
	// WatchDir was handed to it instead.
	AlreadyRunning bool
	// RootErr is set when WatchDir could NOT be registered as a capture root.
	// Callers must surface it instead of reporting the directory covered: a
	// failed registration that reads as success is the precise failure mode
	// capture roots exist to remove.
	RootErr error
	// StaleVersion is the version of a live daemon that is running an OLDER build
	// than this binary — empty when capture is current, was not running, or did
	// not record a version. Set only on the AlreadyRunning path, where "nothing
	// to do" was the wrong answer.
	StaleVersion string
}

// staleCapture reports the version of a live daemon running an older build than
// this binary, or "" when there is nothing to act on.
//
// Deliberately strict, in both directions. It answers "" when the daemon did not
// record a version (any build older than watch_runtime.go), because acting on a
// guess would restart working capture on every `start`. And it answers "" when
// the daemon is NEWER than us — that is an ordinary consequence of a stale
// foreground copy (an old `npx`, a second binary earlier in PATH), and killing a
// newer daemon to install an older one is a downgrade with extra steps.
func staleCapture(running string) string {
	if running == "" || !isOlderVersion(running, version.Version) {
		return ""
	}
	return running
}

func StartDaemon(args []string) (StartResult, error) {
	if p, running := DaemonStatus(); running {
		dir, rootErr := registerWatchDir()
		return StartResult{
			PID: p, WatchDir: dir, AlreadyRunning: true, RootErr: rootErr,
			StaleVersion: staleCapture(RunningCapture().Version),
		}, nil
	}
	// A watcher launched some other way (foreground `watch` or the autostart
	// service) holds the single-instance lock but never wrote supervisor.json —
	// treat it as already running so we don't spawn a child that would just hit
	// the lock and exit. Idempotent, like the DaemonStatus check above; callers
	// render their own UX.
	if p, running := watchRunning(); running {
		dir, rootErr := registerWatchDir()
		return StartResult{
			PID: p, WatchDir: dir, AlreadyRunning: true, RootErr: rootErr,
			StaleVersion: staleCapture(RunningCapture().Version),
		}, nil
	}

	token, apiURL, watchDir, noAutoUpdate, err := resolveWatchEnv(args)
	if err != nil {
		return StartResult{}, err
	}
	// Record the first root too, so the watched-directory list a later `start`
	// prints is the whole truth rather than only the additions.
	_, _, rootErr := RegisterCaptureRoot(watchDir)

	logPath := daemonLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return StartResult{}, err
	}
	// #nosec G304 -- logPath is daemonLogPath() derived from StateDir(), not user input.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return StartResult{}, err
	}
	defer logFile.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return StartResult{}, err
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

	// #nosec G204 -- re-execs our own running binary (state.SelfBin()); the subcommand is a constant.
	cmd := exec.Command(state.SelfBin(), "watch")
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Fully detach: a new session (unix) / process group (windows) so closing
	// the launching terminal doesn't deliver SIGHUP/Ctrl-Break to the daemon.
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		return StartResult{}, fmt.Errorf("could not start background capture: %w", err)
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
		return StartResult{}, err
	}
	_ = cmd.Process.Release()

	return StartResult{PID: pid, WatchDir: watchDir, RootErr: rootErr}, nil
}

// StartTeamsDaemon is the `start` command: it spawns the detached supervisor via
// StartDaemon and prints the CLI banner. `stop` tears it down.
func StartTeamsDaemon(args []string) error {
	res, err := StartDaemon(args)
	if err != nil {
		return err
	}
	// A live daemon on an OLDER build is the one case where "already running" was
	// the wrong answer. It is not a hypothetical: capture keeps running, doctor
	// stays green, and every feature that installs itself at watch startup (the
	// Cursor hook rail, most obviously) is silently missing — so `start`, the
	// command an engineer runs precisely because something is not working, used to
	// print a reassuring line and change nothing.
	//
	// Restart it. This is the ONE mutation `start` performs on a running daemon,
	// it is gated on a recorded version being strictly older than ours, and it is
	// exactly what the engineer would do by hand next.
	if res.AlreadyRunning && res.StaleVersion != "" {
		fmt.Fprintf(os.Stderr, "promptster-teams: capture (pid %d) is running %s but this binary is %s — restarting it\n", res.PID, res.StaleVersion, version.Version)
		if err := StopTeamsDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "promptster-teams: warning: could not stop the stale capture process: %v\n", err)
		}
		res, err = StartDaemon(args)
		if err != nil {
			return err
		}
	}
	// Re-arm the OS supervisor if it is installed but not loaded. `stop` disarms
	// it deliberately (otherwise launchd resurrects the watcher `stop` just
	// killed) and nothing used to arm it again, so every stop/start cycle — the
	// standard fix-it move, and the one the restart above performs — left capture
	// running unsupervised until the next login. Enable() also re-renders the
	// baked path, so this doubles as the `autostart repair` for anyone whose npm
	// postinstall never ran.
	rearmAutostart()
	if res.AlreadyRunning {
		if res.RootErr != nil {
			// Never claim coverage we do not have. This is the one line that
			// decides whether an engineer walks away believing their second tree
			// is captured.
			fmt.Fprintf(os.Stderr, "promptster-teams: background capture already running (pid %d), but %s could NOT be added to it: %v\n", res.PID, res.WatchDir, res.RootErr)
		} else {
			fmt.Fprintf(os.Stderr, "promptster-teams: background capture already running (pid %d) — capture now covers %s\n", res.PID, res.WatchDir)
		}
		printWatchedRoots(os.Stderr)
		fmt.Fprintf(os.Stderr, "promptster-teams: stop with `promptster-teams stop`\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "promptster-teams: capturing in background (pid %d) under %s → %s\n", res.PID, res.WatchDir, ingest.APIHost())
	if res.RootErr != nil {
		fmt.Fprintf(os.Stderr, "promptster-teams: warning: could not record %s for later `start` calls: %v\n", res.WatchDir, res.RootErr)
	}
	printWatchedRoots(os.Stderr)
	fmt.Fprintf(os.Stderr, "promptster-teams: logs at %s · stop with `promptster-teams stop`\n", daemonLogPath())
	return nil
}

// rearmAutostart re-registers an autostart unit that is installed but not
// currently loaded, and does nothing at all otherwise.
//
// It NEVER installs autostart for someone who has not enabled it — that would
// turn `start` into a consent-free enrollment in something that survives
// reboots. Installed-and-unloaded is the only state it touches, and that state
// is only reachable from our own `stop`.
//
// Best-effort by contract: capture is already running by the time this is
// called, and a supervisor we cannot reach is not a reason to report a failed
// start.
func rearmAutostart() {
	mgr := newServiceManager()
	st, err := mgr.Status()
	if err != nil || !st.Installed || st.Loaded {
		return
	}
	if err := mgr.Enable(); err != nil {
		fmt.Fprintf(os.Stderr, "promptster-teams: warning: autostart is installed but not loaded and could not be re-armed (%v) — run `promptster-teams autostart enable`\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "promptster-teams: autostart re-armed at %s\n", state.SelfBin())
}

// registerWatchDir adds the directory this invocation would have watched to the
// captured set and returns it. Used on the already-running paths, where the
// credential never has to be resolved — a second `start` should widen capture
// even from a shell with no key in scope, since the running daemon owns the
// credential.
func registerWatchDir() (string, error) {
	dir := watchDirFromEnv()
	_, _, err := RegisterCaptureRoot(dir)
	return dir, err
}

// printWatchedRoots names every directory now being captured. Widening capture
// is consent-shaped: `start` must never quietly extend what leaves the machine,
// so the full list is printed on every start, not just the directory added.
func printWatchedRoots(w io.Writer) {
	roots := RegisteredCaptureRoots()
	if len(roots) < 2 {
		return // a single root is already named by the line above
	}
	fmt.Fprintf(w, "promptster-teams: watching %d directories:\n", len(roots))
	for _, r := range roots {
		fmt.Fprintf(w, "promptster-teams:   %s\n", r)
	}
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
	clearWatchRuntime()
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
