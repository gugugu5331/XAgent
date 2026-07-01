package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type GlobTool struct {
	projectRoot string
}

func NewGlobTool(projectRoot string) Tool {
	return &GlobTool{projectRoot: projectRoot}
}

func (t *GlobTool) Name() string { return "Glob" }

func (t *GlobTool) Description() string {
	return "Find project files matching a glob pattern. Use this dedicated tool to discover project paths before reading or editing; matches must stay within the project."
}

func (t *GlobTool) Risk() Risk { return RiskSafe }

func (t *GlobTool) Schema() Schema {
	return ObjectSchema([]string{"pattern"}, map[string]SchemaProperty{
		"pattern": StringProperty("Glob pattern relative to the project root, for example internal/**/*.go."),
	})
}

func (t *GlobTool) Execute(ctx context.Context, input Input) Result {
	pattern, ok := stringArg(input.Arguments, "pattern")
	if !ok {
		return Failure(input, ErrInvalidArguments, "pattern 参数不能为空", true)
	}
	matches, err := filepath.Glob(filepath.Join(t.projectRoot, pattern))
	if err != nil {
		return Failure(input, ErrInvalidArguments, fmt.Sprintf("glob pattern 无效: %v", err), true)
	}
	files := make([]string, 0, len(matches))
	for _, match := range matches {
		resolved, err := ResolveProjectPath(t.projectRoot, match)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			continue
		}
		files = append(files, RelativeToRoot(t.projectRoot, resolved))
		if len(files) >= 200 {
			break
		}
	}
	return Success(input, fmt.Sprintf("Found %d files", len(files)), joinLines(files), map[string]any{
		"pattern": pattern,
		"count":   len(files),
		"files":   files,
	})
}
