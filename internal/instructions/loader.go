package instructions

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

type candidate struct {
	name        string
	path        string
	allowedRoot string
	priority    int
	scope       Scope
}

func (l Loader) Load(ctx context.Context) ([]prompt.Section, []diagnostics.Diagnostic) {
	sources, items := l.LoadSources(ctx)
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
	return sections, items
}

func (l Loader) LoadSources(ctx context.Context) ([]Source, []diagnostics.Diagnostic) {
	cfg := normalizedConfig(l.Config)
	candidates, items := l.candidates(cfg)
	sources := make([]Source, 0, len(candidates))

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			items = append(items, newDiagnostic("instructions_context_cancelled", err.Error(), candidate.name, candidate.path))
			break
		}
		content, actualPath, ok, diag := readInstructionFile(candidate.path, candidate.allowedRoot, cfg.MaxFileBytes, false, candidate.name)
		if diag != nil {
			items = append(items, *diag)
		}
		if !ok {
			continue
		}
		expanded, includeDiagnostics := expandIncludes(ctx, includeRequest{
			content:     string(content),
			baseDir:     filepath.Dir(actualPath),
			allowedRoot: candidate.allowedRoot,
			maxDepth:    cfg.MaxIncludeDepth,
			maxBytes:    cfg.MaxFileBytes,
			sourceName:  candidate.name,
			visited:     map[string]bool{actualPath: true},
		})
		items = append(items, includeDiagnostics...)
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
	return sources, items
}

func (l Loader) candidates(cfg config.InstructionsConfig) ([]candidate, []diagnostics.Diagnostic) {
	var items []diagnostics.Diagnostic
	projectRoot := strings.TrimSpace(l.ProjectRoot)
	userDir := l.userDir(cfg)
	candidates := make([]candidate, 0, 3)

	if projectRoot != "" {
		rootPath := filepath.Join(projectRoot, cfg.ProjectFile)
		projectDirPath := filepath.Join(projectRoot, cfg.ProjectDir, cfg.ProjectFile)
		candidates = append(candidates,
			candidate{name: "项目根指令", path: rootPath, allowedRoot: projectRoot, priority: PriorityProjectRoot, scope: ScopeProjectRoot},
			candidate{name: "项目目录指令", path: projectDirPath, allowedRoot: projectRoot, priority: PriorityProjectDir, scope: ScopeProjectDir},
		)
	}
	if userDir != "" {
		candidates = append(candidates, candidate{name: "用户指令", path: filepath.Join(userDir, cfg.ProjectFile), allowedRoot: userDir, priority: PriorityUserDir, scope: ScopeUserDir})
	}
	return candidates, items
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
