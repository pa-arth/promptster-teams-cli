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

// THE COOLDOWN MUST STAY REACHABLE FROM ONE LONG-LIVED UPDATER.
//
// The updater outlives every poll, and its stamp cache is what stops an
// unchanged file buying a subprocess. Caching a target that IS newer suppresses
// every later poll for that file, which makes the guard's six-hour cooldown
// unreachable in the only situations it exists for: an exec that FAILED
// (ETXTBSY while an installer is still writing, a permission fault) or one the
// guard BLOCKED. The daemon then stays stale for the life of the process, which
// on this product is weeks.
//
// TestCatchupGuardStopsARestartLoop cannot catch this: it builds a fresh updater
// per attempt and changes the stamp between them, so it never exercises the
// cache at all. Caught by review on PR #139.
func TestCatchupRetriesAfterTheCooldownEvenThoughTheFileNeverChanged(t *testing.T) {
	w := newDiskWorld("0.12.2", "0.13.0")
	u := w.updater("0.12.2", nil, false) // ONE updater for every poll, as in production
	u.reexec = func(p string) error {
		w.execd = append(w.execd, p)
		return fmt.Errorf("exec failed: text file busy")
	}

	if got := u.catchUpToDisk(); got != catchupNone {
		t.Fatalf("failed exec = %v, want catchupNone", got)
	}
	// Several more polls inside the cooldown, file untouched: blocked, not silent.
	for i := 0; i < 3; i++ {
		if got := u.catchUpToDisk(); got != catchupBlockedRetry {
			t.Fatalf("poll %d inside the cooldown = %v, want catchupBlockedRetry — the stamp cache swallowed the decision", i, got)
		}
	}
	if len(w.execd) != 1 {
		t.Fatalf("re-exec calls inside the cooldown = %v, want exactly one", w.execd)
	}

	// Past the cooldown, still the same unchanged file on disk: it must try again.
	// The verdict is catchupNone because this fake exec fails a second time too —
	// the evidence that the retry happened is that the exec was ATTEMPTED.
	w.now = w.now.Add(catchupCooldown + time.Minute)
	u.catchUpToDisk()
	if len(w.execd) != 2 {
		t.Fatalf("re-exec calls past the cooldown = %v, want two — the daemon would stay stale for the life of the process", w.execd)
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

// catchupCandidates answers "which files on disk may this daemon re-exec into",
// in the order they win. Each row is a state observed in the field.
func TestCatchupCandidatesPreferOwnPathThenTheManagedOne(t *testing.T) {
	const (
		self      = "/usr/local/lib/node_modules/@promptster/teams-cli/binaries/promptster-teams-darwin-arm64"
		local     = "/Users/dev/work/app/node_modules/@promptster/teams-cli/binaries/promptster-teams-darwin-arm64"
		canonical = "/Users/dev/.promptster-teams/bin/promptster-teams"
	)
	cases := []struct {
		name   string
		self   string
		on     []string
		want   []string
		reason string
	}{
		{"both present — own path first, managed as fallback", self, []string{self, canonical}, []string{self, canonical},
			"an in-place replacement wins, but a stale baked path must not trap the daemon forever"},
		{"orphaned — only the managed path is left", self, []string{canonical}, []string{canonical},
			"npm dropped the layout we were started from; nothing will ever replace that file again"},
		{"nothing on disk at all", self, []string{}, nil,
			"no candidate"},
		{"project-local never reaches the managed binary", local, []string{local, canonical}, []string{local},
			"jumping a lockfile-pinned copy into the shared binary is what the lockfile refuses"},
		{"we ARE the managed path", canonical, []string{canonical}, []string{canonical},
			"the fallback is the same file — listing it twice would just double the probes"},
	}
	for _, c := range cases {
		on := map[string]bool{}
		for _, p := range c.on {
			on[p] = true
		}
		got := catchupCandidates(c.self, canonical, func(p string) bool { return on[p] })
		if len(got) != len(c.want) {
			t.Errorf("%s: candidates = %v, want %v (%s)", c.name, got, c.want, c.reason)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: candidates = %v, want %v (%s)", c.name, got, c.want, c.reason)
				break
			}
		}
	}
}

// THE SHAPE THAT WAS ACTUALLY REPORTED. autostart bakes an absolute path at
// enable time and nothing revisits it, so launchd keeps starting the daemon from
// a file that still exists, is stale, and that no installer will ever touch
// again — while the managed binary on the same machine is current. Following
// only our own path leaves that daemon stale forever.
func TestCatchupFollowsTheManagedBinaryWhenOurOwnPathIsStaleButPresent(t *testing.T) {
	const baked = "/usr/local/lib/node_modules/@promptster/teams-cli/binaries/promptster-teams-darwin-arm64"
	w := newDiskWorld("0.12.2", "0.12.2")
	w.self = baked
	w.versions = map[string]string{baked: "0.12.2", w.canon: "0.13.0"}
	w.stamps = map[string]string{baked: "s-baked", w.canon: "s-canon"}

	if got := w.updater("0.12.2", nil, false).catchUpToDisk(); got != catchupGo {
		t.Fatalf("verdict = %v, want catchupGo", got)
	}
	if len(w.execd) != 1 || w.execd[0] != w.canon {
		t.Fatalf("re-exec calls = %v, want [%s] — the stale baked path trapped the daemon", w.execd, w.canon)
	}
}

// Our own path still wins when IT is the newer one: an installer that replaced
// our file in place is the most direct statement of intent on the machine.
func TestCatchupPrefersOurOwnPathWhenItIsTheNewerOne(t *testing.T) {
	const other = "/opt/promptster/bin/promptster-teams"
	w := newDiskWorld("0.12.2", "0.13.0")
	w.self = other
	w.versions = map[string]string{other: "0.13.0", w.canon: "0.14.0"}
	w.stamps = map[string]string{other: "s-self", w.canon: "s-canon"}

	w.updater("0.12.2", nil, false).catchUpToDisk()
	if len(w.execd) != 1 || w.execd[0] != other {
		t.Fatalf("re-exec calls = %v, want [%s]", w.execd, other)
	}
	if len(w.probed) != 1 {
		t.Errorf("probed %v — the managed path should not be probed once our own path already wins", w.probed)
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
