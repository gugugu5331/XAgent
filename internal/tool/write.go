package tool

import (
	"context"
	"fmt"
)

type WriteTool struct {
	projectRoot string
}

func NewWriteTool(projectRoot string) Tool {
	return &WriteTool{projectRoot: projectRoot}
}

func (t *WriteTool) Name() string { return "Write" }

func (t *WriteTool) Description() string {
	return "Write text content to a file inside the project. Read the target or related file first when modifying existing content; paths must stay within the project."
}

func (t *WriteTool) Risk() Risk { return RiskDangerous }

func (t *WriteTool) Schema() Schema {
	return ObjectSchema([]string{"path", "content"}, map[string]SchemaProperty{
		"path":    StringProperty("Path to write, relative to the project root."),
		"content": StringProperty("Text content to write."),
	})
}

func (t *WriteTool) Execute(ctx context.Context, input Input) Result {
	path, ok := stringArg(input.Arguments, "path")
	if !ok {
		return Failure(input, ErrInvalidArguments, "path 参数不能为空", true)
	}
	content, ok := input.Arguments["content"].(string)
	if !ok {
		return Failure(input, ErrInvalidArguments, "content 参数必须是字符串", true)
	}
	resolved, err := WriteProjectFile(t.projectRoot, path, []byte(content))
	if err != nil {
		return Failure(input, errorCode(err), fmt.Sprintf("写入文件失败: %v", err), true)
	}
	rel := RelativeToRoot(t.projectRoot, resolved)
	return Success(input, fmt.Sprintf("Wrote %s (%d bytes)", rel, len(content)), fmt.Sprintf("Wrote %d bytes to %s", len(content), rel), map[string]any{
		"path":  rel,
		"bytes": len(content),
	})
}
