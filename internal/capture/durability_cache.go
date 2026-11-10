package capture

import (
	"os"
	"reflect"
	"sync"
)

// durabilityLedgerCache keeps the last parsed durability.json so the daemon does
// not decode the whole file again for every root on every poll. pollDurability
// reads the ledger about three times per root and rewrites it about twice. On a
// machine with hundreds of worktree roots and a multi-MB ledger, decoding JSON
// was most of the daemon's steady-state CPU.
//
// The cache is valid only while the file on disk is the SAME file (os.SameFile:
// inode/file index), with the same size and mtime, as when it was cached. Every
// writer replaces the file with tmp + rename, which gives it a new inode, so a
// write by another process always invalidates. Callers get a deep copy: the
// ledger is mutated in place by mutateDurabilityLedger callbacks and by
// read-only callers that treat their copy as scratch.
var durabilityLedgerCache struct {
	mu   sync.Mutex
	path string
	info os.FileInfo
	led  durabilityLedger
}

func durabilityCacheGet(path string, info os.FileInfo) (durabilityLedger, bool) {
	c := &durabilityLedgerCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info == nil || c.path != path || !os.SameFile(c.info, info) ||
		c.info.Size() != info.Size() || !c.info.ModTime().Equal(info.ModTime()) {
		return durabilityLedger{}, false
	}
	return cloneDurabilityLedger(c.led), true
}

func durabilityCachePut(path string, info os.FileInfo, led durabilityLedger) {
	c := &durabilityLedgerCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.info, c.led = path, info, cloneDurabilityLedger(led)
}

func durabilityLedgersEqual(a, b durabilityLedger) bool { return reflect.DeepEqual(a, b) }

func cloneDurabilityLedger(l durabilityLedger) durabilityLedger {
	out := durabilityLedger{V: l.V}
	if l.Roots != nil {
		out.Roots = make(map[string]map[string][]durTrackedRange, len(l.Roots))
		for root, paths := range l.Roots {
			if paths == nil {
				out.Roots[root] = nil
				continue
			}
			cp := make(map[string][]durTrackedRange, len(paths))
			for p, ranges := range paths {
				if ranges == nil {
					cp[p] = nil
					continue
				}
				cp[p] = append(make([]durTrackedRange, 0, len(ranges)), ranges...)
			}
			out.Roots[root] = cp
		}
	}
	if l.Cursors != nil {
		out.Cursors = make(map[string]string, len(l.Cursors))
		for k, v := range l.Cursors {
			out.Cursors[k] = v
		}
	}
	if l.Inventoried != nil {
		out.Inventoried = make(map[string]int64, len(l.Inventoried))
		for k, v := range l.Inventoried {
			out.Inventoried[k] = v
		}
	}
	if l.Seeded != nil {
		out.Seeded = make(map[string]map[string]int64, len(l.Seeded))
		for root, marks := range l.Seeded {
			if marks == nil {
				out.Seeded[root] = nil
				continue
			}
			cp := make(map[string]int64, len(marks))
			for k, v := range marks {
				cp[k] = v
			}
			out.Seeded[root] = cp
		}
	}
	return out
}
