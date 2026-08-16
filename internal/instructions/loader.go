package instructions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"xagent/internal/budget"
	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/prompt"
	"xagent/internal/redact"
	"xagent/internal/safefs"
)

const maxInstructionIncludeDepth = 32

type Loader struct {
	ProjectRoot string
	UserDir     string
	Config      config.InstructionsConfig

	workspace *workspaceLoaderRoots
}

type workspaceLoaderRoots struct {
	project *frozenInstructionRoot
	user    *frozenInstructionRoot
	cache   *ExpansionCache
	config  config.InstructionsConfig
}

type frozenInstructionRoot struct {
	path     string
	identity []byte
}

// CachedLoader preserves the public loader shape while serializing loads.
// Filesystem freshness is never decided through path-based Stat metadata;
// every load re-enters the safefs Root chain and the graph cache remains
// available only through its budget-aware ExpansionCache contract.
type CachedLoader struct {
	Loader Loader
	mu     sync.Mutex
}

type loaderCache struct {
	sections    []prompt.Section
	diagnostics []diagnostics.Diagnostic
}

type instructionRoot struct {
	path   string
	handle *safefs.Root
	frozen *frozenInstructionRoot
}

type candidate struct {
	name     string
	path     string
	relative string
	root     *instructionRoot
	priority int
	scope    Scope
}

// NewWorkspaceLoader constructs a loader whose project and optional user roots
// are canonical, absolute and bound to their live directory identities. The
// returned Loader ignores later mutation of its exported compatibility fields.
func NewWorkspaceLoader(projectRoot, userRoot string, cfg config.InstructionsConfig) (Loader, error) {
	frozenConfig := cloneInstructionsConfig(normalizedConfig(cfg))
	if _, err := newInstructionCounter(frozenConfig); err != nil {
		return Loader{}, errors.New("instruction workspace config is invalid")
	}
	project, err := freezeInstructionRoot(projectRoot)
	if err != nil {
		return Loader{}, errors.New("instruction project root is invalid")
	}
	var user *frozenInstructionRoot
	if strings.TrimSpace(userRoot) != "" {
		user, err = freezeInstructionRoot(userRoot)
		if err != nil {
			return Loader{}, errors.New("instruction user root is invalid")
		}
	}
	return Loader{
		ProjectRoot: project.path,
		UserDir:     frozenInstructionRootPath(user),
		Config:      cloneInstructionsConfig(frozenConfig),
		workspace:   &workspaceLoaderRoots{project: project, user: user, cache: &ExpansionCache{}, config: frozenConfig},
	}, nil
}

func frozenInstructionRootPath(root *frozenInstructionRoot) string {
	if root == nil {
		return ""
	}
	return root.path
}

func (l Loader) Load(ctx context.Context) ([]prompt.Section, []diagnostics.Diagnostic) {
	result := l.load(ctx)
	return result.sections, result.diagnostics
}

func (l Loader) load(ctx context.Context) loaderCache {
	sources, items := l.loadSources(ctx)
	sections := make([]prompt.Section, 0, len(sources))
	for _, source := range sources {
		content := strings.TrimSpace(source.Content)
		if content == "" {
			continue
		}
		sections = append(sections, prompt.Section{
			Name:     source.Name,
			Priority: source.Priority,
			Content:  content,
			Stable:   true,
			Scope:    promptScope(source.Scope),
		})
	}
	return loaderCache{sections: sections, diagnostics: items}
}

func promptScope(scope Scope) prompt.Scope {
	switch scope {
	case ScopeUserDir:
		return prompt.ScopeUser
	case ScopeProjectRoot, ScopeProjectDir:
		return prompt.ScopeProject
	default:
		return ""
	}
}

func (l Loader) LoadSources(ctx context.Context) ([]Source, []diagnostics.Diagnostic) {
	return l.loadSources(ctx)
}

