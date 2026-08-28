//go:build darwin

package capture

import "golang.org/x/sys/unix"

// cloneFile makes an APFS COPY-ON-WRITE clone of src at dst.
//
// This is what lets the no-live-read rule cost nothing. `state.vscdb` is a
// GIGABYTE on a working machine (measured: 1,010,061,312 bytes), and a byte
// copy of it every 15 minutes to read 848 bytes would be ~96GB of writes a day
// on an engineer's laptop. clonefile(2) shares the extents instead: measured
// 2026-08-27 the clone took 5ms and wrote no data, and the two auth keys came
// back off the clone in 23ms.
//
// dst must not exist — clonefile refuses to overwrite, which is a property we
// want rather than one to work around.
//
// EXDEV (a temp dir on a different volume) and ENOTSUP (a non-APFS filesystem)
// are ordinary outcomes here, not errors to report: the caller falls back to a
// byte copy, which is slower and correct.
func cloneFile(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
