package capture

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// childPIDs scans /proc for processes whose parent is ppid, zombies included.
func childPIDs(ppid int) ([]int, error) {
	stats, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return nil, err
	}
	if len(stats) == 0 {
		return nil, os.ErrNotExist // no /proc mounted
	}
	var pids []int
	for _, path := range stats {
		raw, err := os.ReadFile(path) // #nosec G304 -- fixed /proc glob
		if err != nil {
			continue // exited mid-scan
		}
		// "pid (comm) state ppid ...": comm may contain spaces/parens, so split after the last ')'.
		s := string(raw)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) < 2 || f[1] != strconv.Itoa(ppid) {
			continue
		}
		if pid, err := strconv.Atoi(filepath.Base(filepath.Dir(path))); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