func (l Loader) loadSources(ctx context.Context) ([]Source, []diagnostics.Diagnostic) {
	if ctx == nil {
		return nil, []diagnostics.Diagnostic{newDiagnostic("instructions_context_cancelled", "指令加载上下文无效", "instructions", "")}
	}
	cfg := normalizedConfig(l.Config)
	if l.workspace != nil {
		cfg = cloneInstructionsConfig(l.workspace.config)
	}
	counter, err := newInstructionCounter(cfg)
	if err != nil {
		return nil, []diagnostics.Diagnostic{newDiagnostic("instructions_budget_invalid", err.Error(), "instructions", "")}
	}
	candidates, roots, items := l.candidates(ctx, cfg)
	sources := make([]Source, 0, len(candidates))

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			items = append(items, newDiagnostic("instructions_context_cancelled", err.Error(), candidate.name, candidate.relative))
			break
		}
		rootFile, ok, diag := readSafefsInstructionFile(ctx, candidate.root.handle, candidate.relative, cfg.MaxFileBytes, false, candidate.name, counter)
		if diag != nil {
			items = append(items, *diag)
		}
		if !ok {
			continue
		}
		observed := map[string]includeFile{rootFile.path: rootFile}
		reader := func(ctx context.Context, relative, _ string, maxBytes int64, sourceName string) (includeFile, bool, *diagnostics.Diagnostic) {
			file, found, item := readSafefsInstructionFile(ctx, candidate.root.handle, relative, maxBytes, true, sourceName, counter)
			if found {
				observed[file.path] = file
			}
			return file, found, item
		}
		expanded, includeDiagnostics, _ := expandIncludesWithDeps(ctx, includeRequest{
			content:          string(rootFile.content),
			baseDir:          pathpkg.Dir(candidate.relative),
			allowedRoot:      candidate.path,
			maxDepth:         cfg.MaxIncludeDepth,
			maxBytes:         cfg.MaxFileBytes,
			maxTotalBytes:    cfg.MaxTotalBytes,
			maxFiles:         cfg.MaxFiles,
			maxExpandedBytes: cfg.MaxExpandedBytes,
			sourceName:       candidate.name,
			rootIdentity:     rootFile.identity,
			rootFile:         rootFile,
			counter:          counter,
			read:             reader,
		})
		items = append(items, includeDiagnostics...)
		if l.workspace != nil && l.workspace.cache != nil {
			source, files, edges, graphErr := instructionGraphSnapshot(candidate, rootFile, observed)
			if graphErr == nil {
				key, keyErr := NewCacheKey(source, files, edges)
				if keyErr == nil {
					if cached, found := l.workspace.cache.lookupCharged(key); found {
						expanded = cached.Content
					} else {
						_ = l.workspace.cache.Store(key, CachedExpansion{Source: source, Content: expanded, Files: files, Edges: edges})
					}
				}
			}
		}
		sources = append(sources, Source{
			Name:     candidate.name,
			Path:     candidate.path,
			Priority: candidate.priority,
			Scope:    candidate.scope,
			Content:  expanded,
		})
	}

	rootChanged := false
	for _, root := range roots {
		if err := root.handle.Close(); err != nil {
			path := root.path
			if root.frozen != nil {
				path = ""
			}
			items = append(items, newDiagnostic("instructions_root_close_failed", "无法关闭指令根目录", "instructions", path))
		}
		if root.frozen != nil && !revalidateFrozenInstructionRoot(root.frozen) {
			rootChanged = true
		}
	}
	if rootChanged {
		items = append(items, newDiagnostic("instructions_root_changed", "指令根目录稳定身份发生变化，已拒绝本次结果", "instructions", ""))
		return nil, items
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Priority == sources[j].Priority {
			return sources[i].Name < sources[j].Name
		}
		return sources[i].Priority < sources[j].Priority
	})
	return sources, items
}

