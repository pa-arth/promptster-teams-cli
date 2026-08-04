package selfupdate

import (
	"fmt"
	"testing"
	"time"
)

// diskWorld fakes every edge catchUpToDisk touches, so the policy is exercised
// with no filesystem, no subprocess and no process replacement.
type diskWorld struct {
	self     string
	canon    string
	versions map[string]string // path -> what `--version` prints ("" = probe fails)
	stamps   map[string]string // path -> fileStamp; missing = file does not exist
	guard    catchupGuard
	now      time.Time

	probed []string
	saved  []catchupGuard
	execd  []string
}

func (w *diskWorld) updater(running string, pol PolicyView, noAuto bool) *updater {
	return &updater{
		currentVersion: running,
		noAutoUpdate:   noAuto,
		policy:         pol,
		resolveSelf:    func() (string, error) { return w.self, nil },
		canonicalBin:   func() string { return w.canon },
		fileExists: func(p string) bool {
			_, ok := w.stamps[p]
			return ok
		},
		fileStamp: func(p string) string { return w.stamps[p] },
		binVersionOf: func(p string) (string, error) {
			w.probed = append(w.probed, p)
			v, ok := w.versions[p]
			if !ok || v == "" {
				return "", fmt.Errorf("probe failed: %s", p)
			}
			return v, nil
		},
		loadGuard: func() catchupGuard { return w.guard },
		saveGuard: func(g catchupGuard) {
			w.saved = append(w.saved, g)
			w.guard = g
		},
		reexec: func(p string) error {
			// Production never returns from this. Recording the call is what
			// stands in for "the process became that binary".
			w.execd = append(w.execd, p)
			return nil
		},
		logf: func(string, ...any) {},
		now:  func() time.Time { return w.now },
	}
}

func newDiskWorld(selfVersion, diskVersion string) *diskWorld {
	const self = "/opt/promptster/bin/promptster-teams"
	return &diskWorld{
		self:     self,
		canon:    "/home/dev/.promptster-teams/bin/promptster-teams",
		versions: map[string]string{self: diskVersion},
		stamps:   map[string]string{self: "stamp-1"},
		now:      time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
	}
}

// The whole point: a newer build already sitting on disk gets EXECUTED, with no
// network call and nobody typing anything.
func TestCatchupReexecsIntoANewerBinaryOnDisk(t *testing.T) {
	w := newDiskWorld("0.12.2", "0.13.0")
	if got := w.updater("0.12.2", nil, false).catchUpToDisk(); got != catchupGo {
		t.Fatalf("verdict = %v, want catchupGo", got)
	}
	if len(w.execd) != 1 || w.execd[0] != w.self {
		t.Fatalf("re-exec calls = %v, want exactly [%s]", w.execd, w.self)
	}
}

// Equal and older must be no-ops. Older especially: a daemon that self-updated
// forward is routinely NEWER than a binary an installer just laid down, and
// executing that would be a silent downgrade of the process that reports data.
func TestCatchupNeverRunsAnEqualOrOlderDiskBuild(t *testing.T) {
	for _, disk := range []string{"0.12.2", "0.11.9"} {
		w := newDiskWorld("0.12.2", disk)
		if got := w.updater("0.12.2", nil, false).catchUpToDisk(); got != catchupNone {
			t.Errorf("disk %s: verdict = %v, want catchupNone", disk, got)
		}
		if len(w.execd) != 0 {
			t.Errorf("disk %s: re-exec'd %v, want none", disk, w.execd)
		}
	}
}

// "dev" and "" parse as 0.0.0, so without an explicit stamped-build gate a
// developer's own watcher execs any release binary in the managed path, and a
// released daemon execs a locally-built dev binary and strands itself there —
// with no release ever able to look newer than 0.0.0 again.
func TestCatchupRefusesUnstampedBuildsOnEitherSide(t *testing.T) {
	cases := []struct{ running, disk string }{
		{"dev", "0.13.0"},
		{"", "0.13.0"},
		{"0.12.2", "dev"},
		{"0.12.2", ""},
	}
	for _, c := range cases {
		w := newDiskWorld(c.running, c.disk)
		if got := w.updater(c.running, nil, false).catchUpToDisk(); got == catchupGo {
			t.Errorf("running=%q disk=%q: verdict = catchupGo, want anything else", c.running, c.disk)
		}
		if len(w.execd) != 0 {
			t.Errorf("running=%q disk=%q: re-exec'd %v, want none", c.running, c.disk, w.execd)
		}
	}
}

