package repoaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestUnsupportedRunnerHasNoBareExecFallback(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve proctree audit source failed")
	}
	target := filepath.Join(filepath.Dir(currentFile), "..", "proctree", "runner_unsupported.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), target, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal("parse unsupported proctree runner failed")
	}
	for _, spec := range parsed.Imports {
		name, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatal("decode unsupported proctree import failed")
		}
		switch name {
		case "os/exec", "syscall", "golang.org/x/sys/windows":
			t.Fatalf("unsupported proctree runner imports execution package %q", name)
		}
	}
	forbiddenCalls := map[string]struct{}{
		"Command":             {},
		"CommandContext":      {},
		"CreateProcess":       {},
		"CreateProcessAsUser": {},
		"Exec":                {},
		"ForkExec":            {},
		"ShellExecute":        {},
		"StartProcess":        {},
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch function := call.Fun.(type) {
		case *ast.Ident:
			name = function.Name
		case *ast.SelectorExpr:
			name = function.Sel.Name
		}
		if _, forbidden := forbiddenCalls[name]; forbidden {
			t.Fatalf("unsupported proctree runner contains execution call %q", name)
		}
		return true
	})
}
