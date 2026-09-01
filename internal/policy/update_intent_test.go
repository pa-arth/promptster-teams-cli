package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

func intentStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	return dir
}

func boolPtr(b bool) *bool { return &b }

// THE regression test for this file's reason to exist.
//
// An org sets autoUpdate:false so its security team reviews releases before they
// reach company laptops. readDiskCache discards the whole policy cache on ANY
// read or JSON error, after which autoUpdate reverts to its true default — so
// before the intent mirror, deleting or corrupting one file silently put
// unreviewed builds back on the fleet. The switch means nothing if a stray byte
// can flip it.
func TestCorruptPolicyCacheDoesNotReEnableAutoUpdate(t *testing.T) {
	dir := intentStateDir(t)

	// The org has stated: no self-update, pinned to v0.25.0.
	writeUpdateIntent(boolPtr(false), "v0.25.0")

	for _, tc := range []struct {
		name string
		body string
	}{
		{"corrupt json", "{not json"},
		{"truncated", `{"autoUpdate":fal`},
		{"empty", ""},
		{"zero fetchedAt", `{"autoUpdate":true,"fetchedAt":"0001-01-01T00:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, cacheFileName), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			r := NewResolver("PSE-TEST-KEY")
			if r.AutoUpdateEnabled() {
				t.Fatal("a damaged policy cache re-enabled auto-update on a fleet the org had turned it off for")
			}
			if got := r.PinnedCliVersion(); got != "v0.25.0" {
				t.Fatalf("pin = %q, want v0.25.0 to survive the damaged cache", got)
			}
			if !r.OrgManaged() {
				t.Fatal("the org's stated intent was lost, so the machine would fall back to asking the engineer")
			}
		})
	}

	// Deleting the cache outright is the same class of failure.
	if err := os.Remove(filepath.Join(dir, cacheFileName)); err != nil {
		t.Fatal(err)
	}
	if NewResolver("PSE-TEST-KEY").AutoUpdateEnabled() {
		t.Fatal("a deleted policy cache re-enabled auto-update")
	}
}

// Silence is not consent, in either direction. A machine no org has spoken about
// is UNMANAGED and keeps failing open — that is the solo install, and making it
// managed would freeze every fleet whose backend predates the autoUpdate field.
func TestNoStatedIntentMeansUnmanagedAndFailsOpen(t *testing.T) {
	intentStateDir(t)

	r := NewResolver("PSE-TEST-KEY")
	if r.OrgManaged() {
		t.Fatal("a machine with no stated org intent reported managed")
	}
	if !r.AutoUpdateEnabled() {
		t.Fatal("an unmanaged machine must fail OPEN — failing closed strands solo installs forever")
	}
}

// An explicit autoUpdate:TRUE also makes a machine managed. The org has spoken;
// it happens to have said yes. Getting this wrong would send a managed fleet
// down the consent path and ask individual engineers a question their security
// team already answered.
func TestExplicitTrueStillCountsAsManaged(t *testing.T) {
	intentStateDir(t)
	writeUpdateIntent(boolPtr(true), "")

	r := NewResolver("PSE-TEST-KEY")
	if !r.OrgManaged() {
		t.Fatal("an explicit autoUpdate:true did not register as an org intent")
	}
	if !r.AutoUpdateEnabled() {
		t.Fatal("autoUpdate = false after the org explicitly enabled it")
	}
}

// A pin alone is an intent: an org that names the exact version its fleet runs
// is managing that fleet, whether or not it also set the switch.
func TestPinAloneMakesAMachineManaged(t *testing.T) {
	intentStateDir(t)
	writeUpdateIntent(nil, "v0.25.0")

	r := NewResolver("PSE-TEST-KEY")
	if !r.OrgManaged() {
		t.Fatal("a pinned fleet did not register as managed")
	}
}

// A response carrying NEITHER field must not erase a stated intent. A backend
// rollback, a partial outage, or an older deployment that does not know these
// fields has not withdrawn the org's decision — and treating it as a withdrawal
// re-enables self-update on a fleet configured off, which is the same failure
// the mirror exists to prevent, arriving by a different road.
func TestSilentResponseDoesNotEraseAStatedIntent(t *testing.T) {
	intentStateDir(t)
	writeUpdateIntent(boolPtr(false), "v0.25.0")

	writeUpdateIntent(nil, "") // a backend that says nothing about updates

	m, ok := readUpdateIntent()
	if !ok {
		t.Fatal("the stated intent was erased by a response that said nothing")
	}
	if m.AutoUpdate == nil || *m.AutoUpdate {
		t.Fatalf("autoUpdate = %v, want the stored false to survive", m.AutoUpdate)
	}
	if m.PinnedCliVersion != "v0.25.0" {
		t.Fatalf("pin = %q, want v0.25.0 to survive", m.PinnedCliVersion)
	}
}

// A successful fetch is authoritative and overwrites the mirror, including
// turning self-update back ON. Withdrawal has to be possible — it just has to be
// explicit.
func TestExplicitReEnableOverwritesTheMirror(t *testing.T) {
	intentStateDir(t)
	writeUpdateIntent(boolPtr(false), "v0.25.0")
	writeUpdateIntent(boolPtr(true), "")

	r := NewResolver("PSE-TEST-KEY")
	if !r.AutoUpdateEnabled() {
		t.Fatal("an explicit re-enable did not take effect")
	}
}

// THE P1 regression (Greptile, PR #206). The mirror must never OVERRIDE the
// cache — it fills gaps only.
//
// Both files are written by the same Refresh but independently, so the mirror is
// stale whenever its write failed and the cache's succeeded. The dangerous
// ordering is precisely the one the mirror exists to protect: an org disables
// self-update, writeUpdateIntent(false) fails, writeDiskCache(false) succeeds,
// and on the next start a mirror still saying `true` would turn self-update back
// ON for the fleet that just disabled it.
func TestStaleMirrorNeverOverridesTheCache(t *testing.T) {
	dir := intentStateDir(t)

	// The mirror is stale: it still holds the org's OLD "yes".
	writeUpdateIntent(boolPtr(true), "")

	// The cache holds the org's CURRENT answer — no.
	fresh, err := json.Marshal(diskCache{AutoUpdate: boolPtr(false), FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName), fresh, 0o600); err != nil {
		t.Fatal(err)
	}

	if NewResolver("PSE-TEST-KEY").AutoUpdateEnabled() {
		t.Fatal("a stale mirror re-enabled auto-update over a cache that said the org disabled it")
	}
}

// The mirror still fills a gap the cache leaves. A valid cache from a backend
// that sends no autoUpdate field says nothing about the org's intent, so a
// previously stated one must survive — silence is not withdrawal.
func TestMirrorFillsAGapTheCacheLeaves(t *testing.T) {
	dir := intentStateDir(t)
	writeUpdateIntent(boolPtr(false), "v0.25.0")

	// A valid, fresh cache that simply carries no autoUpdate (older backend).
	fresh, err := json.Marshal(diskCache{FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName), fresh, 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewResolver("PSE-TEST-KEY")
	if r.AutoUpdateEnabled() {
		t.Fatal("the org's stated 'no' was lost to a cache that never mentioned autoUpdate")
	}
	if got := r.PinnedCliVersion(); got != "v0.25.0" {
		t.Fatalf("pin = %q, want the mirrored v0.25.0 to fill the cache's gap", got)
	}
	// Sanity: the mirror really is under the state dir the test redirected.
	if _, err := os.Stat(filepath.Join(state.StateDir(), updateIntentFileName)); err != nil {
		t.Fatalf("intent mirror not written to the state dir: %v", err)
	}
}

// A cache that explicitly re-enables updates IS authoritative — withdrawal is
// possible, it just has to be explicit and come from a successful fetch.
func TestExplicitCacheReEnableBeatsAnOlderMirroredNo(t *testing.T) {
	dir := intentStateDir(t)
	writeUpdateIntent(boolPtr(false), "")

	fresh, err := json.Marshal(diskCache{AutoUpdate: boolPtr(true), FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName), fresh, 0o600); err != nil {
		t.Fatal(err)
	}

	if !NewResolver("PSE-TEST-KEY").AutoUpdateEnabled() {
		t.Fatal("an explicit re-enable in the cache was overridden by an older mirrored no")
	}
}
