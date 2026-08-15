package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T4.29a freezes the production cutover: normal CLI execution must publish
// only the Runtime returned by Assembly.Build and then run/close that object.
func TestMainUsesOnlyAssemblyAndLifecycle(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var runArgs *ast.FuncDecl
	ast.Inspect(file, func(node ast.Node) bool {
		if fn, ok := node.(*ast.FuncDecl); ok && fn.Name.Name == "runArgs" {
			runArgs = fn
		}
		return true
	})
	if runArgs == nil {
		t.Fatal("runArgs is unavailable")
	}
	if countDirectCall(runArgs.Body, "buildAssemblyFromCLI", func(call *ast.CallExpr) bool { return len(call.Args) == 1 && identifierName(call.Args[0]) == "args" }) != 1 {
		t.Fatal("runArgs must build its Runtime through buildAssemblyFromCLI")
	}
	hasRuntimeLifecycle := false
	ast.Inspect(runArgs.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && (selector.Sel.Name == "Run" || selector.Sel.Name == "Close") {
			hasRuntimeLifecycle = true
		}
		return true
	})
	if !hasRuntimeLifecycle {
		t.Fatal("runArgs must run and close the published Runtime")
	}
	for _, forbidden := range []string{"defaultStartupFactories", "runWithFactories", "newCompositeCloser"} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("main.go still references legacy production path %q", forbidden)
		}
	}
}

func TestMainNeverPublishesPartialRuntime(t *testing.T) {
	source, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, "defaultAssembly().Build(context.Background(), options)") {
		t.Fatal("CLI seam does not publish Assembly.Build result")
	}
	if strings.Contains(text, "app.New(") || strings.Contains(text, "tui.Run(") {
		t.Fatal("CLI seam constructs or runs a partial UI graph")
	}
}

func TestOnlyAssemblyConstructsProductionGraph(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") || entry.Name() == "assembly.go" {
			continue
		}
		body, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "app.NewWithOptions(") {
			t.Fatalf("%s constructs App outside Assembly", entry.Name())
		}
	}
}
