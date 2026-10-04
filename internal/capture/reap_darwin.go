package capture

import "golang.org/x/sys/unix"

// childPIDs reads the kernel process table for processes whose parent is ppid,
// zombies included (pgrep skips them).
func childPIDs(ppid int) ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, p := range procs {
		if int(p.Eproc.Ppid) == ppid {
			pids = append(pids, int(p.Proc.P_pid))
		}
	}
	return pids, nil
}
