package redact

// WHICH KINDS THIS CLI ACTUALLY CONSTRUCTS — read from the source, not inferred
// from a table.
//
// allowlist_lockstep_test.go used to prove "this CLI does not emit kind X" by
// checking that X was absent from projectFieldAllowlist. That reads the wrong
// table for the evidence it claims. The allowlist says what SURVIVES projection;
// it says nothing about what is MINTED. An emitter added without an allowlist
// entry keeps that assertion green while ProjectEvent strips every payload of
// that kind to {} — which is the "emitted into a void" failure this whole change
// exists to catch. The exception list would have been hiding the exact bug the
// lockstep test is for.
//
// The reasons in serverOnlyFields already cited the right evidence — "grep: zero
// constructors in internal/normalize and internal/capture". This turns that grep
// into a test.
//
// HOW THE WALK IS KEPT HONEST, because a static analysis that quietly matches
// nothing is a check disarmed by its own plumbing:
//
//   - A floor on construction sites and distinct kinds found. Zero matches is a
//     FAILURE, not a clean run.
//   - `event.NewEvent` is asserted to be the ONLY place event.Kind is ever set.
//     If a second path appears, the walk goes blind to it — so that invariant is
//     checked rather than assumed.
//   - A construction site whose kind is not a string literal cannot be resolved
//     statically. Those are permitted ONLY inside the pass-through helpers that
//     forward their own `kind` parameter; anywhere else they are a blind spot and
//     fail by position.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The module root, relative to this package's directory (Go runs a test with its
// own package dir as cwd).
const moduleRootFromPackage = "../.."

// eventConstructorNames — every function that mints an event with a caller-chosen
// kind. `NewEvent` is the root (internal/event/event.go); the rest are the
// per-rail wrappers that forward to it, and they are listed because a call site
// naming a kind literal usually calls a wrapper, not the root.
//
// Matched by function NAME only, not by resolved type. That over-matches rather
// than under-matches, which is the correct direction for a completeness walk: a
// spurious extra kind shows up as a loud, wrong finding, while a missed
// constructor shows up as nothing at all.
var eventConstructorNames = map[string]bool{
	"NewEvent":           true, // internal/event — the root constructor
	"newEvent":           true, // cursorHookPayload, CursorTranscriptProcessor
	"newAIEvent":         true, // cursorHookPayload, CursorTranscriptProcessor
	"lifecycleEvent":     true, // cursorHookPayload
	"newTranscriptEvent": true, // ClaudeTranscriptProcessor
	"newCodexEvent":      true, // CodexRolloutProcessor
}

// Floors on what the walk must find. These are a plumbing check, not a census:
// they exist so a walk that matches nothing (a renamed helper, a moved package, a
// parser that silently skipped every file) fails as broken plumbing instead of
// passing as "no kinds are constructed", which would green-light every entry in
// kindsNotEmittedOnDevice at once. They may rise.
const (
	minConstructionSites = 40
	minDistinctKinds     = 25
)

// walkEventConstructionSites parses every non-test Go file in the module and
// returns the kinds constructed, each with the file:line that constructs it.
func walkEventConstructionSites(t *testing.T) (map[string][]string, []string) {
	t.Helper()

	fset := token.NewFileSet()
	byKind := map[string][]string{}
	var unresolved []string
	var parsedFiles int

	root, err := filepath.Abs(moduleRootFromPackage)
	if err != nil {
		t.Fatalf("cannot resolve the module root from %q: %v", moduleRootFromPackage, err)
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "bin", "dist", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			// A file this walk cannot parse is a file it cannot see constructors
			// in. Silently skipping it is how the walk would go blind.
			t.Fatalf("cannot parse %s: %v", path, parseErr)
		}
		parsedFiles++

		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			// A wrapper forwards its own `kind` parameter, so its inner call is
			// legitimately non-literal. Everywhere else, non-literal means the walk
			// cannot see the kind and must say so.
			insideWrapper := eventConstructorNames[fn.Name.Name]

			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall || len(call.Args) == 0 {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				}
				if !eventConstructorNames[name] {
					return true
				}
				pos := relativePos(fset, call.Pos(), root)
				lit, isLit := call.Args[0].(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					if !insideWrapper {
						unresolved = append(unresolved, pos+" ("+name+")")
					}
					return true
				}
				kind, unquoteErr := strconv.Unquote(lit.Value)
				if unquoteErr != nil {
					unresolved = append(unresolved, pos+" (unquotable literal)")
					return true
				}
				byKind[kind] = append(byKind[kind], pos)
				return true
			})
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", root, walkErr)
	}
	if parsedFiles == 0 {
		t.Fatalf("the emitter walk parsed 0 Go files under %s — the walk is broken, "+
			"and a broken walk would report every kind as un-emitted.", root)
	}
	for kind := range byKind {
		sort.Strings(byKind[kind])
	}
	return byKind, unresolved
}

