package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// statuslineDrivers are the calls that run statuslineCommand, which writes the cache.
var statuslineDrivers = map[string]bool{
	"statuslineCommand":   true,
	"runStatuslineFor":    true,
	"runStatuslineWithin": true,
}

// unisolatedStatuslineExceptions are the functions allowed to drive the statusline without
// isolateState, each with the reason. Add to this only with one.
var unisolatedStatuslineExceptions = map[string]string{
	// The helpers themselves: they are the drivers, and every caller is audited instead.
	"runStatuslineFor":    "the driver helper; its callers are what this audits",
	"runStatuslineWithin": "the driver helper; its callers are what this audits",

	// The state directory IS this test's subject. It points GOVERNOR_STATE_DIR at a path that
	// must not exist and asserts the command never creates it. isolateState would create that
	// directory up front and turn the assertion into one that cannot fail.
	"TestStatuslineNeverCreatesTheStateDirectory": "asserts the state directory is never created; isolateState would create it",
}

// TestEveryStatuslineDriverIsolatesItsState is the audit GOV-B18 asked for.
//
// statuslineCommand writes a cache and serves from it for a second without dialling. A test
// that drives it without a state directory of its own shares TestMain's package-wide one, so
// under -count>1 a later iteration finds an earlier one's cache, sends nothing, and fails on
// whatever it was actually testing. TestEveryRequestCarriesTheProtocolVersion did exactly
// that, 30 runs in 60, and nothing ran at -count>1 to notice. This makes the next such test
// fail at the default count, where CI runs.
//
// It reads the source rather than running anything, so it is cheap enough to keep.
func TestEveryStatuslineDriverIsolatesItsState(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(".", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			drives, isolates := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok {
					drives = drives || statuslineDrivers[id.Name]
					isolates = isolates || id.Name == "isolateState"
				}
				return true
			})
			if !drives {
				continue
			}
			name := fn.Name.Name
			seen[name] = true
			if _, ok := unisolatedStatuslineExceptions[name]; ok {
				continue
			}
			if !isolates {
				t.Errorf("%s (%s) drives the statusline without isolateState(t): it will share "+
					"the package's state directory, and under -count>1 read an earlier run's "+
					"cache instead of dialling", name, fset.Position(fn.Pos()))
			}
		}
	}

	// An exception that no longer matches anything is a stale permission, and a rename would
	// leave one behind silently.
	for name := range unisolatedStatuslineExceptions {
		if !seen[name] {
			t.Errorf("exception %q no longer drives the statusline (renamed or removed?); "+
				"drop it from unisolatedStatuslineExceptions", name)
		}
	}
}
