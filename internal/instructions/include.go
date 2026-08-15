package instructions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
)

const (
	defaultIncludeMaxTotalBytes    int64 = 1 << 20
	defaultIncludeMaxFiles         int64 = 64
	defaultIncludeMaxExpandedBytes int64 = 2 << 20
)

var errIncludeExpansionLimit = errors.New("instruction include expansion limit reached")

type includeRequest struct {
	content          string
	baseDir          string
	allowedRoot      string
	maxDepth         int
	maxBytes         int64
	maxTotalBytes    int64
	maxFiles         int64
	maxExpandedBytes int64
	sourceName       string
	depth            int
	rootIdentity     FileIdentity
	rootFile         includeFile
	counter          *budget.Counter
	read             includeReadFunc
}

type includeReadFunc func(context.Context, string, string, int64, string) (includeFile, bool, *diagnostics.Diagnostic)

type includeFile struct {
	content  []byte
	path     string
	identity FileIdentity
	budgeted bool
}

type includeNode struct {
	file   includeFile
	active bool
}

type includeGraph struct {
	req         includeRequest
	counter     *budget.Counter
	nodes       []*includeNode
	diagnostics []diagnostics.Diagnostic
	deps        []string
}

type includeOutput struct {
	data  []byte
	items int
	limit int64
}

type includeCheckpoint struct {
	bytes int
	items int
}

func expandIncludes(ctx context.Context, req includeRequest) (string, []diagnostics.Diagnostic) {
	expanded, items, _ := expandIncludesWithDeps(ctx, req)
	return expanded, items
}

func expandIncludesWithDeps(ctx context.Context, req includeRequest) (string, []diagnostics.Diagnostic, []string) {
	if ctx == nil {
		item := newDiagnostic("instructions_context_cancelled", "指令 include 上下文无效", req.sourceName, req.baseDir)
		return "", []diagnostics.Diagnostic{item}, nil
	}
	normalized, counter, err := normalizeIncludeRequest(req)
	if err != nil {
		item := newDiagnostic("instructions_budget_invalid", err.Error(), req.sourceName, req.baseDir)
		return "", []diagnostics.Diagnostic{item}, nil
	}
	graph := &includeGraph{req: normalized, counter: counter}
	root := graph.rootFile()
	rootNode := &includeNode{file: root, active: true}
	graph.nodes = append(graph.nodes, rootNode)
	if !graph.reserveFile(root, len(normalized.content)) {
		return "", graph.diagnostics, graph.deps
	}

	output := includeOutput{limit: counter.Remaining(budget.ExpandedBytes)}
	if err := graph.expandNode(ctx, rootNode, normalized.content, normalized.baseDir, normalized.depth, &output); err != nil {
		return "", graph.diagnostics, graph.deps
	}
	trimmed := bytes.TrimSpace(output.data)
	if err := counter.Consume(budget.ExpandedBytes, int64(len(trimmed))); err != nil {
		graph.addBudgetDiagnostic(budget.ExpandedBytes, err, root.path)
		return "", graph.diagnostics, graph.deps
	}
	return string(trimmed), graph.diagnostics, graph.deps
}

func normalizeIncludeRequest(req includeRequest) (includeRequest, *budget.Counter, error) {
	if req.maxDepth <= 0 {
		req.maxDepth = 5
	}
	if req.maxBytes <= 0 {
		req.maxBytes = 64 * 1024
	}
	if req.maxTotalBytes <= 0 {
		req.maxTotalBytes = defaultIncludeMaxTotalBytes
	}
	if req.maxFiles <= 0 {
		req.maxFiles = defaultIncludeMaxFiles
	}
	if req.maxExpandedBytes <= 0 {
		req.maxExpandedBytes = defaultIncludeMaxExpandedBytes
	}
	if req.read == nil {
		return includeRequest{}, nil, errors.New("instruction include reader is unavailable")
	}
	if req.counter != nil {
		return req, req.counter, nil
	}
	limits, err := budget.NewLimits(
		budget.Limit{Dimension: budget.Bytes, Value: req.maxTotalBytes},
		budget.Limit{Dimension: budget.Files, Value: req.maxFiles},
		budget.Limit{Dimension: budget.ExpandedBytes, Value: req.maxExpandedBytes},
	)
	if err != nil {
		return includeRequest{}, nil, err
	}
	counter, err := budget.NewCounter(limits, limits)
	if err != nil {
		return includeRequest{}, nil, err
	}
	return req, counter, nil
}

