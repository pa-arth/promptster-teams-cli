package capture

import (
	"path/filepath"
)

// The Cursor hook rail's health, as one closed word the device can SAY.
//
// WHY THIS EXISTS. v0.18.1 shipped a repair for a defect that silently killed
// every hook in a shared ~/.cursor/hooks.json (see cursor_hooks_validate.go).
// The repair works, and it is completely unobservable from the backend: it runs
// at watch startup, fixes the file, and nothing leaves the machine to say so.
// Asked "did the fleet actually recover", the honest answer was "run `doctor` on
// each machine and tell me" — which is not an answer for a fleet.
//
// That is the same shape as the defect itself. The state exists on the device
// and never leaves it, so the only way to know is to already be there.
//
// It is also the same shape as `pendingEvents` (presence.go): the device is the
// only party that can see its own hooks.json, the backend had no field for it,
// and an absence of hook data has at least five causes that look identical from
// the server — Cursor not installed, rail never enrolled, file rejected, binary
// missing, engineer simply idle. A count of zero events distinguishes none of
// them. One word does.
//
// WHAT MAY BE IN THIS WORD: nothing but the words below. The file it describes
// belongs partly to other tools, so a reason string could carry a neighbour's
// command, a path, or an argument. A closed enum cannot. Never widen this to
// free text — the doctor's prose stays on the machine, where it is safe.
type CursorHookRailState string

const (
	// CursorHookRailNotInstalled — no ~/.cursor at all. Not a fault, and the
	// single most useful value: it turns "this engineer captured no Cursor" from
	// a mystery into a settled fact.
	CursorHookRailNotInstalled CursorHookRailState = "not_installed"
	// CursorHookRailUnreadable — hooks.json exists and is not valid JSON. We
	// refuse to overwrite it, so the rail stays off until a human fixes it.
	CursorHookRailUnreadable CursorHookRailState = "unreadable"
	// CursorHookRailRejected — the file parses, and Cursor is provably throwing
	// ALL of it away. The state this whole change exists to make visible.
	CursorHookRailRejected CursorHookRailState = "rejected"
	// CursorHookRailDangling — enrolled, but the registered command cannot run:
	// gone, or present without an executable bit. Either way Cursor execs it
	// inside the agent loop on every event and gets nothing.
	CursorHookRailDangling CursorHookRailState = "dangling"
	// CursorHookRailUnenrolled — Cursor is installed, none of our steps are
	// registered. Usually a daemon that has not restarted since 0.12.0.
	CursorHookRailUnenrolled CursorHookRailState = "unenrolled"
	// CursorHookRailPartial — some steps registered, some not.
	CursorHookRailPartial CursorHookRailState = "partial"
	// CursorHookRailOK — enrolled for every step, runnable, and nothing in the
	// file is provably rejected.
	CursorHookRailOK CursorHookRailState = "ok"
)

// CursorHookRailReport is what the device knows about its own hook rail.
type CursorHookRailReport struct {
	State CursorHookRailState
	// Repairs is how many entries we have ever repaired in this file — the log's
	// cumulative Total, NOT the length of its trimmed record window, which
	// saturates at 50. Reported
	// as a number and NOT omitted at zero, for the same reason pendingEvents is
	// not: a reported zero is a measurement ("we changed nothing of theirs"),
	// and a field that vanishes at zero cannot be told apart from a fleet too old
	// to report it at all.
	Repairs int
	// Unverifiable counts entries whose hook type this build cannot judge. It is
	// the honesty term on State: `ok` means "nothing PROVABLY wrong", and where
	// this is non-zero that is a weaker claim than it sounds.
	Unverifiable int
}

// InspectCursorHookRail resolves the rail's state from the same primitives the
// doctor prose uses.
//
// ORDERED WORST-FIRST, and the order is a claim about what blocks what. A
// rejected file makes enrollment irrelevant — Cursor runs none of it — so
// "rejected" must win over "partial" rather than the file's most recent
// complaint winning. The doctor makes the same ordering in prose, and
// TestDoctorAndRailStateAgree pins the two together so they cannot drift into
// telling an engineer and a dashboard different stories about one machine.
func InspectCursorHookRail() CursorHookRailReport {
	rep := CursorHookRailReport{Repairs: loadCursorHookRepairLog().totalRepairs()}

	path := cursorUserHooksPath()
	if !dirExists(filepath.Dir(path)) {
		rep.State = CursorHookRailNotInstalled
		return rep
	}
	cfg, err := loadCursorHookConfig(path)
	if err != nil {
		rep.State = CursorHookRailUnreadable
		return rep
	}

	defects, unknown := cursorHookConfigDefects(cfg)
	rep.Unverifiable = unknown
	if len(defects) > 0 {
		rep.State = CursorHookRailRejected
		return rep
	}

	var missing int
	bins := map[string]bool{}
	for _, step := range cursorHookSteps {
		found := false
		for _, e := range cfg.Hooks[step] {
			if !isPromptsterCursorHook(e) {
				continue
			}
			found = true
			bins[cursorHookCommandBinary(e.Command)] = true
		}
		if !found {
			missing++
		}
	}
	if missing == len(cursorHookSteps) {
		rep.State = CursorHookRailUnenrolled
		return rep
	}
	// Dangling outranks partial for the same reason the doctor says so: a machine
	// with three of eight steps pointed at a deleted binary is running a missing
	// command inside the engineer's agent loop, which is worse than the three
	// signals it is not collecting.
	for bin := range bins {
		if bin == "" || !isRunnable(bin) {
			rep.State = CursorHookRailDangling
			return rep
		}
	}
	if missing > 0 {
		rep.State = CursorHookRailPartial
		return rep
	}
	rep.State = CursorHookRailOK
	return rep
}
