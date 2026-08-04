package capture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// THIS IS THE CHECK THAT WOULD HAVE CAUGHT #140.
//
// It parses the two progress loaders and finds every `p.V < N` migration block,
// then reconciles them against the declared table IN BOTH DIRECTIONS:
//
//  1. Every block has a row. A step nobody declared is a step nobody costed.
//  2. Every row has a block. The version const is DERIVED from the table, so a
//     row with no body raises the version and stamps every device as having
//     applied a migration that never ran — unrecoverable without another bump.
//  3. If a block CLEARS OFFSETS — i.e. it causes every in-window transcript on
//     every device to be re-read from zero — its row declares a ReplayHorizon.
//
// A source check, not a table check, and that distinction is the whole point. A
// test that only validated the table would be satisfied by a table nobody
// updated: #140 was a hand-written `p.V < 2 { p.Offsets = map[string]int64{} }`
// block, and no amount of validating a structure it never touched would have
// seen it. The AST sees the block whether or not anyone remembered the table.
//
// Scope, stated plainly: this covers offset-clearing inside the versioned
// migration blocks of the two loaders. Code that clears offsets somewhere else
// entirely is out of reach here and is not claimed to be covered.

// progressLoader names one loader and the table that must describe it.
type progressLoader struct {
	file       string
	fn         string
	migrations []progressMigration
	label      string
}

func progressLoaders() []progressLoader {
	return []progressLoader{
		{
			file: "cmd_claude_watch.go", fn: "loadClaudeWatchProgress",
			migrations: claudeProgressMigrations, label: "claudeProgressMigrations",
		},
		{
			file: "cmd_codex_watch.go", fn: "loadCodexWatchProgress",
			migrations: codexProgressMigrations, label: "codexProgressMigrations",
		},
	}
}

func TestEveryOffsetClearingMigrationDeclaresItsReplayHorizon(t *testing.T) {
	for _, ld := range progressLoaders() {
		blocks := parseMigrationBlocks(t, ld.file, ld.fn)
		if len(blocks) == 0 {
			t.Errorf("%s: found no `V < N` migration blocks — either the loader was "+
				"restructured or this check has gone blind; a check that silently matches "+
				"nothing is worse than no check", ld.fn)
			continue
		}
		// ROW -> BLOCK. Review caught this direction missing, and the hole it left
		// is worse than the one the block->row direction closes: a row added with
		// no loader block passes a block->row check, raises the DERIVED schema
		// version, and both loaders then stamp `p.V = <new>` on every device — so
		// every device is permanently marked as having applied a migration whose
		// body never ran, and can never be made to run it. There is no recovery
		// short of another version bump.
		for _, m := range ld.migrations {
			if !hasMigrationBlock(blocks, m.V) {
				t.Errorf("%s declares v%d but %s has no `V < %d` block. The schema version is "+
					"DERIVED from this table, so a row with no body silently stamps every device as "+
					"having applied a migration that never ran — and nothing can re-run it.",
					ld.label, m.V, ld.fn, m.V)
			}
		}

		for _, b := range blocks {
			m, ok := findMigration(ld.migrations, b.version)
			if !ok {
				t.Errorf("%s migrates to v%d but %s has no row for it. Every schema step "+
					"must be declared, because the version const is derived from the table and "+
					"a step nobody declared is a step nobody costed.",
					ld.fn, b.version, ld.label)
				continue
			}
			if b.clearsOffsets && m.ReplayHorizon == 0 {
				t.Errorf("%s v%d CLEARS OFFSETS but %s declares ReplayHorizon: 0.\n"+
					"Clearing offsets re-reads every in-window transcript on every device in the "+
					"fleet — #140 was exactly this, one line of diff that cost 62,302 replayed "+
					"events on one device and a 15-day-stale backlog. Declare the horizon (see "+
					"progress_migrations.go), or do not clear offsets.",
					ld.fn, b.version, ld.label)
			}
			if !b.clearsOffsets && m.ReplayHorizon != 0 {
				t.Errorf("%s v%d declares ReplayHorizon %s but clears no offsets. A horizon "+
					"that overstates the cost trains reviewers to discount it.",
					ld.fn, b.version, m.ReplayHorizon)
			}
		}
	}
}

// The version const must equal the table's high-water mark. It is derived, so
// this can only fail if someone reintroduces a hand-written const — which is the
// shape of #140 and is worth failing loudly rather than quietly diverging.
func TestSchemaVersionsComeFromTheMigrationTables(t *testing.T) {
	if got, want := claudeProgressSchemaV, latestProgressV(claudeProgressMigrations); got != want {
		t.Errorf("claudeProgressSchemaV = %d, want %d (the table's highest V)", got, want)
	}
	if got, want := codexProgressSchemaV, latestProgressV(codexProgressMigrations); got != want {
		t.Errorf("codexProgressSchemaV = %d, want %d (the table's highest V)", got, want)
	}
}

// Rows must be a gapless 1..N: a table with a hole means some device's file sits
// at a version no row describes, and `progressReplayPending` would report a cost
// for a step that does not exist.
func TestMigrationTablesAreGaplessAndOrdered(t *testing.T) {
	for _, ld := range progressLoaders() {
		for i, m := range ld.migrations {
			if m.V != i+1 {
				t.Errorf("%s[%d].V = %d, want %d — rows are append-only and gapless",
					ld.label, i, m.V, i+1)
			}
			if m.Why == "" {
				t.Errorf("%s v%d has no Why. The operator reading the startup line did not "+
					"write this migration.", ld.label, m.V)
			}
		}
	}
}

// hasMigrationBlock reports whether the loader actually implements version v.
func hasMigrationBlock(blocks []migrationBlock, v int) bool {
	for _, b := range blocks {
		if b.version == v {
			return true
		}
	}
	return false
}

// migrationBlock is one `if <recv>.V < N { ... }` statement in a loader.
type migrationBlock struct {
	version       int
	clearsOffsets bool
}

// parseMigrationBlocks finds the versioned migration blocks in one loader.
func parseMigrationBlocks(t *testing.T, file, fnName string) []migrationBlock {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fnName {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatalf("%s: no func %s — this check is pinned to a loader that no longer exists", file, fnName)
	}

	var blocks []migrationBlock
	ast.Inspect(fn, func(n ast.Node) bool {
		is, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		v, ok := versionGuard(is.Cond)
		if !ok {
			return true
		}
		blocks = append(blocks, migrationBlock{version: v, clearsOffsets: bodyClearsOffsets(is.Body)})
		return true
	})
	return blocks
}

// versionGuard matches `<x>.V < <int>` and returns the int.
func versionGuard(cond ast.Expr) (int, bool) {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.LSS {
		return 0, false
	}
	sel, ok := bin.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "V" {
		return 0, false
	}
	lit, ok := bin.Y.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil {
		return 0, false
	}
	return n, true
}

// bodyClearsOffsets reports whether a migration body ASSIGNS to any field whose
// name ends in "Offsets".
//
// Assignment, not deletion, and the difference matters: `delete(m.Offsets, k)`
// drops one file's cursor, while `m.Offsets = map[string]int64{}` drops the
// whole fleet's. Only the second is a replay event. Suffix-matching also catches
// `ClassifyOffsets`, which clears the classification cursors and re-reads the
// same history by another door.
func bodyClearsOffsets(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if name := sel.Sel.Name; len(name) >= len("Offsets") &&
				name[len(name)-len("Offsets"):] == "Offsets" {
				found = true
			}
		}
		return true
	})
	return found
}
