package tool

import (
	"context"
	"fmt"
)

type ReadTool struct {
	projectRoot string
}

func NewReadTool(projectRoot string) Tool {
	return &ReadTool{projectRoot: projectRoot}
}

func (t *ReadTool) Name() string { return "Read" }

func (t *ReadTool) Description() string {
	return "Read a text file inside the project or an active Skill's read-only package root. Use this dedicated tool before editing or when the user asks to inspect file content; paths must stay within an allowed root."
}

func (t *ReadTool) Risk() Risk { return RiskSafe }

func (t *ReadTool) Schema() Schema {
	return ObjectSchema([]string{"path"}, map[string]SchemaProperty{
		"path": StringProperty("Path to read. Relative paths prefer the project root; absolute paths may target an active Skill's allowed package root."),
	})
}

func (t *ReadTool) Execute(ctx context.Context, input Input) Result {
	path, ok := stringArg(input.Arguments, "path")
	if !ok {
		return Failure(input, ErrInvalidArguments, "path 参数不能为空", true)
	}
	scope, err := effectiveReadScope(ctx, t.projectRoot)
	if err != nil {
		return Failure(input, errorCode(err), fmt.Sprintf("读取范围无效: %v", err), true)
	}
	target, data, err := readFileInScope(scope, path)
	if err != nil {
		return Failure(input, errorCode(err), fmt.Sprintf("读取文件失败: %v", err), true)
	}
	return Success(input, fmt.Sprintf("Read %s (%d bytes)", target.display, len(data)), string(data), map[string]any{
		"path":  target.display,
		"bytes": len(data),
	})
}
