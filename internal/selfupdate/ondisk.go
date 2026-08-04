package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// THE DAEMON MUST BE ABLE TO CATCH UP WITHOUT THE INTERNET AND WITHOUT A HUMAN.
//
// checkAndApply compares the RUNNING version against a GitHub release tag. Until
// this file existed that was the ONLY thing that could ever move a live daemon
// forward, which left it blind to the single most common way it goes stale: a
// newer binary arriving on its own disk by some other route — `npm i -g`,
// install.sh, an MDM push, or another invocation of this CLI swapping the shared
// managed path. In every one of those cases the machine already HAS the new
// build and the daemon keeps executing an old image indefinitely, because
// nothing outside a process can change the code that process is running.
//
// That is the root cause behind every "doctor is green but the daemon is months
// old" report: the fix was already installed, and the only things that would
// have picked it up were a reboot or a human typing `stop && start`.
//
// So once per poll this compares our own version against the version of the
// binary ON DISK and re-execs into it when disk is strictly newer. It downloads
// nothing and verifies no signature, because it installs nothing: the bytes are
// already on the machine, put there by whoever administers it.
//
// WHY THIS IS DELIBERATELY NOT GATED ON THE AUTO-UPDATE SWITCH, and must not
// become so: `--no-auto-update`, the org's AutoUpdateEnabled and its
// PinnedCliVersion all govern what we FETCH AND INSTALL, and catch-up installs
// nothing. An org that turns auto-update off does it to control which build
// lands on its machines — and then lands one. Refusing to execute that build
// would mean their fleet can never move without a reboot, which is a strictly
// worse version of the bug this closes. A pin's enforcement point is the
// download, and it stays there: the file on disk is already the administrator's
// answer to "which version", and after the next reboot it is the version running
// anyway. All catch-up changes is whether they wait for the reboot.
//
// The one gate it does keep is the STAMPED-BUILD gate. "dev" and "" parse as
// 0.0.0 (see parseVersion), so without it a developer's own watcher would exec
// any release binary sitting in the managed path, and a released daemon would
// exec a locally-built `dev` binary and permanently strand itself on unreleased
// code.

// EnvHandoff marks a process spawned to REPLACE a still-running watcher, rather
// than one started independently.
//
// It exists for Windows, which has no execve: reexecInto there spawns a child
// and exits, so for a few milliseconds two processes exist and the child races
// the parent for the single-instance lock. Bowing out of that race means the
// machine captures nothing until the next login, so a marked child WAITS for the
// lock instead. An unmarked second `watch` still fails fast, which is what makes
// this an env marker and not a blanket retry: a human double-starting capture
// should still be told immediately.
//
// Set only by the Windows reexecInto; unix replaces the image in place and never
// needs it.
const EnvHandoff = "PROMPTSTER_TEAMS_HANDOFF"

const (
	// catchupCooldown bounds a catch-up that does not take.
	//
	// The termination argument for the normal path is structural: after the
	// re-exec our version IS the disk version, so IsNewer is false and we never
	// look again. That argument depends on the binary's `--version` output
	// agreeing with the version it runs as. If it ever does not — a wrapper
	// script, a stale hardlink, an exec that silently lands somewhere else — the
	// process comes back up still older than disk and re-execs again, forever.
	//
	// A restart loop is far worse here than anywhere else in this CLI, because
	// every watcher restart re-seeds the Cursor rail: transcripts already on disk
	// at the first poll are seeded to EOF, so a daemon bouncing every five minutes
	// silently drops the opening prompt of every session in between. That is
	// data loss, not just churn — hence a persisted guard rather than a comment
	// asserting the loop cannot happen.
	catchupCooldown = 6 * time.Hour

	// versionProbeTimeout bounds the `--version` subprocess. It runs on the
	// update goroutine, so a binary that hangs must not take auto-update with it.
	versionProbeTimeout = 10 * time.Second
)

// catchupTarget is a binary on disk and the version it reports.
type catchupTarget struct {
	Path    string
	Version string
}

// catchupGuard is the on-disk record of the last catch-up this machine
// attempted. It is written BEFORE the exec, because on unix the exec never
// returns and a record written after it would never be written at all.
type catchupGuard struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	At      string `json:"at"`
}

// catchupVerdict is the decision, split out so the policy is testable without a
// filesystem or a process replacement.
type catchupVerdict int

const (
	catchupNone         catchupVerdict = iota // nothing newer on disk (the answer approximately always)
	catchupGo                                 // re-exec into the target
	catchupBlockedRetry                       // this exact target was already tried and did not take
)

// stampedVersion reports whether a version string can be ordered against
// another. An unstamped build parses as 0.0.0, which would make every release
// look newer than a developer's working copy.
func stampedVersion(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "dev"
}