func instructionGraphSnapshot(candidate candidate, rootFile includeFile, observed map[string]includeFile) (GraphSource, []FileVersion, []IncludeEdge, error) {
	source := GraphSource{
		Name: candidate.name, Scope: candidate.scope, Priority: candidate.priority,
		RootPath: candidate.path, Root: rootFile.identity,
	}
	ordered := make([]includeFile, 0, len(observed))
	ordered = append(ordered, rootFile)
	known := map[FileIdentity]struct{}{rootFile.identity: {}}
	edges := make([]IncludeEdge, 0, len(observed))
	var visit func(includeFile)
	visit = func(parent includeFile) {
		position := uint64(0)
		for _, line := range strings.Split(string(parent.content), "\n") {
			includePath, ok := parseIncludeLine(line)
			if !ok {
				continue
			}
			childPath := includePath
			if !pathpkg.IsAbs(childPath) {
				childPath = pathpkg.Join(pathpkg.Dir(parent.path), childPath)
			}
			child, found := observed[childPath]
			if found && child.identity.valid() {
				edges = append(edges, IncludeEdge{From: parent.identity, To: child.identity, Position: position})
				if _, exists := known[child.identity]; !exists {
					known[child.identity] = struct{}{}
					ordered = append(ordered, child)
					visit(child)
				}
			}
			position++
		}
	}
	visit(rootFile)
	files := make([]FileVersion, 0, len(ordered))
	for _, file := range ordered {
		absolute := filepath.Join(candidate.root.path, filepath.FromSlash(file.path))
		version, err := NewFileVersion(absolute, file.identity, file.content)
		if err != nil {
			return GraphSource{}, nil, nil, err
		}
		files = append(files, version)
	}
	return source, files, edges, nil
}

func (l *CachedLoader) Load(ctx context.Context) ([]prompt.Section, []diagnostics.Diagnostic) {
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Loader.Load(ctx)
}

func (l Loader) candidates(ctx context.Context, cfg config.InstructionsConfig) ([]candidate, []*instructionRoot, []diagnostics.Diagnostic) {
	var candidates []candidate
	var roots []*instructionRoot
	var items []diagnostics.Diagnostic

	addRoot := func(rootPath, sourceName string, build func(*instructionRoot)) {
		if strings.TrimSpace(rootPath) == "" || ctx.Err() != nil {
			return
		}
		root, diag := openInstructionRoot(ctx, rootPath, sourceName)
		if diag != nil {
			items = append(items, *diag)
		}
		if root == nil {
			return
		}
		roots = append(roots, root)
		build(root)
	}
	addFrozenRoot := func(root *frozenInstructionRoot, sourceName string, build func(*instructionRoot)) {
		if root == nil || ctx.Err() != nil {
			return
		}
		opened, diag := openFrozenInstructionRoot(ctx, root, sourceName)
		if diag != nil {
			items = append(items, *diag)
		}
		if opened == nil {
			return
		}
		roots = append(roots, opened)
		build(opened)
	}
	addCandidate := func(root *instructionRoot, name string, priority int, scope Scope, parts ...string) {
		relative, err := canonicalInstructionRelative(parts...)
		if err != nil {
			items = append(items, newDiagnostic("instructions_path_invalid", "指令相对路径无效，已跳过", name, strings.Join(parts, "/")))
			return
		}
		candidates = append(candidates, candidate{
			name:     name,
			path:     filepath.Join(root.path, filepath.FromSlash(relative)),
			relative: relative,
			root:     root,
			priority: priority,
			scope:    scope,
		})
	}

	addProject := addRoot
	addUser := addRoot
	projectPath := strings.TrimSpace(l.ProjectRoot)
	userPath := l.userDir(cfg)
	if l.workspace != nil {
		addProject = func(_ string, sourceName string, build func(*instructionRoot)) {
			addFrozenRoot(l.workspace.project, sourceName, build)
		}
		addUser = func(_ string, sourceName string, build func(*instructionRoot)) {
			addFrozenRoot(l.workspace.user, sourceName, build)
		}
		projectPath = ""
		userPath = ""
	}
	addProject(projectPath, "项目指令根目录", func(root *instructionRoot) {
		for offset, projectFile := range projectFiles(cfg) {
			addCandidate(root, "项目根指令", PriorityProjectRoot+offset, ScopeProjectRoot, projectFile)
		}
		addCandidate(root, "项目目录指令", PriorityProjectDir, ScopeProjectDir, cfg.ProjectDir, cfg.ProjectFile)
	})
	addUser(userPath, "用户指令根目录", func(root *instructionRoot) {
		addCandidate(root, "用户指令", PriorityUserDir, ScopeUserDir, cfg.ProjectFile)
	})
	if err := ctx.Err(); err != nil {
		items = append(items, newDiagnostic("instructions_context_cancelled", err.Error(), "instructions", ""))
	}
	return candidates, roots, items
}

