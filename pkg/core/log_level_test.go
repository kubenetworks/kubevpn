package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoInfoLevelLogging enforces the logging architecture rule (docs/13): the
// data-plane core is internal tracing, so it must log at Debug only — never at
// Info. Info streams to the CLI by default (StreamHook), which is what flooded
// the connect output with per-packet [Gvisor-*]/[Route] lines. This guard fails
// if any .Info(/.Infof( logging call reappears in a non-test file under pkg/core.
func TestNoInfoLevelLogging(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read pkg/core: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Info" || sel.Sel.Name == "Infof" {
				pos := fset.Position(sel.Pos())
				t.Errorf("%s:%d: Info-level logging is forbidden in pkg/core (data-plane internals must use Debug); found .%s(",
					filepath.Base(name), pos.Line, sel.Sel.Name)
			}
			return true
		})
	}
}
