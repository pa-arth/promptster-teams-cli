package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
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
// Paths only, never a credential. A remembered dir is never dropped for a failed
// read, because a store that is briefly unavailable would then be forgotten
// until its Cursor runs again. It leaves only by the cap.
//
// ONE FILE PER PROFILE. The hook and the daemon both remember profiles, at the
// same moments. A shared list needs a read-merge-write, which loses an update
// without a lock and needs a fallback when the lock fails. A file per profile,
// named by a hash of its path, has no merge, so no writer can erase another's
// profile.

const (
	// ponytail: 8 remembered profiles, the most recently seen kept. Raise it if
	// anyone runs more.
	cursorProfilesMax     = 8
	cursorUserDataDirFlag = "--user-data-dir"
	// The hook runs this scan inside its 2s budget. `ps` takes tens of ms.
	cursorProcessScanWait = 500 * time.Millisecond
)

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

func cursorProfilesDir() string {
	return filepath.Join(state.StateDir(), "cursor-profiles")
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

// rememberRunningCursorProfiles remembers every running non-default profile and
// returns every remembered one, most recently seen first.
func rememberRunningCursorProfiles() []string {
	if !CursorVendorPlatformSupported(runtime.GOOS) && os.Getenv(cursorStateDBEnv) == "" {
		return nil
	}
	return rememberCursorProfiles(cursorUserDataDirs(cursorProcessArgs()))
}

func rememberCursorProfiles(running []string) []string {
	defaultDB, _ := cursorStateDBPath()
	root := cursorProfilesDir()
	for _, dir := range running {
		if cursorProfileStateDB(dir) != defaultDB {
			writeCursorProfile(root, dir)
		}
	}
	entries, _ := os.ReadDir(root)
	type seen struct {
		dir string
		at  time.Time
	}
	var all []seen
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name())) // #nosec G304 -- state dir path.
		if dir := string(b); err == nil && filepath.IsAbs(dir) && cursorProfileStateDB(dir) != defaultDB {
			all = append(all, seen{dir, info.ModTime()})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	var dirs []string
	for i, p := range all {
		if i >= cursorProfilesMax {
			_ = os.Remove(filepath.Join(root, cursorProfileFileName(p.dir)))
			continue
		}
		dirs = append(dirs, p.dir)
	}
	// A running profile whose entry could not be written (a read-only state
	// dir) is still read this cycle.
	for i := len(running) - 1; i >= 0; i-- {
		if dir := running[i]; cursorProfileStateDB(dir) != defaultDB && !slices.Contains(dirs, dir) {
			dirs = append([]string{dir}, dirs...)
		}
	}
	return dirs
}

func cursorProfileFileName(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:8])
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

// writeCursorProfile saves one profile path, or refreshes its mtime (the recency
// the cap evicts by). The rename keeps a reader from seeing a torn file.
func writeCursorProfile(root, dir string) {
	if os.MkdirAll(root, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(root, "profile-*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.WriteString(dir)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), filepath.Join(root, cursorProfileFileName(dir))) != nil {
		_ = os.Remove(tmp.Name())
	}
}