func openInstructionRoot(ctx context.Context, rootPath, sourceName string) (*instructionRoot, *diagnostics.Diagnostic) {
	if err := ctx.Err(); err != nil {
		item := newDiagnostic("instructions_context_cancelled", err.Error(), sourceName, "")
		return nil, &item
	}
	absolute, err := filepath.Abs(strings.TrimSpace(rootPath))
	if err != nil {
		item := newDiagnostic("instructions_root_open_failed", "无法解析指令根目录", sourceName, "")
		return nil, &item
	}
	absolute = filepath.Clean(absolute)
	if _, err := os.Lstat(absolute); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		item := newDiagnostic("instructions_root_open_failed", "无法检查指令根目录", sourceName, absolute)
		return nil, &item
	}
	opened, err := safefs.Bootstrap(absolute, safefs.Policy{})
	if err != nil || opened.Root == nil {
		item := newDiagnostic("instructions_root_open_failed", "无法安全打开指令根目录", sourceName, absolute)
		return nil, &item
	}
	if err := ctx.Err(); err != nil {
		_ = opened.Root.Close()
		item := newDiagnostic("instructions_context_cancelled", err.Error(), sourceName, absolute)
		return nil, &item
	}
	return &instructionRoot{path: absolute, handle: opened.Root}, nil
}

func freezeInstructionRoot(rootPath string) (*frozenInstructionRoot, error) {
	canonical, err := canonicalInstructionRoot(rootPath)
	if err != nil {
		return nil, err
	}
	opened, err := safefs.Bootstrap(canonical, safefs.Policy{})
	if err != nil || opened.Root == nil {
		return nil, errors.New("instruction root identity is unavailable")
	}
	identity, marshalErr := opened.Root.Identity().MarshalBinary()
	stablePath, pathErr := canonicalInstructionRoot(canonical)
	closeErr := opened.Root.Close()
	if marshalErr != nil || pathErr != nil || stablePath != canonical || closeErr != nil {
		return nil, errors.New("instruction root identity is unavailable")
	}
	return &frozenInstructionRoot{path: canonical, identity: append([]byte(nil), identity...)}, nil
}

func openFrozenInstructionRoot(ctx context.Context, frozen *frozenInstructionRoot, sourceName string) (*instructionRoot, *diagnostics.Diagnostic) {
	if frozen == nil || len(frozen.identity) == 0 {
		item := newDiagnostic("instructions_root_changed", "指令根目录稳定身份不可用，已拒绝", sourceName, "")
		return nil, &item
	}
	if err := ctx.Err(); err != nil {
		item := newDiagnostic("instructions_context_cancelled", err.Error(), sourceName, "")
		return nil, &item
	}
	if !frozenInstructionPathStable(frozen) {
		item := newDiagnostic("instructions_root_changed", "指令根目录稳定身份发生变化，已拒绝", sourceName, "")
		return nil, &item
	}
	opened, err := safefs.Bootstrap(frozen.path, safefs.Policy{})
	if err != nil || opened.Root == nil {
		item := newDiagnostic("instructions_root_changed", "指令根目录稳定身份发生变化，已拒绝", sourceName, "")
		return nil, &item
	}
	live, marshalErr := opened.Root.Identity().MarshalBinary()
	if marshalErr != nil || !bytes.Equal(frozen.identity, live) || !frozenInstructionPathStable(frozen) {
		_ = opened.Root.Close()
		item := newDiagnostic("instructions_root_changed", "指令根目录稳定身份发生变化，已拒绝", sourceName, "")
		return nil, &item
	}
	return &instructionRoot{path: frozen.path, handle: opened.Root, frozen: frozen}, nil
}

