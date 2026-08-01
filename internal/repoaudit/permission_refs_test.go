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

func TestLegacyGrantHasNoProductionExecutionReference(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve permission reference audit source failed")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	for _, tree := range []string{"cmd", "internal"} {
		root := filepath.Join(repositoryRoot, tree)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
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
			permissionAliases := map[string]bool{}
			dotPermissionImport := false
			for _, spec := range parsed.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if importPath != "xagent/internal/permission" {
					continue
				}
				name := "permission"
				if spec.Name != nil {
					name = spec.Name.Name
				}
				if name == "." {
					dotPermissionImport = true
				}
				permissionAliases[name] = true
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.Ident:
					if (parsed.Name.Name == "permission" || dotPermissionImport) && typed.Name == "Grant" {
						t.Errorf("legacy Grant remains in production file %s", relativeAuditPath(repositoryRoot, path))
					}
				case *ast.SelectorExpr:
					alias, ok := typed.X.(*ast.Ident)
					if ok && permissionAliases[alias.Name] && typed.Sel.Name == "Grant" {
						t.Errorf("legacy permission.Grant remains in production file %s", relativeAuditPath(repositoryRoot, path))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("audit production %s tree: %v", tree, err)
		}
	}
}

func relativeAuditPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(relative)
}
