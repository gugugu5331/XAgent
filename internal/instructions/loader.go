package instructions

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/prompt"
	"xagent/internal/redact"
)

type Scope string

const (
	ScopeProjectRoot Scope = "project_root"
	ScopeProjectDir  Scope = "project_dir"
	ScopeUserDir     Scope = "user_dir"
)

const (
	PriorityProjectRoot = 1000
	PriorityProjectDir  = 1010
	PriorityUserDir     = 1020
)

type Source struct {
	Name     string
	Path     string
	Priority int
	Scope    Scope
	Content  string
}

type Loader struct {
	ProjectRoot string
	UserDir     string
	Config      config.InstructionsConfig
}

type CachedLoader struct {
	Loader Loader
	mu     sync.RWMutex
	cache  *loaderCache
}

type loaderCache struct {
	sections     []prompt.Section
	diagnostics  []diagnostics.Diagnostic
	dependencies []fileDependency
}

type fileDependency struct {
	path    string
	modTime time.Time
	size    int64
	missing bool
}

type candidate struct {
	name        string
	path        string
	allowedRoot string
	priority    int
	scope       Scope
}

func (l Loader) Load(ctx context.Context) ([]prompt.Section, []diagnostics.Diagnostic) {
	result := l.load(ctx, false)
	return result.sections, result.diagnostics
}

func (l Loader) load(ctx context.Context, trackDeps bool) loaderCache {
	sources, items, deps := l.loadSources(ctx, trackDeps)
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
		})
	}
	return loaderCache{sections: sections, diagnostics: items, dependencies: deps}
}

func (l Loader) LoadSources(ctx context.Context) ([]Source, []diagnostics.Diagnostic) {
	sources, items, _ := l.loadSources(ctx, false)
	return sources, items
}

func (l Loader) loadSources(ctx context.Context, trackDeps bool) ([]Source, []diagnostics.Diagnostic, []fileDependency) {
	cfg := normalizedConfig(l.Config)
	candidates, items := l.candidates(cfg)
	sources := make([]Source, 0, len(candidates))
	var deps []fileDependency

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			items = append(items, newDiagnostic("instructions_context_cancelled", err.Error(), candidate.name, candidate.path))
			break
		}
		content, actualPath, ok, diag := readInstructionFile(candidate.path, candidate.allowedRoot, cfg.MaxFileBytes, false, candidate.name)
		if diag != nil {
			items = append(items, *diag)
		}
		if trackDeps {
			deps = append(deps, instructionDependency(candidate.path, actualPath, ok))
		}
		if !ok {
			continue
		}
		expanded, includeDiagnostics, includeDeps := expandIncludesWithDeps(ctx, includeRequest{
			content:     string(content),
			baseDir:     filepath.Dir(actualPath),
			allowedRoot: candidate.allowedRoot,
			maxDepth:    cfg.MaxIncludeDepth,
			maxBytes:    cfg.MaxFileBytes,
			sourceName:  candidate.name,
			visited:     map[string]bool{actualPath: true},
		})
		items = append(items, includeDiagnostics...)
		if trackDeps {
			for _, depPath := range includeDeps {
				deps = append(deps, instructionDependency(depPath, depPath, true))
			}
		}
		sources = append(sources, Source{
			Name:     candidate.name,
			Path:     actualPath,
			Priority: candidate.priority,
			Scope:    candidate.scope,
			Content:  expanded,
		})
	}

	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Priority == sources[j].Priority {
			return sources[i].Name < sources[j].Name
		}
		return sources[i].Priority < sources[j].Priority
	})
	return sources, items, deps
}

func (l *CachedLoader) Load(ctx context.Context) ([]prompt.Section, []diagnostics.Diagnostic) {
	if l == nil {
		return nil, nil
	}
	if cached, ok := l.cached(); ok {
		return cloneSections(cached.sections), cloneDiagnostics(cached.diagnostics)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cache != nil && dependenciesFresh(l.cache.dependencies) {
		return cloneSections(l.cache.sections), cloneDiagnostics(l.cache.diagnostics)
	}
	loaded := l.Loader.load(ctx, true)
	l.cache = &loaderCache{sections: cloneSections(loaded.sections), diagnostics: cloneDiagnostics(loaded.diagnostics), dependencies: cloneDependencies(loaded.dependencies)}
	return loaded.sections, loaded.diagnostics
}

func (l *CachedLoader) cached() (loaderCache, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.cache == nil || !dependenciesFresh(l.cache.dependencies) {
		return loaderCache{}, false
	}
	return loaderCache{sections: cloneSections(l.cache.sections), diagnostics: cloneDiagnostics(l.cache.diagnostics), dependencies: cloneDependencies(l.cache.dependencies)}, true
}

func instructionDependency(requestedPath string, actualPath string, exists bool) fileDependency {
	path := requestedPath
	if exists && actualPath != "" {
		path = actualPath
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileDependency{path: path, missing: true}
	}
	return fileDependency{path: path, modTime: info.ModTime(), size: info.Size()}
}

func dependenciesFresh(deps []fileDependency) bool {
	for _, dep := range deps {
		info, err := os.Stat(dep.path)
		if dep.missing {
			if err == nil {
				return false
			}
			continue
		}
		if err != nil || info.Size() != dep.size || !info.ModTime().Equal(dep.modTime) {
			return false
		}
	}
	return true
}

func cloneSections(sections []prompt.Section) []prompt.Section {
	items := make([]prompt.Section, len(sections))
	copy(items, sections)
	return items
}

func cloneDiagnostics(items []diagnostics.Diagnostic) []diagnostics.Diagnostic {
	copyItems := make([]diagnostics.Diagnostic, len(items))
	copy(copyItems, items)
	return copyItems
}

func cloneDependencies(deps []fileDependency) []fileDependency {
	items := make([]fileDependency, len(deps))
	copy(items, deps)
	return items
}

func (l Loader) candidates(cfg config.InstructionsConfig) ([]candidate, []diagnostics.Diagnostic) {
	var items []diagnostics.Diagnostic
	projectRoot := strings.TrimSpace(l.ProjectRoot)
	userDir := l.userDir(cfg)
	candidates := make([]candidate, 0, 3)

	if projectRoot != "" {
		for offset, projectFile := range projectFiles(cfg) {
			priority := PriorityProjectRoot + offset
			candidates = append(candidates, candidate{name: "项目根指令", path: filepath.Join(projectRoot, projectFile), allowedRoot: projectRoot, priority: priority, scope: ScopeProjectRoot})
		}
		projectDirPath := filepath.Join(projectRoot, cfg.ProjectDir, cfg.ProjectFile)
		candidates = append(candidates, candidate{name: "项目目录指令", path: projectDirPath, allowedRoot: projectRoot, priority: PriorityProjectDir, scope: ScopeProjectDir})
	}
	if userDir != "" {
		candidates = append(candidates, candidate{name: "用户指令", path: filepath.Join(userDir, cfg.ProjectFile), allowedRoot: userDir, priority: PriorityUserDir, scope: ScopeUserDir})
	}
	return candidates, items
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
	return cfg
}

func newDiagnostic(code string, message string, source string, path string) diagnostics.Diagnostic {
	return diagnostics.New(code, diagnostics.SeverityWarning, message).WithSource(source).WithPath(path).Safe(redact.Text)
}
