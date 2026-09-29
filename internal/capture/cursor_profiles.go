package capture

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// openspec cursor-vendor-multi-account §2.1 — EXTRA CURSOR PROFILES.
//
// A second Cursor login is usually a second profile: Cursor started with
// `--user-data-dir <dir>`, whose store is `<dir>/User/globalStorage/state.vscdb`.
// The default-path read never sees it. Measured 2026-09-29: on CLI 0.36.0 one
// engineer's `unreadable:` login still ran ~100 turns a day, while
// cursor-agent's login was already read.
//
// NO SETUP. A profile the engineer works in has a running Cursor process, and
// Chromium passes `--user-data-dir=` to every helper process. So each cycle scans
// the process list, and remembers each profile dir it finds. That way the
// profile is still read in cycles where that Cursor window is closed, which
// matters because an account's usage list covers the whole billing cycle (D1).
// The file holds paths only, never a credential. A remembered dir whose store is
// gone is dropped.

const (
	cursorProfilesVersion = 1
	// ponytail: 8 remembered profiles, the most recently seen kept. Raise it if
	// anyone runs more.
	cursorProfilesMax     = 8
	cursorUserDataDirFlag = "--user-data-dir"
	cursorProcessScanWait = 2 * time.Second
)

type cursorProfiles struct {
	Version int      `json:"version"`
	Dirs    []string `json:"dirs"`
}

// cursorProcessArgs lists every process's command line. A var for tests.
var cursorProcessArgs = func() []string {
	ctx, cancel := context.WithTimeout(context.Background(), cursorProcessScanWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "args=").Output()
	if err != nil {
		return nil
	}
	return strings.Split(string(out), "\n")
}

func cursorProfilesPath() string {
	return filepath.Join(state.StateDir(), "cursor-profiles.json")
}

func cursorProfileStateDB(dir string) string {
	return filepath.Join(dir, "User", "globalStorage", "state.vscdb")
}

// cursorUserDataDirs pulls `--user-data-dir` values out of Cursor command lines.
// ps joins argv with spaces, so a value runs to the next ` --` flag. That keeps
// a dir with a space in its name intact. Relative dirs are skipped.
func cursorUserDataDirs(lines []string) []string {
	var dirs []string
	seen := map[string]bool{}
	for _, line := range lines {
		if !strings.Contains(line, "Cursor") {
			continue
		}
		i := strings.Index(line, cursorUserDataDirFlag)
		if i < 0 {
			continue
		}
		v := strings.TrimLeft(line[i+len(cursorUserDataDirFlag):], "= ")
		if j := strings.Index(v, " --"); j >= 0 {
			v = v[:j]
		}
		v = filepath.Clean(strings.Trim(strings.TrimSpace(v), `"'`))
		if !filepath.IsAbs(v) || seen[v] {
			continue
		}
		seen[v] = true
		dirs = append(dirs, v)
	}
	return dirs
}

// cursorExtraProfileStateDBs returns the store paths of every non-default
// profile found running now or remembered from before, and updates the file.
func cursorExtraProfileStateDBs() []string {
	if !CursorVendorPlatformSupported(runtime.GOOS) && os.Getenv(cursorStateDBEnv) == "" {
		return nil
	}
	defaultDB, _ := cursorStateDBPath()
	var f cursorProfiles
	if b, err := os.ReadFile(cursorProfilesPath()); err == nil { // #nosec G304 -- state dir path.
		if json.Unmarshal(b, &f) != nil || f.Version != cursorProfilesVersion {
			f = cursorProfiles{}
		}
	}
	var dirs, dbs []string
	seen := map[string]bool{}
	// Running dirs first, so the cap keeps the most recently seen.
	for _, dir := range append(cursorUserDataDirs(cursorProcessArgs()), f.Dirs...) {
		db := cursorProfileStateDB(dir)
		if seen[dir] || db == defaultDB || len(dirs) == cursorProfilesMax {
			continue
		}
		seen[dir] = true
		if _, err := os.Stat(db); err != nil {
			continue
		}
		dirs = append(dirs, dir)
		dbs = append(dbs, db)
	}
	if !slices.Equal(dirs, f.Dirs) {
		writeCursorProfiles(dirs)
	}
	return dbs
}

func writeCursorProfiles(dirs []string) {
	path := cursorProfilesPath()
	if len(dirs) == 0 {
		_ = os.Remove(path)
		return
	}
	b, err := json.Marshal(cursorProfiles{Version: cursorProfilesVersion, Dirs: dirs})
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}
