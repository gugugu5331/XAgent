package repoaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	providerImportPath  = "xagent/internal/provider"
	mcpclientImportPath = "xagent/internal/mcpclient"
)

var providerMCPDeletionCandidates = map[string]struct{}{
	"internal/provider/sse.go":          {},
	"internal/mcpclient/http.go":        {},
	"internal/mcpclient/http_closer.go": {},
	"internal/mcpclient/stdio.go":       {},
	"internal/mcpclient/stdio_unix.go":  {},
	"internal/mcpclient/stdio_other.go": {},
	"internal/mcpclient/jsonrpc.go":     {},
	"internal/mcpclient/protocol.go":    {},
	"internal/mcpclient/limits.go":      {},
	"internal/mcpclient/redact.go":      {},
}

var legacyProviderEntrypoints = map[string]struct{}{
	"New":      {},
	"SSEEvent": {},
}

var legacyMCPEntrypoints = map[string]struct{}{
	// Legacy HTTP transport and its private policy helpers.
	"HTTPConfig":               {},
	"HTTPTransport":            {},
	"NewHTTPTransport":         {},
	"ClosableHTTPTransport":    {},
	"NewClosableHTTPTransport": {},
	"headerContentType":        {},
	"headerAccept":             {},
	"headerSessionID":          {},
	"headerProtocolVersion":    {},
	"contentTypeJSON":          {},
	"contentTypeEventStream":   {},
	"acceptStreamableHTTPMCP":  {},
	"httpErrorBodyPreview":     {},
	"validateMCPHTTPURL":       {},
	"isBlockedHTTPHeader":      {},
	"protectRedirectHeaders":   {},
	"sameHost":                 {},

	// Legacy stdio transport and platform process helpers.
	"StdioConfig":             {},
	"StdioTransport":          {},
	"NewStdioTransport":       {},
	"maxStderrSummary":        {},
	"waitForClosed":           {},
	"ignoreExpectedWaitError": {},
	"configureProcessGroup":   {},
	"terminateProcessGroup":   {},

	// Legacy root-package JSON-RPC connection.
	"RPCID":               {},
	"StringID":            {},
	"NumberID":            {},
	"RPCRequest":          {},
	"RPCNotification":     {},
	"RPCResponse":         {},
	"RPCError":            {},
	"Transport":           {},
	"Connection":          {},
	"pendingRequest":      {},
	"NewConnection":       {},
	"marshalOptional":     {},
	"ValidateRPCResponse": {},

	// Legacy root-package MCP protocol client and DTOs.
	"SupportedProtocolVersion": {},
	"ClientInfo":               {},
	"InitializeRequest":        {},
	"InitializeResult":         {},
	"ListToolsRequest":         {},
	"ListToolsResult":          {},
	"RemoteTool":               {},
	"CallToolRequest":          {},
	"CallToolResult":           {},
	"ContentBlock":             {},
	"ProtocolClient":           {},
	"NewProtocolClient":        {},

	// Legacy response limits and package-local redaction boundary.
	"maxResponseBytes": {},
	"readLimited":      {},
	"RedactArguments":  {},
	"redactValue":      {},
	"RedactAny":        {},
	"RedactText":       {},
	"IsSensitiveKey":   {},

	// Transitional compatibility path that must disappear at T2.69.
	"legacyManagerSessionFactory": {},
	"legacyManagerSession":        {},
	"closeableTransport":          {},
	"legacyProtocolTools":         {},
	"NewToolAdapter":              {},
}

