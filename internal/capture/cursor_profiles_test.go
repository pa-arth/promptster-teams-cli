package capture

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCursorUserDataDirs(t *testing.T) {
	got := cursorUserDataDirs([]string{
		"/Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir=/Users/a/cursor-client",
		"/Applications/Cursor.app/Contents/Frameworks/Cursor Helper (Renderer).app/Contents/MacOS/Cursor Helper (Renderer) --type=renderer --user-data-dir=/Users/a/cursor-client --lang=en",
		"/Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir /Users/a/Client Work/cursor --new-window",
		"/Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir=relative/dir",
		"/Applications/Visual Studio Code.app/Contents/MacOS/Electron --user-data-dir=/Users/a/vscode",
		"/Applications/Cursor.app/Contents/MacOS/Cursor",
	})
	want := []string{"/Users/a/cursor-client", "/Users/a/Client Work/cursor"}
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

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	if _, err := os.Stat(cursorProfilesPath()); !os.IsNotExist(err) {
		t.Fatalf("profiles file still present after its only store was removed: %v", err)
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
	if b, _ := os.ReadFile(cursorProfilesPath()); strings.Contains(string(b), dir) {
		t.Fatalf("default dir remembered: %s", b)
	}
}
