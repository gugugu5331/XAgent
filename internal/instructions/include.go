package instructions

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"xagent/internal/diagnostics"
)

type includeRequest struct {
	content     string
	baseDir     string
	allowedRoot string
	maxDepth    int
	maxBytes    int64
	sourceName  string
	visited     map[string]bool
	depth       int
}

func expandIncludes(ctx context.Context, req includeRequest) (string, []diagnostics.Diagnostic) {
	expanded, items, _ := expandIncludesWithDeps(ctx, req)
	return expanded, items
}

func expandIncludesWithDeps(ctx context.Context, req includeRequest) (string, []diagnostics.Diagnostic, []string) {
	var items []diagnostics.Diagnostic
	var deps []string
	lines := strings.Split(req.content, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			items = append(items, newDiagnostic("instructions_context_cancelled", err.Error(), req.sourceName, req.baseDir))
			return strings.Join(out, "\n"), items, deps
		}
		includePath, ok := parseIncludeLine(line)
		if !ok {
			out = append(out, line)
			continue
		}
		if req.depth >= req.maxDepth {
			items = append(items, newDiagnostic("instructions_include_too_deep", fmt.Sprintf("@include 超过最大深度 %d", req.maxDepth), req.sourceName, includePath))
			continue
		}
		path := includePath
		if !filepath.IsAbs(path) {
			path = filepath.Join(req.baseDir, path)
		}
		content, actualPath, ok, diag := readInstructionFile(path, req.allowedRoot, req.maxBytes, true, req.sourceName)
		if diag != nil {
			items = append(items, *diag)
		}
		if !ok {
			continue
		}
		deps = append(deps, actualPath)
		if req.visited[actualPath] {
			items = append(items, newDiagnostic("instructions_include_cycle", "检测到 @include 循环引用，已跳过", req.sourceName, actualPath))
			continue
		}
		nextVisited := copyVisited(req.visited)
		nextVisited[actualPath] = true
		expanded, nested, nestedDeps := expandIncludesWithDeps(ctx, includeRequest{
			content:     string(content),
			baseDir:     filepath.Dir(actualPath),
			allowedRoot: req.allowedRoot,
			maxDepth:    req.maxDepth,
			maxBytes:    req.maxBytes,
			sourceName:  req.sourceName,
			visited:     nextVisited,
			depth:       req.depth + 1,
		})
		items = append(items, nested...)
		deps = append(deps, nestedDeps...)
		if strings.TrimSpace(expanded) != "" {
			out = append(out, expanded)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n")), items, deps
}

func parseIncludeLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "@include") {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "@include"))
	if rest == "" {
		return "", false
	}
	rest = strings.Trim(rest, "\"'")
	if strings.TrimSpace(rest) == "" {
		return "", false
	}
	return rest, true
}

func copyVisited(visited map[string]bool) map[string]bool {
	copy := make(map[string]bool, len(visited)+1)
	for key, value := range visited {
		copy[key] = value
	}
	return copy
}