func TestLegacyProviderMCPEntrypointsHaveNoProductionReferences(t *testing.T) {
	if len(providerMCPDeletionCandidates) != 10 {
		t.Fatal("Provider/MCP deletion candidate allowlist must contain exactly ten files")
	}
	repositoryRoot := providerMCPRepositoryRoot(t)
	for relative := range providerMCPDeletionCandidates {
		info, err := os.Stat(filepath.Join(repositoryRoot, filepath.FromSlash(relative)))
		if err != nil || info.IsDir() {
			t.Fatalf("M5 Provider/MCP deletion candidate is missing: %s", relative)
		}
	}

	findings := map[string]map[string]bool{}
	for _, tree := range []string{"cmd", "internal"} {
		root := filepath.Join(repositoryRoot, tree)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(repositoryRoot, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if _, candidate := providerMCPDeletionCandidates[relative]; candidate {
				return nil
			}

			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			imports, err := providerMCPImports(parsed)
			if err != nil {
				return err
			}
			ignored := providerMCPIgnoredIdentifiers(parsed)
			ast.Inspect(parsed, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.SelectorExpr:
					qualifier, ok := typed.X.(*ast.Ident)
					if !ok {
						return true
					}
					if imports.providerAliases[qualifier.Name] {
						providerMCPRecordLegacyReference(findings, legacyProviderEntrypoints, typed.Sel.Name, relative)
					}
					if imports.mcpAliases[qualifier.Name] {
						providerMCPRecordLegacyReference(findings, legacyMCPEntrypoints, typed.Sel.Name, relative)
					}
				case *ast.Ident:
					if ignored[typed] {
						return true
					}
					if parsed.Name.Name == "provider" || imports.dotProvider {
						providerMCPRecordLegacyReference(findings, legacyProviderEntrypoints, typed.Name, relative)
					}
					if parsed.Name.Name == "mcpclient" || imports.dotMCP {
						providerMCPRecordLegacyReference(findings, legacyMCPEntrypoints, typed.Name, relative)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("audit legacy Provider/MCP production references in %s: %v", tree, err)
		}
	}
	paths := make([]string, 0, len(findings))
	for path := range findings {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		names := make([]string, 0, len(findings[path]))
		for name := range findings[path] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			t.Errorf("legacy Provider/MCP entrypoint %s remains in production file %s", name, path)
		}
	}
}

type providerMCPImportSet struct {
	providerAliases map[string]bool
	mcpAliases      map[string]bool
	dotProvider     bool
	dotMCP          bool
}

func providerMCPImports(parsed *ast.File) (providerMCPImportSet, error) {
	result := providerMCPImportSet{
		providerAliases: map[string]bool{},
		mcpAliases:      map[string]bool{},
	}
	for _, specification := range parsed.Imports {
		importPath, err := strconv.Unquote(specification.Path.Value)
		if err != nil {
			return providerMCPImportSet{}, err
		}
		if importPath != providerImportPath && importPath != mcpclientImportPath {
			continue
		}
		name := filepath.Base(importPath)
		if specification.Name != nil {
			name = specification.Name.Name
		}
		switch name {
		case "_":
			continue
		case ".":
			if importPath == providerImportPath {
				result.dotProvider = true
			} else {
				result.dotMCP = true
			}
		default:
			if importPath == providerImportPath {
				result.providerAliases[name] = true
			} else {
				result.mcpAliases[name] = true
			}
		}
	}
	return result, nil
}

// providerMCPIgnoredIdentifiers excludes syntactic names that cannot resolve
// to package-level entrypoints. Declaration names are deliberately retained:
// outside the exact M5 candidate files, redeclaring a legacy or transitional
// entrypoint would preserve the forbidden production surface.
func providerMCPIgnoredIdentifiers(parsed *ast.File) map[*ast.Ident]bool {
	ignored := map[*ast.Ident]bool{parsed.Name: true}
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.ImportSpec:
			if typed.Name != nil {
				ignored[typed.Name] = true
			}
		case *ast.Field:
			for _, name := range typed.Names {
				ignored[name] = true
			}
		case *ast.SelectorExpr:
			ignored[typed.Sel] = true
		case *ast.KeyValueExpr:
			if name, ok := typed.Key.(*ast.Ident); ok {
				ignored[name] = true
			}
		case *ast.LabeledStmt:
			ignored[typed.Label] = true
		case *ast.BranchStmt:
			if typed.Label != nil {
				ignored[typed.Label] = true
			}
		}
		return true
	})
	return ignored
}

func providerMCPRecordLegacyReference(findings map[string]map[string]bool, forbidden map[string]struct{}, name string, relative string) {
	if _, exists := forbidden[name]; !exists {
		return
	}
	if findings[relative] == nil {
		findings[relative] = map[string]bool{}
	}
	findings[relative][name] = true
}

func providerMCPRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve Provider/MCP reference audit source failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}
