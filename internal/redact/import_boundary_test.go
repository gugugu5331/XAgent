package redact

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSecurityBaseImportBoundaries(t *testing.T) {
	allowed := map[string]map[string]bool{
		"redact":      {},
		"budget":      {},
		"diagnostics": {"xagent/internal/budget": true, "xagent/internal/redact": true},
		"safefs":      {"xagent/internal/budget": true},
		"proctree":    {"xagent/internal/diagnostics": true, "xagent/internal/safefs": true},
		"artifact":    {"xagent/internal/budget": true},
		"netpolicy":   {},
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve security base source root failed")
	}
	internalRoot := filepath.Dir(filepath.Dir(currentFile))
	for packageName, packageAllowed := range allowed {
		entries, err := os.ReadDir(filepath.Join(internalRoot, packageName))
		if err != nil {
			t.Fatal("read security base package failed")
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			name := filepath.Join(internalRoot, packageName, entry.Name())
			parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly|parser.SkipObjectResolution)
			if err != nil {
				t.Fatal("parse security base package failed")
			}
			for _, spec := range parsed.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal("decode security base import failed")
				}
				if strings.HasPrefix(importPath, "xagent/internal/") && !packageAllowed[importPath] {
					t.Fatalf("security base package %s imports forbidden dependency %s", packageName, importPath)
				}
			}
		}
	}
}