// pickCatchupPath chooses WHICH file on disk this daemon ought to be running.
//
// The invariant is deliberately narrow — "run the binary at your own path" —
// because anything wider is a policy decision about which install wins, and this
// code is in no position to make one:
//
//   - our own path still exists: it is the answer, whatever else is on the
//     machine. An installer that replaced it in place is telling us to run it,
//     and that includes a project-local copy someone just `npm ci`'d. The
//     project-local gate in checkAndApply blocks US overwriting THEIR pin; it has
//     no bearing on executing the pinned version they installed themselves.
//   - our own path is GONE: we are an orphan holding a deleted inode, which is a
//     real and observed state (an `npm i -g` that drops the layout the daemon was
//     started from — see the autostart section of AGENTS.md). Nothing will ever
//     replace that file, so following it is following a corpse. The managed path
//     is the one place every installer agrees on, so fall back to it.
//   - orphaned AND project-local: no fallback. Jumping from a lockfile-pinned
//     copy into the shared managed binary is exactly the version the lockfile
//     exists to refuse.
func pickCatchupPath(self, canonical string, exists func(string) bool) string {
	if self == "" {
		return ""
	}
	if exists(self) {
		return self
	}
	if canonical == "" || samePath(self, canonical) || isProjectLocalInstall(self) {
		return ""
	}
	if !exists(canonical) {
		return ""
	}
	return canonical
}

// decideCatchup is the whole policy, as a pure function.
func decideCatchup(running string, t catchupTarget, guard catchupGuard, now time.Time) catchupVerdict {
	if t.Path == "" || !stampedVersion(running) || !stampedVersion(t.Version) {
		return catchupNone
	}
	if !IsNewer(running, t.Version) {
		return catchupNone
	}
	// We are older than disk AND we already tried to become exactly this. The
	// exec did not take; trying again on the next poll is a restart loop.
	if guard.Path == t.Path && guard.Version == t.Version {
		if at, err := time.Parse(time.RFC3339, guard.At); err == nil && now.Sub(at) < catchupCooldown {
			return catchupBlockedRetry
		}
	}
	return catchupGo
}

// catchupGuardPath sits beside the update cursor, for the same reason: it
// describes the machine's install, not one watched workspace.
func catchupGuardPath() string {
	return filepath.Join(state.GlobalPromptsterDir(), "last-catchup")
}

func loadCatchupGuard() catchupGuard {
	raw, err := os.ReadFile(catchupGuardPath())
	if err != nil {
		return catchupGuard{}
	}
	var g catchupGuard
	if err := json.Unmarshal(raw, &g); err != nil {
		return catchupGuard{}
	}
	return g
}

func saveCatchupGuard(g catchupGuard) {
	p := catchupGuardPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	raw, err := json.Marshal(g)
	if err != nil {
		return
	}
	_ = os.WriteFile(p, raw, 0o600)
}

// probeBinaryVersion asks a binary what version it is. `version`/`--version`
// prints version.Version and nothing else (internal/cli/cli.go), so the whole
// output is the answer.
func probeBinaryVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	// #nosec G204 -- path is either os.Executable() or state.CanonicalInstallBin(),
	// both of which are our own resolved install paths and never user input.
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// fileStamp identifies a file cheaply enough to check every poll. Probing costs
// a subprocess and the answer is "unchanged" approximately always, so the stat
// is what keeps this affordable at a 5-minute cadence.
func fileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
}

// catchUpToDisk re-execs this process into a strictly newer build already
// present on disk. On success under unix it DOES NOT RETURN.
//
// Every failure is best-effort and silent-ish: this runs on the auto-update
// goroutine inside a daemon nobody is watching, so it logs and gives up rather
// than disturbing capture.
func (u *updater) catchUpToDisk() catchupVerdict {
	self, err := u.resolveSelf()
	if err != nil {
		u.logf("selfupdate: catch-up: cannot resolve own path: %v", err)
		return catchupNone
	}
	path := pickCatchupPath(self, u.canonicalBin(), u.fileExists)
	if path == "" {
		return catchupNone
	}

	stamp := u.fileStamp(path)
	if stamp != "" && stamp == u.lastProbed {
		return catchupNone
	}
	diskVer, err := u.binVersionOf(path)
	if err != nil {
		u.logf("selfupdate: catch-up: cannot read version of %s: %v", path, err)
		return catchupNone
	}
	// Recorded whatever the verdict: a file we have judged does not need judging
	// again until it changes, and that includes one we decided not to run.
	u.lastProbed = stamp

	target := catchupTarget{Path: path, Version: diskVer}
	switch decideCatchup(u.currentVersion, target, u.loadGuard(), u.now()) {
	case catchupBlockedRetry:
		u.logf("selfupdate: catch-up to %s (%s) was already attempted and did not take — not retrying for %s", path, diskVer, catchupCooldown)
		return catchupBlockedRetry
	case catchupGo:
		u.saveGuard(catchupGuard{Path: path, Version: diskVer, At: u.now().UTC().Format(time.RFC3339)})
		u.logf("selfupdate: catch-up: running %s, %s on disk is %s — re-execing", u.currentVersion, path, diskVer)
		fmt.Fprintf(os.Stderr, "promptster-teams: capture is running %s but %s on disk is %s — restarting into it\n", u.currentVersion, path, diskVer)
		if err := u.reexec(path); err != nil {
			u.logf("selfupdate: catch-up re-exec %s: %v", path, err)
			return catchupNone
		}
		return catchupGo
	}
	return catchupNone
}