func revalidateFrozenInstructionRoot(frozen *frozenInstructionRoot) bool {
	if frozen == nil || len(frozen.identity) == 0 || !frozenInstructionPathStable(frozen) {
		return false
	}
	opened, err := safefs.Bootstrap(frozen.path, safefs.Policy{})
	if err != nil || opened.Root == nil {
		return false
	}
	live, marshalErr := opened.Root.Identity().MarshalBinary()
	closeErr := opened.Root.Close()
	return marshalErr == nil && closeErr == nil && bytes.Equal(frozen.identity, live) && frozenInstructionPathStable(frozen)
}

func frozenInstructionPathStable(frozen *frozenInstructionRoot) bool {
	if frozen == nil || frozen.path == "" {
		return false
	}
	canonical, err := canonicalInstructionRoot(frozen.path)
	return err == nil && canonical == frozen.path
}

func canonicalInstructionRoot(rootPath string) (string, error) {
	rootPath = strings.TrimSpace(rootPath)
	if rootPath == "" || len(rootPath) > maxGraphPathBytes || !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return "", errors.New("instruction root path is invalid")
	}
	resolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil || !filepath.IsAbs(resolved) {
		return "", errors.New("instruction root path is invalid")
	}
	return canonicalInstructionExistingPath(resolved)
}

func canonicalInstructionExistingPath(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("instruction path is not absolute")
	}
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	remainder := strings.TrimPrefix(path, current)
	if remainder == "" {
		return current, nil
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("instruction path is invalid")
		}
		candidate := filepath.Join(current, component)
		candidateInfo, err := os.Lstat(candidate)
		if err != nil {
			return "", err
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", err
		}
		actual := ""
		for _, entry := range entries {
			entryInfo, infoErr := entry.Info()
			if infoErr == nil && os.SameFile(candidateInfo, entryInfo) {
				actual = entry.Name()
				break
			}
		}
		if actual == "" {
			return "", errors.New("instruction path identity is unavailable")
		}
		current = filepath.Join(current, actual)
	}
	return filepath.Clean(current), nil
}

func readSafefsInstructionFile(ctx context.Context, root *safefs.Root, relative string, maxBytes int64, reportMissing bool, sourceName string, counter *budget.Counter) (includeFile, bool, *diagnostics.Diagnostic) {
	if err := ctx.Err(); err != nil {
		item := newDiagnostic("instructions_context_cancelled", err.Error(), sourceName, relative)
		return includeFile{}, false, &item
	}
	before, err := root.Bind(relative)
	if err != nil {
		if !reportMissing {
			return includeFile{}, false, nil
		}
		item := newDiagnostic("instructions_path_escape", "指令路径无法在允许根内安全绑定，已拒绝", sourceName, relative)
		return includeFile{}, false, &item
	}
	file, err := root.OpenRead(ctx, relative)
	if err != nil {
		if !reportMissing {
			return includeFile{}, false, nil
		}
		item := newDiagnostic("instructions_include_missing", "@include 文件不存在或无法安全打开，已跳过", sourceName, relative)
		return includeFile{}, false, &item
	}
	if err := counter.Consume(budget.Files, 1); err != nil {
		_ = file.Close()
		item := instructionBudgetDiagnostic(budget.Files, err, sourceName, relative)
		return includeFile{}, false, &item
	}

	content, readDiag := readBoundedInstructionContent(ctx, file, maxBytes, sourceName, relative, counter)
	closeErr := file.Close()
	if readDiag != nil {
		return includeFile{}, false, readDiag
	}
	if closeErr != nil {
		item := newDiagnostic("instructions_read_failed", "无法关闭已读取的指令文件", sourceName, relative)
		return includeFile{}, false, &item
	}
	if err := ctx.Err(); err != nil {
		item := newDiagnostic("instructions_context_cancelled", err.Error(), sourceName, relative)
		return includeFile{}, false, &item
	}
	after, err := root.Bind(relative)
	if err != nil || before != after {
		item := newDiagnostic("instructions_path_changed", "指令文件读取期间稳定身份发生变化，已拒绝", sourceName, relative)
		return includeFile{}, false, &item
	}
	identity, err := FileIdentityFromBinding(before)
	if err != nil {
		item := newDiagnostic("instructions_path_changed", "无法确认指令文件稳定身份，已拒绝", sourceName, relative)
		return includeFile{}, false, &item
	}
	return includeFile{content: content, path: relative, identity: identity, budgeted: true}, true, nil
}

