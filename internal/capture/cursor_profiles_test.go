package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestCursorUserDataDirs(t *testing.T) {
	client, _ := profileLogin(t, "auth0|a", "a@example.com")
	spaced, _ := profileLogin(t, "auth0|b", "b@example.com")
	dashed := filepath.Join(t.TempDir(), "Client -- Work")
	if err := os.MkdirAll(filepath.Dir(cursorProfileStateDB(dashed)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorProfileStateDB(dashed), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := cursorUserDataDirs([]string{
		"/Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir=" + client,
		"/Applications/Cursor.app/Contents/Frameworks/Cursor Helper (Renderer).app/Contents/MacOS/Cursor Helper (Renderer) --type=renderer --user-data-dir=" + client + " --lang=en",
		"/Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir " + spaced + " --new-window",
		"/Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir=" + dashed + " --lang=en",
		"/Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir=relative/dir",
		"/Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir=/no/such/profile",
		"/Applications/Visual Studio Code.app/Contents/MacOS/Electron --user-data-dir=" + client,
		"/Applications/Cursor.app/Contents/MacOS/Cursor",
	})
	want := []string{client, spaced, dashed}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// profileLogin puts a signed-in store in a --user-data-dir profile and returns
// the profile dir and its token.
func profileLogin(t *testing.T, sub, email string) (string, string) {
	t.Helper()
	token := jwtWithClaims(t, `{"sub":"`+sub+`","exp":4102444800}`)
	db := makeCursorStateDB(t, map[string]string{
		cursorAuthAccessTokenKey: token, cursorAuthRefreshTokenKey: token, cursorAuthCachedEmailKey: email,
	})
	dir := filepath.Join(t.TempDir(), "Client Profile")
	dest := cursorProfileStateDB(dir)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(db, dest); err != nil {
		t.Fatal(err)
	}
	return dir, token
}

func runningCursor(t *testing.T, dirs ...string) {
	t.Helper()
	prev := cursorProcessArgs
	t.Cleanup(func() { cursorProcessArgs = prev })
	cursorProcessArgs = func() []string {
		var lines []string
		for _, d := range dirs {
			lines = append(lines, "/Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir="+d)
		}
		return lines
	}
}

// A second login in a running --user-data-dir profile is collected and
// attributed with no setup, is still read after that Cursor quits, and is
// forgotten once its store is gone.
func TestCursorVendorExtraProfileLogin(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	resolver := permittedCursorPolicy(t)
	client, calls := fakeVendor(t)
	evs := captureVendorEvents(t)
	ide := ideLogin(t, "auth0|ide_login", "ide@example.com", 4102444800)
	dir, profile := profileLogin(t, "auth0|client_login", "client@example.com")

	runningCursor(t, dir)
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	if refs := refsOf(currentCycleComplete(*evs)); !refs[cursorAccountRef(ide)] || !refs[cursorAccountRef(profile)] || len(refs) != 2 {
		t.Fatalf("collected %v, want the IDE and the profile login", refs)
	}
	if ref, _ := cursorHookAccountRef(stopPayload(t, "client@example.com")); ref != cursorAccountRef(profile) {
		t.Fatalf("profile turn ref=%q, want %q", ref, cursorAccountRef(profile))
	}

	runningCursor(t) // that Cursor quit
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	if calls[profile] < 2 {
		t.Fatalf("calls=%v, want the remembered profile read again", calls)
	}

	// A store that is briefly gone is skipped, not forgotten.
	if err := os.Rename(dir, dir+".away"); err != nil {
		t.Fatal(err)
	}
	before := calls[profile]
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	if calls[profile] != before {
		t.Fatalf("calls=%v, want the missing store skipped", calls)
	}
	if err := os.Rename(dir+".away", dir); err != nil {
		t.Fatal(err)
	}
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	if calls[profile] <= before {
		t.Fatalf("calls=%v, want the profile read again once its store is back", calls)
	}
}

// The hook's scan remembers a profile that is running during a turn, so the
// next poll reads it after that Cursor has quit.
func TestRememberRunningCursorProfilesForLaterPoll(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ideLogin(t, "auth0|ide", "ide@example.com", 4102444800)
	dir, _ := profileLogin(t, "auth0|short", "short@example.com")
	runningCursor(t, dir)
	rememberRunningCursorProfiles()
	runningCursor(t)
	if got := cursorExtraProfileStateDBs(); !slices.Equal(got, []string{cursorProfileStateDB(dir)}) {
		t.Fatalf("got %v, want the profile seen by the hook", got)
	}
}

// The default profile named explicitly is not read twice.
func TestCursorExtraProfileSkipsDefault(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	dir, _ := profileLogin(t, "auth0|x", "x@example.com")
	t.Setenv(cursorStateDBEnv, cursorProfileStateDB(dir))
	runningCursor(t, dir)
	if got := cursorExtraProfileStateDBs(); len(got) != 0 {
		t.Fatalf("got %v, want the default store excluded", got)
	}
	if got := rememberCursorProfiles(nil); len(got) != 0 {
		t.Fatalf("default dir remembered: %v", got)
	}
}

// The hook and the daemon write the list at the same time without losing
// each other's additions.
func TestRememberCursorProfilesConcurrentWritersKeepEveryDir(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	t.Setenv(cursorStateDBEnv, filepath.Join(t.TempDir(), "default.vscdb"))
	var wg sync.WaitGroup
	for i := 0; i < cursorProfilesMax; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rememberCursorProfiles([]string{fmt.Sprintf("/profiles/p%d", i)})
		}(i)
	}
	wg.Wait()
	if got := rememberCursorProfiles(nil); len(got) != cursorProfilesMax {
		t.Fatalf("remembered %d of %d: %v", len(got), cursorProfilesMax, got)
	}
}

// Past the cap, the least recently seen profile is dropped.
func TestRememberCursorProfilesEvictsOldest(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	t.Setenv(cursorStateDBEnv, filepath.Join(t.TempDir(), "default.vscdb"))
	base := time.Now().Add(-time.Hour)
	for i := 0; i <= cursorProfilesMax; i++ {
		dir := fmt.Sprintf("/profiles/p%d", i)
		rememberCursorProfiles([]string{dir})
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(cursorProfilesDir(), cursorProfileFileName(dir)), at, at); err != nil {
			t.Fatal(err)
		}
	}
	got := rememberCursorProfiles(nil)
	if len(got) != cursorProfilesMax || got[0] != fmt.Sprintf("/profiles/p%d", cursorProfilesMax) || slices.Contains(got, "/profiles/p0") {
		t.Fatalf("got %v, want the newest %d, p0 evicted", got, cursorProfilesMax)
	}
}
