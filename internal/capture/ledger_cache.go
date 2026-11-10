package capture

import (
	"maps"
	"os"
	"reflect"
	"slices"
	"sync"
)

// ledgerCache keeps the last parse of a JSON ledger file so the daemon does not
// decode the whole file again for every root on every poll. git-watch reads
// durability.json about three times per root per poll, and rework.json on every
// branch check. On a machine with hundreds of worktree roots and a multi-MB
// ledger, JSON decoding was most of the daemon's steady-state CPU.
//
// An entry is valid only while the file on disk is the SAME file (os.SameFile:
// inode / file index), with the same size and mtime, as when it was cached.
// Every writer replaces the file with tmp + rename, which gives it a new inode,
// so a write by another process always invalidates. Callers get a deep copy:
// ledgers are mutated in place by mutate callbacks, and read-only callers may
// treat their copy as scratch.
type ledgerCache[T any] struct {
	mu   sync.Mutex
	path string
	info os.FileInfo
	val  T
}

func (c *ledgerCache[T]) get(path string, info os.FileInfo) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	if c.info == nil || c.path != path || !os.SameFile(c.info, info) ||
		c.info.Size() != info.Size() || !c.info.ModTime().Equal(info.ModTime()) {
		return zero, false
	}
	return deepCopy(c.val), true
}

func (c *ledgerCache[T]) put(path string, info os.FileInfo, val T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.info, c.val = path, info, deepCopy(val)
}

// matches reports whether val equals the cached parse of the file at path, as
// it is on disk right now. A mutate callback that left its ledger equal to this
// changed nothing, so the rewrite can be skipped. Comparing against the cache
// avoids keeping a second copy of the ledger just for the before/after check.
func (c *ledgerCache[T]) matches(path string, val T) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info == nil || c.path != path || !os.SameFile(c.info, info) ||
		c.info.Size() != info.Size() || !c.info.ModTime().Equal(info.ModTime()) {
		return false
	}
	return reflect.DeepEqual(c.val, val)
}

// putAfterWrite records what was just renamed into place at path.
func (c *ledgerCache[T]) putAfterWrite(path string, val T) {
	if info, err := os.Stat(path); err == nil {
		c.put(path, info, val)
	}
}

var (
	durabilityLedgerCache ledgerCache[durabilityLedger]
	reworkLedgerCache     ledgerCache[reworkLedger]
)

// deepCopy copies maps, slices, structs and pointers recursively, so no part of
// the result aliases v. It is reflection-based on purpose: a hand-written clone
// would silently share any map field added to a ledger later. Ledgers are plain
// JSON data (no channels, funcs or cycles).
func deepCopy[T any](v T) T {
	out := reflect.New(reflect.TypeOf(&v).Elem()).Elem()
	copyValue(out, reflect.ValueOf(&v).Elem())
	return out.Interface().(T)
}

func copyValue(dst, src reflect.Value) {
	// Fast paths for the leaf types the ledgers are made of: these hold no
	// pointers, so a shallow clone is a deep one. Reflection over them was most of
	// the copy's cost.
	if src.CanInterface() {
		switch v := src.Interface().(type) {
		case []durTrackedRange:
			if v != nil {
				dst.Set(reflect.ValueOf(slices.Clone(v)))
			}
			return
		case map[string]string:
			if v != nil {
				dst.Set(reflect.ValueOf(maps.Clone(v)))
			}
			return
		case map[string]int64:
			if v != nil {
				dst.Set(reflect.ValueOf(maps.Clone(v)))
			}
			return
		case map[string]bool:
			if v != nil {
				dst.Set(reflect.ValueOf(maps.Clone(v)))
			}
			return
		}
	}
	switch src.Kind() {
	case reflect.Map:
		if src.IsNil() {
			return
		}
		m := reflect.MakeMapWithSize(src.Type(), src.Len())
		elem := src.Type().Elem()
		iter := src.MapRange()
		for iter.Next() {
			e := reflect.New(elem).Elem()
			copyValue(e, iter.Value())
			m.SetMapIndex(iter.Key(), e)
		}
		dst.Set(m)
	case reflect.Slice:
		if src.IsNil() {
			return
		}
		s := reflect.MakeSlice(src.Type(), src.Len(), src.Len())
		for i := 0; i < src.Len(); i++ {
			copyValue(s.Index(i), src.Index(i))
		}
		dst.Set(s)
	case reflect.Struct:
		for i := 0; i < src.NumField(); i++ {
			copyValue(dst.Field(i), src.Field(i))
		}
	case reflect.Ptr:
		if src.IsNil() {
			return
		}
		p := reflect.New(src.Type().Elem())
		copyValue(p.Elem(), src.Elem())
		dst.Set(p)
	default:
		dst.Set(src)
	}
}