func (g *includeGraph) expandNode(ctx context.Context, node *includeNode, content, baseDir string, depth int, output *includeOutput) error {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			g.diagnostics = append(g.diagnostics, newDiagnostic("instructions_context_cancelled", err.Error(), g.req.sourceName, node.file.path))
			node.active = false
			return err
		}
		includePath, ok := parseIncludeLine(line)
		if !ok {
			if err := output.appendLine(line); err != nil {
				g.addBudgetDiagnostic(budget.ExpandedBytes, err, node.file.path)
				node.active = false
				return err
			}
			continue
		}
		if depth >= g.req.maxDepth {
			g.diagnostics = append(g.diagnostics, newDiagnostic("instructions_include_too_deep", fmt.Sprintf("@include 超过最大深度 %d", g.req.maxDepth), g.req.sourceName, includePath))
			continue
		}
		path := includePath
		if !pathpkg.IsAbs(path) {
			path = pathpkg.Join(baseDir, path)
		}
		file, ok, diag := g.req.read(ctx, path, g.req.allowedRoot, g.req.maxBytes, g.req.sourceName)
		if diag != nil {
			g.diagnostics = append(g.diagnostics, *diag)
		}
		if !ok {
			continue
		}
		g.deps = append(g.deps, file.path)
		if previous := g.findNode(file); previous != nil {
			code := "instructions_include_duplicate"
			message := "检测到重复 @include，已按稳定文件身份跳过"
			if previous.active {
				code = "instructions_include_cycle"
				message = "检测到 @include 循环引用，已跳过"
			}
			g.diagnostics = append(g.diagnostics, newDiagnostic(code, message, g.req.sourceName, file.path))
			continue
		}

		child := &includeNode{file: file, active: true}
		g.nodes = append(g.nodes, child)
		if !g.reserveFile(file, len(file.content)) {
			child.active = false
			continue
		}
		checkpoint := output.checkpoint()
		err := g.expandNode(ctx, child, string(file.content), pathpkg.Dir(file.path), depth+1, output)
		if err != nil {
			output.rollback(checkpoint)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				node.active = false
				return err
			}
			continue
		}
		child.active = false
	}
	node.active = false
	return nil
}

func (g *includeGraph) reserveFile(file includeFile, contentBytes int) bool {
	if int64(contentBytes) > g.req.maxBytes {
		g.diagnostics = append(g.diagnostics, newDiagnostic("instructions_file_too_large", fmt.Sprintf("指令文件超过大小限制：%d > %d", contentBytes, g.req.maxBytes), g.req.sourceName, file.path))
		return false
	}
	if file.budgeted {
		return true
	}
	if err := g.counter.Consume(budget.Files, 1); err != nil {
		g.addBudgetDiagnostic(budget.Files, err, file.path)
		return false
	}
	if err := g.counter.Consume(budget.Bytes, int64(contentBytes)); err != nil {
		g.addBudgetDiagnostic(budget.Bytes, err, file.path)
		return false
	}
	return true
}

func (g *includeGraph) findNode(file includeFile) *includeNode {
	for _, node := range g.nodes {
		if sameIncludeFile(node.file, file) {
			return node
		}
	}
	return nil
}

func sameIncludeFile(left, right includeFile) bool {
	if left.identity.valid() && right.identity.valid() {
		return left.identity == right.identity
	}
	return left.path != "" && pathpkg.Clean(left.path) == pathpkg.Clean(right.path)
}

func (g *includeGraph) rootFile() includeFile {
	if g.req.rootFile.path != "" {
		root := g.req.rootFile
		root.content = []byte(g.req.content)
		return root
	}
	path := g.req.baseDir
	return includeFile{content: []byte(g.req.content), path: path, identity: g.req.rootIdentity}
}

func (g *includeGraph) addBudgetDiagnostic(dimension budget.Dimension, err error, path string) {
	code := "instructions_budget_exceeded"
	switch dimension {
	case budget.Files:
		code = "instructions_files_limit"
	case budget.Bytes:
		code = "instructions_total_bytes_limit"
	case budget.ExpandedBytes:
		code = "instructions_expanded_bytes_limit"
	}
	message := err.Error()
	if errors.Is(err, errIncludeExpansionLimit) {
		message = fmt.Sprintf("指令展开超过累计大小限制 %d", g.counter.Remaining(budget.ExpandedBytes))
	}
	g.diagnostics = append(g.diagnostics, newDiagnostic(code, message, g.req.sourceName, path))
}

func (o *includeOutput) appendLine(line string) error {
	additional := int64(len(line))
	if o.items > 0 {
		additional++
	}
	if additional > o.limit-int64(len(o.data)) {
		return errIncludeExpansionLimit
	}
	if o.items > 0 {
		o.data = append(o.data, '\n')
	}
	o.data = append(o.data, line...)
	o.items++
	return nil
}

func (o *includeOutput) checkpoint() includeCheckpoint {
	return includeCheckpoint{bytes: len(o.data), items: o.items}
}

func (o *includeOutput) rollback(checkpoint includeCheckpoint) {
	o.data = o.data[:checkpoint.bytes]
	o.items = checkpoint.items
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