// choosesAKind reports whether an expression NAMES a kind rather than carrying
// one already chosen elsewhere.
//
// An ident or a selector (`kind`, `ev.Kind`) is a copy: whatever kind it holds
// was chosen at a site the constructor walk can already see. A literal names one
// outright; a call, a conditional, or anything else computes one, and the walk
// cannot follow either. Unknown node types return true — for a completeness
// guard, "I cannot tell" must read as "report it", never as "fine".
func choosesAKind(value ast.Expr) bool {
	switch value.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return false
	default:
		return true
	}
}

func relativePos(fset *token.FileSet, pos token.Pos, root string) string {
	p := fset.Position(pos)
	if rel, err := filepath.Rel(root, p.Filename); err == nil {
		return rel + ":" + strconv.Itoa(p.Line)
	}
	return p.Filename + ":" + strconv.Itoa(p.Line)
}

// TestEventConstructionWalkSeesTheWholeTree — the guard on the walk.
//
// Everything below depends on this walk being able to see every construction
// site. Two ways it could go blind, both checked here rather than assumed:
// matching nothing at all, and a kind reaching an Event by a route the walk does
// not follow.
func TestEventConstructionWalkSeesTheWholeTree(t *testing.T) {
	byKind, unresolved := walkEventConstructionSites(t)

	sites := 0
	for _, positions := range byKind {
		sites += len(positions)
	}
	if sites < minConstructionSites || len(byKind) < minDistinctKinds {
		t.Fatalf("the emitter walk found %d construction sites across %d distinct kinds "+
			"(floors: %d / %d). A walk that matches (almost) nothing is broken plumbing, and it "+
			"would green-light every entry in kindsNotEmittedOnDevice at once. Check whether a "+
			"constructor in eventConstructorNames was renamed.",
			sites, len(byKind), minConstructionSites, minDistinctKinds)
	}

	if len(unresolved) > 0 {
		t.Errorf("%d event construction site(s) pass a non-literal kind outside a pass-through "+
			"helper, so this walk cannot see which kind they mint:\n  - %s\n"+
			"Each is a blind spot: a kind minted here would be invisible to "+
			"TestDeclaredNonEmittedKindsAreReallyNotEmitted. Use a literal, or add the enclosing "+
			"helper to eventConstructorNames if it forwards its own kind parameter.",
			len(unresolved), strings.Join(unresolved, "\n  - "))
	}
}

// TestKindIsOnlyCHOSENByNewEvent — the walk follows constructor CALLS, so it can
// only be complete if choosing a kind is something only a constructor does.
//
// CHOOSING, not setting, and the distinction is load-bearing. The first version
// of this test flagged every `Kind:` and immediately found
// internal/redact/redact.go:394 — the fail-closed rebuild that reconstructs an
// envelope after a redaction reparse failure, carrying `Kind: ev.Kind` across. It
// sets the field and mints nothing: a COPY propagates a kind already chosen
// somewhere the walk can see, so it is not a blind spot.
//
// A string literal or a computed expression is different — it names a kind the
// walk never saw, and "this CLI does not emit X" quietly stops being checkable.
// So a copy (ident or selector) is allowed and everything else fails, which is
// exactly the set of sites that could introduce a kind behind the walk's back.
func TestKindIsOnlyCHOSENByNewEvent(t *testing.T) {
	fset := token.NewFileSet()
	var offenders []string

	root, err := filepath.Abs(moduleRootFromPackage)
	if err != nil {
		t.Fatalf("cannot resolve the module root: %v", err)
	}
	// The one legitimate assignment lives here.
	allowed := filepath.Join(root, "internal", "event")

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "bin", "dist", "testdata":
				return filepath.SkipDir
			}
			if path == allowed {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("cannot parse %s: %v", path, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.KeyValueExpr: // event.Event{Kind: …}
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "Kind" && choosesAKind(n.Value) {
					offenders = append(offenders, relativePos(fset, n.Pos(), root)+" (struct literal)")
				}
			case *ast.AssignStmt: // e.Kind = …
				for i, lhs := range n.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Kind" {
						continue
					}
					// A multi-value assignment (`a.Kind, b = f()`) has no per-LHS
					// expression to inspect, so treat it as choosing — unresolvable is
					// the conservative reading for a completeness guard.
					if len(n.Rhs) != len(n.Lhs) || choosesAKind(n.Rhs[i]) {
						offenders = append(offenders, relativePos(fset, sel.Pos(), root)+" (assignment)")
					}
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", root, walkErr)
	}

	if len(offenders) > 0 {
		t.Errorf("event.Kind is CHOSEN outside internal/event, at %d site(s):\n  - %s\n"+
			"The emitter walk follows constructor CALLS, so it cannot see a kind named this way. "+
			"Either route the construction through event.NewEvent, or teach "+
			"walkEventConstructionSites to follow this path — but do not leave it unseen, because "+
			"the blind spot shows up as a kind confidently reported as un-emitted. (Copying an "+
			"existing kind across — `Kind: ev.Kind` — is fine and is not reported.)",
			len(offenders), strings.Join(offenders, "\n  - "))
	}
}

