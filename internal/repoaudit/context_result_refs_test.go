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

var contextResultForbiddenImports = map[string]struct{}{
	"crypto/sha256":            {},
	"embed":                    {},
	"io":                       {},
	"io/fs":                    {},
	"os":                       {},
	"path/filepath":            {},
	"xagent/internal/artifact": {},
}

var contextResultAccessors = map[string]struct{}{
	"ModelContent":     {},
	"UserView":         {},
	"PersistedContent": {},
	"OutputMeta":       {},
}

var contextResultLegacySelectors = map[string]struct{}{
	"CallID":    {},
	"Name":      {},
	"Summary":   {},
	"Content":   {},
	"Data":      {},
	"Error":     {},
	"Truncated": {},
}

var contextResultForbiddenFunctions = map[string]struct{}{
	"externalizationCandidates":   {},
	"externalizeLargeToolResults": {},
	"externalizeMessage":          {},
	"externalizeMessages":         {},
	"conversationExternalMarker":  {},
	"safeFilePart":                {},
	"toolResultSize":              {},
}

var contextResultForbiddenCalls = map[string]struct{}{
	"Create":     {},
	"CreateTemp": {},
	"Mkdir":      {},
	"MkdirAll":   {},
	"Open":       {},
	"OpenFile":   {},
	"Read":       {},
	"ReadFile":   {},
	"Write":      {},
	"WriteFile":  {},
}

// TestContextResultRefsProductionBoundary seals ContextManager at the safe
// tool.Result projection boundary. Artifact payload access belongs to the
// Artifact Store owner; ContextManager may only consume the four immutable
// views produced by tool.ResultFactory.
func TestContextResultRefsProductionBoundary(t *testing.T) {
	repositoryRoot := contextResultRepositoryRoot(t)
	contextRoot := filepath.Join(repositoryRoot, "internal", "contextmgr")

	accessorCalls := map[string]int{}
	resultTypeReferences := 0
	projectMethods := 0

	err := filepath.WalkDir(contextRoot, func(path string, entry fs.DirEntry, walkErr error) error {
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
		relative := contextResultRelativePath(repositoryRoot, path)
		toolAliases, err := contextResultAuditImports(t, parsed, relative)
		if err != nil {
			return err
		}

		contextResultAuditCapabilityShape(t, parsed, relative)
		contextResultAuditForbiddenProductionNodes(t, parsed, relative)

		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if ok && toolAliases[qualifier.Name] && selector.Sel.Name == "Result" {
				resultTypeReferences++
			}
			return true
		})

		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			if function.Name.Name != "ProjectToolResult" {
				continue
			}
			projectMethods++
			contextResultAuditProjectionMethod(t, function, toolAliases, relative, accessorCalls)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit ContextManager production boundary: %v", err)
	}

	if projectMethods != 1 {
		t.Errorf("ContextManager ProjectToolResult method count = %d, want 1", projectMethods)
	}
	if resultTypeReferences != 1 {
		t.Errorf("ContextManager tool.Result production type reference count = %d, want the sole ProjectToolResult parameter", resultTypeReferences)
	}
	for accessor := range contextResultAccessors {
		if accessorCalls[accessor] != 1 {
			t.Errorf("ContextManager direct %s call count = %d, want 1", accessor, accessorCalls[accessor])
		}
	}
}

func contextResultRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve context result audit source failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

func contextResultAuditImports(t *testing.T, parsed *ast.File, relative string) (map[string]bool, error) {
	t.Helper()
	toolAliases := map[string]bool{}
	for _, spec := range parsed.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		if _, forbidden := contextResultForbiddenImports[importPath]; forbidden {
			t.Errorf("ContextManager production file %s imports forbidden capability %s", relative, importPath)
		}
		if importPath != "xagent/internal/tool" {
			continue
		}
		alias := "tool"
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if alias == "." || alias == "_" {
			t.Errorf("ContextManager production file %s uses forbidden tool import alias %q", relative, alias)
			continue
		}
		toolAliases[alias] = true
	}
	return toolAliases, nil
}

func contextResultAuditCapabilityShape(t *testing.T, parsed *ast.File, relative string) {
	t.Helper()
	for _, declaration := range parsed.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		for _, specification := range generic.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok || (typeSpec.Name.Name != "Manager" && typeSpec.Name.Name != "ManagerOptions") {
				continue
			}
			structure, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Errorf("ContextManager %s in %s is not a concrete struct", typeSpec.Name.Name, relative)
				continue
			}
			for _, field := range structure.Fields.List {
				for _, name := range field.Names {
					lowerName := strings.ToLower(name.Name)
					for _, fragment := range []string{"artifact", "datadir", "file", "open", "path", "payload", "read", "store", "write"} {
						if strings.Contains(lowerName, fragment) {
							t.Errorf("ContextManager %s field %s in %s exposes forbidden capability", typeSpec.Name.Name, name.Name, relative)
						}
					}
				}
				contextResultAuditForbiddenType(t, field.Type, typeSpec.Name.Name, relative)
			}
		}
	}
}

