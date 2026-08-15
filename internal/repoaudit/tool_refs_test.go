package repoaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestBuiltinToolsHaveOnlyExecutorEntryAndNoReadAllThenTruncate(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve tool reference audit source failed")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	toolRoot := filepath.Join(repositoryRoot, "internal", "tool")

	var executeCalls int
	err := filepath.WalkDir(toolRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			if function.Recv != nil && receiverTypeName(function.Recv) == "Executor" && (function.Name.Name == "Execute" || function.Name.Name == "truncate") {
				t.Errorf("production Executor retains forbidden method %s in %s", function.Name.Name, relativeAuditPath(repositoryRoot, path))
			}
			hasReadAll := false
			hasTruncate := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calledName(call.Fun)
				switch name {
				case "ReadAll":
					hasReadAll = true
				case "truncate", "truncateString", "truncatedSummary":
					hasTruncate = true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Execute" {
					return true
				}
				executeCalls++
				if entry.Name() != "executor.go" || function.Name.Name != "executeValidated" || !isPrivateValidatedExecutor(selector.X) {
					t.Errorf("tool implementation Execute bypass in %s.%s", relativeAuditPath(repositoryRoot, path), function.Name.Name)
				}
				return true
			})
			if hasReadAll && hasTruncate {
				t.Errorf("ReadAll-then-truncate remains in %s.%s", relativeAuditPath(repositoryRoot, path), function.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit production tool tree: %v", err)
	}
	if executeCalls != 1 {
		t.Fatalf("tool implementation execution entry count = %d, want 1", executeCalls)
	}

	assertToolBoundaryTypes(t, repositoryRoot)
	assertLegacyToolPathHasNoProductionReferences(t, repositoryRoot)
}

func assertToolBoundaryTypes(t *testing.T, repositoryRoot string) {
	t.Helper()
	checks := []struct {
		file string
		fn   func(*ast.File) bool
		want string
	}{
		{
			file: "internal/tool/tool.go",
			want: "Definition interface without Execute",
			fn: func(parsed *ast.File) bool {
				definition := interfaceMethods(parsed, "Definition")
				return definition["Name"] && definition["Description"] && definition["Schema"] && definition["Risk"] && !definition["Execute"]
			},
		},
		{
			file: "internal/tool/validation.go",
			want: "ValidatedCall public Definition and private Tool executor",
			fn: func(parsed *ast.File) bool {
				fields := structFieldTypes(parsed, "ValidatedCall")
				return fields["Tool"] == "Definition" && fields["executor"] == "Tool"
			},
		},
		{
			file: "internal/tool/registry.go",
			want: "Registry Get/List return non-executable Definition views",
			fn: func(parsed *ast.File) bool {
				return functionResultContains(parsed, "Get", "Definition") && functionResultContains(parsed, "List", "Definition") && !hasMethod(parsed, "registeredTool", "Execute")
			},
		},
	}
	for _, check := range checks {
		path := filepath.Join(repositoryRoot, filepath.FromSlash(check.file))
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", check.file, err)
		}
		if !check.fn(parsed) {
			t.Errorf("missing sealed tool boundary: %s", check.want)
		}
	}
}

func assertLegacyToolPathHasNoProductionReferences(t *testing.T, repositoryRoot string) {
	t.Helper()
	legacy := map[string]bool{
		"ResolveProjectPath": true,
		"ReadProjectFile":    true,
		"WriteProjectFile":   true,
	}
	for _, tree := range []string{"cmd", "internal"} {
		root := filepath.Join(repositoryRoot, tree)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") || path == filepath.Join(repositoryRoot, "internal", "tool", "path.go") {
				return nil
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			toolAliases := map[string]bool{}
			if parsed.Name.Name == "tool" {
				toolAliases[""] = true
			}
			for _, spec := range parsed.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if importPath != "xagent/internal/tool" {
					continue
				}
				alias := "tool"
				if spec.Name != nil {
					alias = spec.Name.Name
				}
				toolAliases[alias] = true
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch function := call.Fun.(type) {
				case *ast.Ident:
					if toolAliases[""] && legacy[function.Name] {
						t.Errorf("legacy Tool path entry %s called by %s", function.Name, relativeAuditPath(repositoryRoot, path))
					}
				case *ast.SelectorExpr:
					alias, ok := function.X.(*ast.Ident)
					if ok && toolAliases[alias.Name] && legacy[function.Sel.Name] {
						t.Errorf("legacy Tool path entry %s called by %s", function.Sel.Name, relativeAuditPath(repositoryRoot, path))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("audit legacy tool path references in %s: %v", tree, err)
		}
	}
}

func receiverTypeName(fields *ast.FieldList) string {
	if fields == nil || len(fields.List) != 1 {
		return ""
	}
	expression := fields.List[0].Type
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}

func calledName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}

func isPrivateValidatedExecutor(expression ast.Expr) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "executor" {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == "validated"
}

func interfaceMethods(parsed *ast.File, name string) map[string]bool {
	methods := map[string]bool{}
	for _, declaration := range parsed.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, specification := range generic.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != name {
				continue
			}
			interfaceType, ok := typeSpec.Type.(*ast.InterfaceType)
			if !ok {
				return methods
			}
			for _, field := range interfaceType.Methods.List {
				for _, methodName := range field.Names {
					methods[methodName.Name] = true
				}
			}
		}
	}
	return methods
}

func structFieldTypes(parsed *ast.File, name string) map[string]string {
	fields := map[string]string{}
	for _, declaration := range parsed.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, specification := range generic.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != name {
				continue
			}
			structure, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				return fields
			}
			for _, field := range structure.Fields.List {
				identifier, ok := field.Type.(*ast.Ident)
				if !ok {
					continue
				}
				for _, fieldName := range field.Names {
					fields[fieldName.Name] = identifier.Name
				}
			}
		}
	}
	return fields
}

func functionResultContains(parsed *ast.File, name, typeName string) bool {
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != name || function.Type.Results == nil {
			continue
		}
		for _, result := range function.Type.Results.List {
			switch typed := result.Type.(type) {
			case *ast.Ident:
				if typed.Name == typeName {
					return true
				}
			case *ast.ArrayType:
				identifier, _ := typed.Elt.(*ast.Ident)
				if identifier != nil && identifier.Name == typeName {
					return true
				}
			}
		}
	}
	return false
}

func hasMethod(parsed *ast.File, receiver, name string) bool {
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name && receiverTypeName(function.Recv) == receiver {
			return true
		}
	}
	return false
}
