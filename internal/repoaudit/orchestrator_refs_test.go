package repoaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLegacyHistorySelectorsHaveNoProductionReference seals the isolated
// Skill history cutover. Production callers must use the Orchestrator's
// budgeted selector instead of either full-conversation copying helper.
func TestLegacyHistorySelectorsHaveNoProductionReference(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve Orchestrator reference audit source failed")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	forbiddenCalls := map[string]struct{}{
		"ContextMessages":     {},
		"RecentCompleteTurns": {},
	}

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
					t.Errorf("legacy history selector %s called by %s", name, relativeAuditPath(repositoryRoot, path))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("audit legacy history selector references in %s: %v", tree, err)
		}
	}
}