func contextResultAuditForbiddenType(t *testing.T, expression ast.Expr, owner string, relative string) {
	t.Helper()
	ast.Inspect(expression, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		switch strings.ToLower(identifier.Name) {
		case "file", "fs", "reader", "readwriter", "store", "writer":
			t.Errorf("ContextManager %s in %s contains forbidden field type %s", owner, relative, identifier.Name)
		}
		return true
	})
}

func contextResultAuditForbiddenProductionNodes(t *testing.T, parsed *ast.File, relative string) {
	t.Helper()
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok {
			if _, forbidden := contextResultForbiddenFunctions[function.Name.Name]; forbidden {
				t.Errorf("ContextManager production file %s retains forbidden raw externalization function %s", relative, function.Name.Name)
			}
			if function.Name.Name == "Open" || function.Name.Name == "Read" || function.Name.Name == "Write" {
				t.Errorf("ContextManager production file %s declares forbidden capability method/function %s", relative, function.Name.Name)
			}
		}
	}

	ast.Inspect(parsed, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.BasicLit:
			if typed.Kind == token.STRING {
				value, err := strconv.Unquote(typed.Value)
				if err == nil && strings.Contains(strings.ToLower(value), "context_blobs") {
					t.Errorf("ContextManager production file %s retains context_blobs path", relative)
				}
			}
		case *ast.CallExpr:
			if _, forbidden := contextResultForbiddenCalls[contextResultCalledName(typed.Fun)]; forbidden {
				t.Errorf("ContextManager production file %s calls forbidden file/payload operation %s", relative, contextResultCalledName(typed.Fun))
			}
		}
		return true
	})
}

func contextResultAuditProjectionMethod(t *testing.T, function *ast.FuncDecl, toolAliases map[string]bool, relative string, accessorCalls map[string]int) {
	t.Helper()
	if contextResultReceiverName(function.Recv) != "Manager" {
		t.Errorf("ProjectToolResult in %s is not a Manager method", relative)
	}

	resultParameter := ""
	resultParameters := 0
	for _, field := range function.Type.Params.List {
		if !contextResultIsToolResult(field.Type, toolAliases) {
			continue
		}
		resultParameters += len(field.Names)
		if len(field.Names) == 1 {
			resultParameter = field.Names[0].Name
		}
	}
	if resultParameters != 1 || resultParameter == "" {
		t.Errorf("ProjectToolResult in %s has %d named tool.Result parameters, want 1", relative, resultParameters)
		return
	}

	parents := map[ast.Node]ast.Node{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if node == nil {
			return true
		}
		ast.Inspect(node, func(child ast.Node) bool {
			if child != nil && child != node {
				if _, exists := parents[child]; !exists {
					parents[child] = node
				}
				return false
			}
			return true
		})
		return true
	})

	ast.Inspect(function.Body, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok || identifier.Name != resultParameter {
			return true
		}
		selector, ok := parents[identifier].(*ast.SelectorExpr)
		if !ok || selector.X != identifier {
			t.Errorf("ProjectToolResult in %s copies, passes, or indirectly reads its tool.Result parameter", relative)
			return true
		}
		if _, legacy := contextResultLegacySelectors[selector.Sel.Name]; legacy {
			t.Errorf("ProjectToolResult in %s reads forbidden legacy Result field %s", relative, selector.Sel.Name)
			return true
		}
		if _, allowed := contextResultAccessors[selector.Sel.Name]; !allowed {
			t.Errorf("ProjectToolResult in %s uses non-approved Result selector %s", relative, selector.Sel.Name)
			return true
		}
		call, ok := parents[selector].(*ast.CallExpr)
		if !ok || call.Fun != selector || len(call.Args) != 0 {
			t.Errorf("ProjectToolResult in %s does not call %s directly with zero arguments", relative, selector.Sel.Name)
			return true
		}
		accessorCalls[selector.Sel.Name]++
		return true
	})
}

func contextResultIsToolResult(expression ast.Expr, toolAliases map[string]bool) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Result" {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	return ok && toolAliases[qualifier.Name]
}

func contextResultReceiverName(fields *ast.FieldList) string {
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

func contextResultCalledName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}

func contextResultRelativePath(repositoryRoot string, path string) string {
	relative, err := filepath.Rel(repositoryRoot, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}