func readBoundedInstructionContent(ctx context.Context, file *safefs.File, maxBytes int64, sourceName, relative string, counter *budget.Counter) ([]byte, *diagnostics.Diagnostic) {
	if maxBytes <= 0 {
		item := newDiagnostic("instructions_budget_invalid", "指令单文件预算无效", sourceName, relative)
		return nil, &item
	}
	capacity := maxBytes
	if capacity > 32*1024 {
		capacity = 32 * 1024
	}
	content := make([]byte, 0, int(capacity))
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			item := newDiagnostic("instructions_context_cancelled", err.Error(), sourceName, relative)
			return nil, &item
		}
		remaining := maxBytes - int64(len(content))
		readSize := int64(len(buffer))
		if remaining < readSize {
			readSize = remaining + 1
		}
		count, readErr := file.Read(buffer[:int(readSize)])
		if count > 0 {
			if err := counter.Consume(budget.Bytes, int64(count)); err != nil {
				item := instructionBudgetDiagnostic(budget.Bytes, err, sourceName, relative)
				return nil, &item
			}
			if int64(count) > remaining {
				item := newDiagnostic("instructions_file_too_large", fmt.Sprintf("指令文件超过大小限制：>%d", maxBytes), sourceName, relative)
				return nil, &item
			}
			content = append(content, buffer[:count]...)
		}
		if errors.Is(readErr, io.EOF) {
			return content, nil
		}
		if readErr != nil {
			item := newDiagnostic("instructions_read_failed", "无法读取指令文件", sourceName, relative)
			return nil, &item
		}
		if count == 0 {
			item := newDiagnostic("instructions_read_failed", "指令文件读取未取得进展", sourceName, relative)
			return nil, &item
		}
	}
}

func instructionBudgetDiagnostic(dimension budget.Dimension, err error, sourceName, relative string) diagnostics.Diagnostic {
	code := "instructions_budget_exceeded"
	switch dimension {
	case budget.Files:
		code = "instructions_files_limit"
	case budget.Bytes:
		code = "instructions_total_bytes_limit"
	case budget.ExpandedBytes:
		code = "instructions_expanded_bytes_limit"
	}
	return newDiagnostic(code, err.Error(), sourceName, relative)
}

func newInstructionCounter(cfg config.InstructionsConfig) (*budget.Counter, error) {
	requested := []struct {
		scope budget.Scope
		value int64
	}{
		{budget.InstructionsMaxFileBytes, cfg.MaxFileBytes},
		{budget.InstructionsMaxTotalBytes, cfg.MaxTotalBytes},
		{budget.InstructionsMaxFiles, cfg.MaxFiles},
		{budget.InstructionsMaxExpandedBytes, cfg.MaxExpandedBytes},
	}
	var effectiveEntries []budget.Limit
	var hardEntries []budget.Limit
	for _, item := range requested {
		spec, ok := instructionBudgetSpec(item.scope)
		if !ok {
			return nil, errors.New("instruction budget specification is unavailable")
		}
		if _, err := spec.Resolve(&item.value); err != nil {
			return nil, err
		}
		if item.scope == budget.InstructionsMaxFileBytes {
			continue
		}
		effectiveEntries = append(effectiveEntries, budget.Limit{Dimension: spec.Dimension, Value: item.value})
		hardEntries = append(hardEntries, budget.Limit{Dimension: spec.Dimension, Value: spec.HardCap})
	}
	if cfg.MaxIncludeDepth <= 0 || cfg.MaxIncludeDepth > maxInstructionIncludeDepth {
		return nil, errors.New("instruction include depth is outside the approved limits")
	}
	effective, err := budget.NewLimits(effectiveEntries...)
	if err != nil {
		return nil, err
	}
	hard, err := budget.NewLimits(hardEntries...)
	if err != nil {
		return nil, err
	}
	return budget.NewCounter(effective, hard)
}

