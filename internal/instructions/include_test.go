package instructions

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"xagent/internal/diagnostics"
)

func TestIncludeGraphDeduplicatesAndEnforcesCumulativeLimits(t *testing.T) {
	graphRoot := filepath.Clean("instruction-graph")
	identity := func(value byte) FileIdentity {
		return FileIdentity{digest: [32]byte{value}}
	}
	rootIdentity := identity(1)
	leftIdentity := identity(2)
	rightIdentity := identity(3)
	sharedIdentity := identity(4)
	rootContent := "root\n@include left.md\n@include right.md\n@include missing.md"
	leftContent := "left\n@include shared.md"
	rightContent := "right\n@include alias.md\n@include root.md"
	sharedContent := "shared"
	files := map[string]includeFile{
		filepath.Join(graphRoot, "left.md"): {
			content: []byte(leftContent), path: filepath.Join(graphRoot, "left.md"), identity: leftIdentity,
		},
		filepath.Join(graphRoot, "right.md"): {
			content: []byte(rightContent), path: filepath.Join(graphRoot, "right.md"), identity: rightIdentity,
		},
		filepath.Join(graphRoot, "shared.md"): {
			content: []byte(sharedContent), path: filepath.Join(graphRoot, "shared.md"), identity: sharedIdentity,
		},
		filepath.Join(graphRoot, "alias.md"): {
			content: []byte(sharedContent), path: filepath.Join(graphRoot, "alias.md"), identity: sharedIdentity,
		},
		filepath.Join(graphRoot, "root.md"): {
			content: []byte(rootContent), path: filepath.Join(graphRoot, "root.md"), identity: rootIdentity,
		},
	}
	reader := func(_ context.Context, path, _ string, _ int64, sourceName string) (includeFile, bool, *diagnostics.Diagnostic) {
		file, ok := files[filepath.Clean(path)]
		if ok {
			return file, true, nil
		}
		item := newDiagnostic("instructions_include_missing", "@include 文件不存在，已跳过", sourceName, filepath.Clean(path))
		return includeFile{}, false, &item
	}
	rawBytes := int64(len(rootContent) + len(leftContent) + len(rightContent) + len(sharedContent))
	want := "root\nleft\nshared\nright"
	baseRequest := includeRequest{
		content:          rootContent,
		baseDir:          graphRoot,
		allowedRoot:      graphRoot,
		maxDepth:         5,
		maxBytes:         1 << 10,
		maxTotalBytes:    rawBytes,
		maxFiles:         4,
		maxExpandedBytes: int64(len(want)),
		sourceName:       "test source",
		rootIdentity:     rootIdentity,
		read:             reader,
	}

	expanded, items, deps := expandIncludesWithDeps(context.Background(), baseRequest)
	if expanded != want || strings.Count(expanded, sharedContent) != 1 {
		t.Fatalf("expanded = %q, want stable-identity deduplicated %q", expanded, want)
	}
	if len(deps) != 5 {
		t.Fatalf("dependencies = %#v, want every encountered readable include", deps)
	}
	assertInstructionDiagnostic(t, items, "instructions_include_duplicate")
	assertInstructionDiagnostic(t, items, "instructions_include_cycle")
	assertInstructionDiagnostic(t, items, "instructions_include_missing")
	assertNoInstructionDiagnostic(t, items, "instructions_files_limit")
	assertNoInstructionDiagnostic(t, items, "instructions_total_bytes_limit")
	assertNoInstructionDiagnostic(t, items, "instructions_expanded_bytes_limit")

	fileLimited := baseRequest
	fileLimited.maxFiles = 3
	expanded, items, _ = expandIncludesWithDeps(context.Background(), fileLimited)
	if strings.Contains(expanded, "right") {
		t.Fatalf("file-limited expansion exposed rejected file content: %q", expanded)
	}
	assertInstructionDiagnostic(t, items, "instructions_files_limit")

	rawLimited := baseRequest
	rawLimited.maxTotalBytes = rawBytes - 1
	expanded, items, _ = expandIncludesWithDeps(context.Background(), rawLimited)
	if strings.Contains(expanded, "right") {
		t.Fatalf("raw-byte-limited expansion exposed rejected file content: %q", expanded)
	}
	assertInstructionDiagnostic(t, items, "instructions_total_bytes_limit")

	expandedLimited := baseRequest
	expandedLimited.maxExpandedBytes = int64(len(want) - 1)
	expanded, items, _ = expandIncludesWithDeps(context.Background(), expandedLimited)
	if strings.Contains(expanded, "right") {
		t.Fatalf("expanded-byte-limited result retained a partial rejected subtree: %q", expanded)
	}
	assertInstructionDiagnostic(t, items, "instructions_expanded_bytes_limit")

	depthLimited := baseRequest
	depthLimited.maxDepth = 1
	expanded, items, _ = expandIncludesWithDeps(context.Background(), depthLimited)
	if expanded != "root\nleft\nright" {
		t.Fatalf("depth-limited expansion = %q, want non-destructive skipped children", expanded)
	}
	assertInstructionDiagnostic(t, items, "instructions_include_too_deep")
}

func assertNoInstructionDiagnostic(t *testing.T, items []diagnostics.Diagnostic, code string) {
	t.Helper()
	for _, item := range items {
		if item.Code == code {
			t.Fatalf("diagnostics unexpectedly contain %q: %#v", code, items)
		}
	}
}
