package tunnel

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
// per-packet tunnel tracing must remain Debug. The owner sampler's bounded recovery
// and observation-gap events are the only Info exceptions; never allow packet,
// flow, or connection success logs through this guard.
func TestNoInfoLevelLogging(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read pkg/tunnel: %v", err)
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
				if name == "datapath_diagnostics.go" && len(call.Args) > 0 {
					if text, ok := call.Args[0].(*ast.BasicLit); ok && (strings.HasPrefix(text.Value, `"datapath_recovered `) || strings.HasPrefix(text.Value, `"datapath observation_gap:`)) {
						return true
					}
				}
				pos := fset.Position(sel.Pos())
				t.Errorf("%s:%d: Info-level logging is forbidden in pkg/tunnel (data-plane internals must use Debug); found .%s(",
					filepath.Base(name), pos.Line, sel.Sel.Name)
			}
			return true
		})
	}
}