// kindsAssertedUnconstructed — every kind that some declared exception's REASON
// rests on this CLI never minting.
//
// It is a superset of kindsNotEmittedOnDevice, and deliberately so. That table
// only lists kinds missing from projectFieldAllowlist; but several exceptions in
// serverOnlyFields lean on the same fact about kinds that DO have an allowlist
// entry — "this CLI emits no api_request event at all", "no tool_decision
// emitter on this device". Those sentences were the right evidence and were pure
// prose. Here they are checked.
//
// The freshness checks in allowlist_lockstep_test.go are NOT affected by the
// wrong-table flaw this file fixes: serverOnlyFields claims "the server
// allowlists it, the device does not", which is a claim about two allowlists, and
// they read those two allowlists. The claim and the evidence match. Only the
// REASONS reached past what those tables can prove — this is what closes that.
var kindsAssertedUnconstructed = map[string]string{
	"instructions_loaded": "Server-side only — minted by the tier-B hook route, not by this CLI.",
	"api_request":         "serverOnlyFields[api_request] rests on this: no api_request constructor here.",
	"api_error":           "Sibling of api_request; the allowlist entry covers the backend proxy rail, not this binary.",
	"tool_decision":       "serverOnlyFields[tool_decision] rests on this: approval decisions are an OTel-wire concept.",
	"heartbeat":           "deviceOnlyFields[heartbeat] rests on this: the beat this CLI builds is `presence`.",
}

// TestDeclaredNonEmittedKindsAreReallyNotEmitted — the finding this file exists
// for.
//
// kindsNotEmittedOnDevice and kindsAssertedUnconstructed both claim a kind is
// never minted here. The evidence for that claim is the construction sites in the
// tree, NOT the absence of an allowlist entry: a kind can be emitted and
// unallowlisted at the same time, and that combination is precisely the bug —
// every payload stripped to {} with no error and no telemetry.
func TestDeclaredNonEmittedKindsAreReallyNotEmitted(t *testing.T) {
	byKind, _ := walkEventConstructionSites(t)
	var findings []string

	for _, kind := range sortedKeys(kindsNotEmittedOnDevice) {
		positions, constructed := byKind[kind]
		if !constructed {
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"%s is declared in kindsNotEmittedOnDevice, but this CLI constructs it at %s. "+
				"It has no projectFieldAllowlist entry, so every event of this kind projects to {} "+
				"— minted, stripped, and delivered empty. Add the kind to projectFieldAllowlist "+
				"with the fields the server allowlists, and delete the exception.",
			kind, strings.Join(positions, ", ")))
	}

	for _, kind := range sortedKeys(kindsAssertedUnconstructed) {
		if positions, constructed := byKind[kind]; constructed {
			findings = append(findings, fmt.Sprintf(
				"%s is listed in kindsAssertedUnconstructed (%s), but this CLI constructs it at %s. "+
					"An exception reason that rests on 'this CLI never emits it' is now false — "+
					"re-read the exceptions that cite it before deleting this line.",
				kind, kindsAssertedUnconstructed[kind], strings.Join(positions, ", ")))
		}
		if strings.TrimSpace(kindsAssertedUnconstructed[kind]) == "" {
			findings = append(findings, fmt.Sprintf("kindsAssertedUnconstructed[%q] carries no reason.", kind))
		}
	}

	// The two tables must not drift: everything kindsNotEmittedOnDevice claims is
	// un-emitted has to be checked here, or the superset silently stops being one.
	for _, kind := range sortedKeys(kindsNotEmittedOnDevice) {
		if _, listed := kindsAssertedUnconstructed[kind]; !listed {
			findings = append(findings, fmt.Sprintf(
				"kindsNotEmittedOnDevice[%q] is not in kindsAssertedUnconstructed, so its "+
					"'not emitted' claim is checked by nothing. Add it.", kind))
		}
	}

	report(t, "kinds declared un-emitted are emitted after all", findings)
}
