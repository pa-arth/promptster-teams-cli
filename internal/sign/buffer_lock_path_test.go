package sign

// A LOCK IS HELD ON AN INODE, NOT ON A PATH.
//
// WithBufferLock flocks the fd it opens at the path it is given. Every caller in
// this tree guards a JSON ledger it rewrites with the write-temp-then-rename
// idiom — and renaming a new file over the locked path replaces the inode the
// lock is held on. The next process to open that path gets the NEW inode and
// flocks it successfully, so two writers sit inside the "critical" section at
// once and the second read-modify-write silently discards the first.
//
// Greptile caught this on the Cursor generation cache (PR #166), where a lost
// entry leaves a token row with no model and therefore unpriced. It was never
// one bug: cursorHookClaimsPath had the identical shape one file over, and its
// own comment says Cursor "fires several steps per turn and does not serialise
// them" — the concurrency was known and the lock protecting it did not hold.
//
// The convention that already works everywhere else is a SIDECAR: lock
// "<ledger>.lock", a file nothing ever renames over. This turns that convention
// into a check, because the defect is invisible at the call site — the broken
// and the correct form differ by five characters and both compile, run, and pass
// every single-process test.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The module root, relative to this package's directory (Go runs a test with its
// own package dir as cwd).
const moduleRootFromPackage = "../.."

// A walk that matches nothing must fail as broken plumbing rather than pass as
// "no caller locks the wrong path". May rise.
const minLockCallSites = 15

// argIsDedicatedLockPath reports whether the expression can only produce a path
// that exists to be locked.
//
// Two accepted forms, both structural rather than name-based guesses:
//
//	<anything> + ".lock"     — the sidecar convention
//	<...>LockPath[For](...)  — a helper whose whole job is to name a lock file
func argIsDedicatedLockPath(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return false
		}
		// Only the RIGHTMOST operand decides the suffix.
		return argIsDedicatedLockPath(v.Y)
	case *ast.BasicLit:
		return v.Kind == token.STRING && strings.HasSuffix(v.Value, `.lock"`)
	case *ast.CallExpr:
		name := ""
		switch f := v.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			name = f.Sel.Name
		}
		return strings.Contains(strings.ToLower(name), "lockpath")
	}
	return false
}

func TestWithBufferLockAlwaysLocksADedicatedPath(t *testing.T) {
	root, err := filepath.Abs(moduleRootFromPackage)
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}

	var offenders []string
	sites := 0

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored and build output are not ours to police.
			if n := d.Name(); n == "vendor" || n == "node_modules" || n == ".git" || n == "dist" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if name != "WithBufferLock" || len(call.Args) == 0 {
				return true
			}
			// The declaration itself, not a call site.
			if strings.HasSuffix(path, "signing_lock_unix.go") || strings.HasSuffix(path, "signing_lock_windows.go") {
				return true
			}
			sites++
			if !argIsDedicatedLockPath(call.Args[0]) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, fmt.Sprintf("%s:%d", rel, fset.Position(call.Args[0].Pos()).Line))
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	if sites < minLockCallSites {
		t.Fatalf(
			"found only %d WithBufferLock call sites (floor %d) — the walk went blind (renamed helper? moved package?), so a green result here proves nothing",
			sites, minLockCallSites,
		)
	}
	if len(offenders) > 0 {
		t.Errorf(
			"WithBufferLock must be given a path that exists ONLY to be locked (\"<ledger>.lock\", or a *LockPath helper).\n"+
				"These lock a path the ledger write renames over, which swaps the locked inode and lets two writers into the critical section:\n  %s",
			strings.Join(offenders, "\n  "),
		)
	}
	t.Logf("checked %d WithBufferLock call sites", sites)
}