// THE ANTI-LOOP GUARD. If a binary reports a version it does not come up as
// (a wrapper, a stale hardlink, an exec that lands elsewhere), the process is
// still older than disk on the next poll and would re-exec forever. Every bounce
// re-seeds the Cursor rail to EOF, so a 5-minute restart loop silently drops the
// opening prompt of every session in between — data loss, not churn.
func TestCatchupGuardStopsARestartLoopWhenTheExecDoesNotTake(t *testing.T) {
	w := newDiskWorld("0.12.2", "0.13.0")

	if got := w.updater("0.12.2", nil, false).catchUpToDisk(); got != catchupGo {
		t.Fatalf("first attempt = %v, want catchupGo", got)
	}
	// Simulate the pathological case: the exec "succeeded" but we came back up
	// still on 0.12.2, and the file has not changed since.
	w.stamps[w.self] = "stamp-2"
	if got := w.updater("0.12.2", nil, false).catchUpToDisk(); got != catchupBlockedRetry {
		t.Fatalf("second attempt = %v, want catchupBlockedRetry", got)
	}
	if len(w.execd) != 1 {
		t.Fatalf("re-exec calls = %v, want exactly one — the loop was not stopped", w.execd)
	}

	// The block is a cooldown, not a tombstone: past it we try once more, because
	// whatever prevented the exec may have been fixed.
	w.now = w.now.Add(catchupCooldown + time.Minute)
	w.stamps[w.self] = "stamp-3"
	if got := w.updater("0.12.2", nil, false).catchUpToDisk(); got != catchupGo {
		t.Fatalf("after cooldown = %v, want catchupGo", got)
	}

	// A DIFFERENT version is never blocked — the guard is about one failed
	// target, not about giving up on catching up.
	w.versions[w.self] = "0.14.0"
	w.stamps[w.self] = "stamp-4"
	if got := w.updater("0.12.2", nil, false).catchUpToDisk(); got != catchupGo {
		t.Fatalf("new version after a block = %v, want catchupGo", got)
	}
}

// On unix the exec never returns, so a guard written after it is a guard that is
// never written at all — and the anti-loop protection above would not exist.
func TestCatchupWritesTheGuardBeforeExecing(t *testing.T) {
	w := newDiskWorld("0.12.2", "0.13.0")
	u := w.updater("0.12.2", nil, false)
	u.reexec = func(p string) error {
		if len(w.saved) == 0 {
			t.Error("re-exec ran with no guard persisted — on unix this call never returns, so the record would never be written")
		}
		w.execd = append(w.execd, p)
		return nil
	}
	u.catchUpToDisk()
	if len(w.saved) != 1 || w.saved[0].Version != "0.13.0" || w.saved[0].Path != w.self {
		t.Fatalf("saved guard = %+v, want one naming %s 0.13.0", w.saved, w.self)
	}
}

// The probe is a subprocess and the answer is "unchanged" approximately always,
// so an unchanged file must not buy one every five minutes.
func TestCatchupProbesOnlyWhenTheFileChanges(t *testing.T) {
	w := newDiskWorld("0.12.2", "0.12.2")
	u := w.updater("0.12.2", nil, false)
	for i := 0; i < 4; i++ {
		u.catchUpToDisk()
	}
	if len(w.probed) != 1 {
		t.Fatalf("probes = %d (%v), want 1 — the stat gate is not holding", len(w.probed), w.probed)
	}
	w.stamps[w.self] = "stamp-changed"
	u.catchUpToDisk()
	if len(w.probed) != 2 {
		t.Fatalf("probes after the file changed = %d, want 2 — a changed binary must be re-probed", len(w.probed))
	}
}

