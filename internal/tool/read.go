package tool

import (
	"context"
	"fmt"
	"os"
)

type ReadTool struct {
	projectRoot string
}

func NewReadTool(projectRoot string) Tool {
	return &ReadTool{projectRoot: projectRoot}
}

func (t *ReadTool) Name() string { return "Read" }

func (t *ReadTool) Description() string {
	return "Read a text file inside the project. Use this dedicated tool before editing or when the user asks to inspect file content; paths must stay within the project."
}

func (t *ReadTool) Risk() Risk { return RiskSafe }

func (t *ReadTool) Schema() Schema {
	return ObjectSchema([]string{"path"}, map[string]SchemaProperty{
		"path": StringProperty("Path to the text file to read, relative to the project root."),
	})
}

func (t *ReadTool) Execute(ctx context.Context, input Input) Result {
	path, ok := stringArg(input.Arguments, "path")
	if !ok {
		return Failure(input, ErrInvalidArguments, "path 参数不能为空", true)
	}
	resolved, err := ResolveProjectPath(t.projectRoot, path)
	if err != nil {
		return Failure(input, errorCode(err), err.Error(), true)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return Failure(input, ErrNotFound, fmt.Sprintf("读取文件失败: %v", err), true)
	}
	rel := RelativeToRoot(t.projectRoot, resolved)
	return Success(input, fmt.Sprintf("Read %s (%d bytes)", rel, len(data)), string(data), map[string]any{
		"path":  rel,
		"bytes": len(data),
	})
}