func instructionBudgetSpec(scope budget.Scope) (budget.Spec, bool) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == scope {
			return spec, true
		}
	}
	return budget.Spec{}, false
}

func canonicalInstructionRelative(parts ...string) (string, error) {
	if len(parts) == 0 {
		return "", errors.New("instruction path is empty")
	}
	cleaned := make([]string, len(parts))
	for index, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || len(part) > maxGraphPathBytes || strings.Contains(part, "\\") || pathpkg.IsAbs(part) || pathpkg.Clean(part) != part {
			return "", errors.New("instruction path is invalid")
		}
		for _, component := range strings.Split(part, "/") {
			if component == "" || component == "." || component == ".." {
				return "", errors.New("instruction path is invalid")
			}
		}
		cleaned[index] = part
	}
	joined := pathpkg.Join(cleaned...)
	if len(joined) > maxGraphPathBytes {
		return "", errors.New("instruction path is too long")
	}
	return joined, nil
}

func projectFiles(cfg config.InstructionsConfig) []string {
	projectFile := strings.TrimSpace(cfg.ProjectFile)
	if projectFile == "" || projectFile == "MEWCODE.md" {
		return []string{"MEWCODE.md", "CLAUDE.md", "AGENTS.md"}
	}
	return []string{projectFile}
}

func (l Loader) userDir(cfg config.InstructionsConfig) string {
	if strings.TrimSpace(l.UserDir) != "" {
		return strings.TrimSpace(l.UserDir)
	}
	configured := strings.TrimSpace(cfg.UserDir)
	if configured == "" {
		return ""
	}
	if filepath.IsAbs(configured) {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, configured)
}

func normalizedConfig(cfg config.InstructionsConfig) config.InstructionsConfig {
	cfg.ProjectFile = strings.TrimSpace(cfg.ProjectFile)
	if cfg.ProjectFile == "" {
		cfg.ProjectFile = "MEWCODE.md"
	}
	cfg.ProjectDir = strings.TrimSpace(cfg.ProjectDir)
	if cfg.ProjectDir == "" {
		cfg.ProjectDir = ".mewcode"
	}
	cfg.UserDir = strings.TrimSpace(cfg.UserDir)
	if cfg.MaxIncludeDepth <= 0 {
		cfg.MaxIncludeDepth = 5
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 64 * 1024
	}
	if cfg.MaxTotalBytes <= 0 {
		cfg.MaxTotalBytes = defaultIncludeMaxTotalBytes
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = defaultIncludeMaxFiles
	}
	if cfg.MaxExpandedBytes <= 0 {
		cfg.MaxExpandedBytes = defaultIncludeMaxExpandedBytes
	}
	return cfg
}

func cloneInstructionsConfig(cfg config.InstructionsConfig) config.InstructionsConfig {
	cloned := cfg
	if cfg.Enabled != nil {
		enabled := *cfg.Enabled
		cloned.Enabled = &enabled
	}
	return cloned
}

func newDiagnostic(code string, message string, source string, path string) diagnostics.Diagnostic {
	return diagnostics.New(code, diagnostics.SeverityWarning, message).WithSource(source).WithPath(path).Safe(redact.Text)
}