// A DESIGN DECISION, PINNED SO IT IS NOT "FIXED" LATER. The auto-update switch,
// the org policy switch and the org pin all govern what we FETCH AND INSTALL.
// Catch-up installs nothing — it executes a build the administrator already put
// on the machine, and which is the version running after the next reboot anyway.
// Gating this on those switches means a fleet that manages its own versions can
// never move a daemon without a reboot, which is the bug this closes wearing a
// different hat.
func TestCatchupIsNotGatedOnTheAutoUpdateSwitchOrThePin(t *testing.T) {
	t.Setenv(envNoAutoUpdate, "1")
	pol := stubPolicy{enabled: false, pinned: "v0.12.2", min: ""}

	w := newDiskWorld("0.12.2", "0.13.0")
	if got := w.updater("0.12.2", pol, true).catchUpToDisk(); got != catchupGo {
		t.Fatalf("verdict = %v, want catchupGo — catch-up must not read the install-time switches", got)
	}
	if len(w.execd) != 1 {
		t.Fatalf("re-exec calls = %v, want one", w.execd)
	}
}

// pickCatchupPath answers "which file on disk should this daemon be running",
// and each branch is a state observed in the field.
func TestPickCatchupPathFollowsOwnPathThenFallsBackWhenOrphaned(t *testing.T) {
	const (
		self      = "/usr/local/lib/node_modules/@promptster/teams-cli/binaries/promptster-teams-darwin-arm64"
		local     = "/Users/dev/work/app/node_modules/@promptster/teams-cli/binaries/promptster-teams-darwin-arm64"
		canonical = "/Users/dev/.promptster-teams/bin/promptster-teams"
	)
	cases := []struct {
		name   string
		self   string
		on     []string
		want   string
		reason string
	}{
		{"own path exists — it is the answer", self, []string{self, canonical}, self,
			"an installer that replaced our own file in place is telling us to run it"},
		{"orphaned — fall back to the managed path", self, []string{canonical}, canonical,
			"npm dropped the layout we were started from; nothing will ever replace that file again"},
		{"orphaned with nothing to fall back to", self, []string{}, "",
			"no candidate at all"},
		{"orphaned project-local install stays put", local, []string{canonical}, "",
			"jumping a lockfile-pinned copy into the shared binary is what the lockfile refuses"},
		{"we ARE the managed path and it is gone", canonical, []string{}, "",
			"the fallback is the same file — following it is following a corpse"},
	}
	for _, c := range cases {
		on := map[string]bool{}
		for _, p := range c.on {
			on[p] = true
		}
		got := pickCatchupPath(c.self, canonical, func(p string) bool { return on[p] })
		if got != c.want {
			t.Errorf("%s: pickCatchupPath = %q, want %q (%s)", c.name, got, c.want, c.reason)
		}
	}
}

// A perfect implementation nobody calls fixes nothing, and the ordering is part
// of the point: catch-up is free and local, so it must not sit behind a network
// check that a firewalled or rate-limited machine may never get past.
func TestAutoUpdateLoopCatchesUpBeforeItAsksTheNetwork(t *testing.T) {
	w := newDiskWorld("0.12.2", "0.13.0")
	u := w.updater("0.12.2", nil, false)
	u.httpRedirect = func(string) (string, int, error) {
		t.Error("the update loop reached the network before catching up to the binary already on disk")
		return "", 0, nil
	}
	stop := make(chan struct{})
	close(stop)
	runAutoUpdate(u, stop)

	if len(w.execd) != 1 || w.execd[0] != w.self {
		t.Fatalf("re-exec calls = %v, want [%s] — catch-up is not wired into the update loop", w.execd, w.self)
	}
}

// A probe that fails must be silent, not a reason to do something. It is the
// state of a binary mid-rename, on a busy machine, or one that is not ours.
func TestCatchupDoesNothingWhenTheProbeFails(t *testing.T) {
	w := newDiskWorld("0.12.2", "")
	if got := w.updater("0.12.2", nil, false).catchUpToDisk(); got != catchupNone {
		t.Fatalf("verdict = %v, want catchupNone", got)
	}
	if len(w.execd) != 0 || len(w.saved) != 0 {
		t.Fatalf("a failed probe caused execs=%v saves=%v, want neither", w.execd, w.saved)
	}
}
