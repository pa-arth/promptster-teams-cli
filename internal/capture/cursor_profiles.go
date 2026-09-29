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
// Chromium passes `--user-data-dir=` to every helper process. The process list
// is scanned by each vendor cycle, and by the hook on any turn whose login is
// unreadable, since that turn's Cursor is running at that moment. That catches
// a profile opened and closed between polls. Each profile dir found is
// remembered, so it is still read in cycles where its window is closed. That
// matters because an account's usage list covers the whole billing cycle (D1).
// The file holds paths only, never a credential. A remembered dir is never
// dropped for a failed read, because a store that is briefly unavailable would
// then be forgotten until its Cursor runs again. It leaves only by the cap.

const (
	cursorProfilesVersion = 1
	// ponytail: 8 remembered profiles, the most recently seen kept. Raise it if
	// anyone runs more.
	cursorProfilesMax     = 8
	cursorUserDataDirFlag = "--user-data-dir"
	// The hook runs this scan inside its 2s budget. `ps` takes tens of ms.
	cursorProcessScanWait = 500 * time.Millisecond
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

func cursorStoreExists(db string) bool {
	_, err := os.Stat(db)
	return err == nil
}

// cursorUserDataDirs pulls `--user-data-dir` values out of Cursor command lines.
// ps joins argv with spaces, so a value's end is ambiguous: `/a/Client -- Work`
// could be a dir, or `/a/Client` followed by a flag. Each ` --` is a possible
// end, and the longest candidate with a Cursor store is taken. Relative dirs are
// skipped.
func cursorUserDataDirs(lines []string) []string {
	var dirs []string
	for _, line := range lines {
		if !strings.Contains(line, "Cursor") {
			continue
		}
		i := strings.Index(line, cursorUserDataDirFlag)
		if i < 0 {
			continue
		}
		rest := strings.TrimLeft(line[i+len(cursorUserDataDirFlag):], "= ")
		cands := []string{rest}
		for j := strings.LastIndex(rest, " --"); j >= 0; j = strings.LastIndex(rest[:j], " --") {
			cands = append(cands, rest[:j])
		}
		for _, c := range cands {
			c = filepath.Clean(strings.Trim(strings.TrimSpace(c), `"'`))
			if filepath.IsAbs(c) && cursorStoreExists(cursorProfileStateDB(c)) {
				if !slices.Contains(dirs, c) {
					dirs = append(dirs, c)
				}
				break
			}
		}
	}
	return dirs
}

// rememberRunningCursorProfiles adds every running non-default profile to the
// file, running ones first, and returns the whole list. Both the daemon and the
// hook write it.
// ponytail: last writer wins between the two, so a dir can be lost to a race and
// is found again on its next scan. Lock the file if that is ever observed.
func rememberRunningCursorProfiles() []string {
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
	var dirs []string
	for _, dir := range append(cursorUserDataDirs(cursorProcessArgs()), f.Dirs...) {
		if !slices.Contains(dirs, dir) && cursorProfileStateDB(dir) != defaultDB && len(dirs) < cursorProfilesMax {
			dirs = append(dirs, dir)
		}
	}
	if !slices.Equal(dirs, f.Dirs) {
		writeCursorProfiles(dirs)
	}
	return dirs
}

// cursorExtraProfileStateDBs returns the stores of every remembered profile
// that is present this cycle.
func cursorExtraProfileStateDBs() []string {
	var dbs []string
	for _, dir := range rememberRunningCursorProfiles() {
		if db := cursorProfileStateDB(dir); cursorStoreExists(db) {
			dbs = append(dbs, db)
		}
	}
	return dbs
}

func writeCursorProfiles(dirs []string) {
	b, err := json.Marshal(cursorProfiles{Version: cursorProfilesVersion, Dirs: dirs})
	path := cursorProfilesPath()
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	// A unique temp name: the hook and the daemon can write at the same moment.
	tmp, err := os.CreateTemp(filepath.Dir(path), "cursor-profiles-*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}
