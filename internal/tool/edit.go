package tool

import (
	"context"
	"fmt"
	"strings"
)

type EditTool struct {
	projectRoot string
}

func NewEditTool(projectRoot string) Tool {
	return &EditTool{projectRoot: projectRoot}
}

func (t *EditTool) Name() string { return "Edit" }

func (t *EditTool) Description() string {
	return "Replace one unique text occurrence in a project file. Read the file first so old_text matches exactly; paths must stay within the project."
}

func (t *EditTool) Risk() Risk { return RiskDangerous }

func (t *EditTool) Schema() Schema {
	return ObjectSchema([]string{"path", "old_text", "new_text"}, map[string]SchemaProperty{
		"path":     StringProperty("File path relative to the project root."),
		"old_text": StringProperty("Exact text to replace. Must occur exactly once."),
		"new_text": StringProperty("Replacement text."),
	})
}

func (t *EditTool) Execute(ctx context.Context, input Input) Result {
	path, ok := stringArg(input.Arguments, "path")
	if !ok {
		return Failure(input, ErrInvalidArguments, "path 参数不能为空", true)
	}
	oldText, ok := input.Arguments["old_text"].(string)
	if !ok || oldText == "" {
		return Failure(input, ErrInvalidArguments, "old_text 参数不能为空", true)
	}
	newText, ok := input.Arguments["new_text"].(string)
	if !ok {
		return Failure(input, ErrInvalidArguments, "new_text 参数必须是字符串", true)
	}
	resolved, data, err := ReadProjectFile(t.projectRoot, path)
	if err != nil {
		return Failure(input, errorCode(err), fmt.Sprintf("读取文件失败: %v", err), true)
	}
	content := string(data)
	count := strings.Count(content, oldText)
	if count == 0 {
		return Failure(input, ErrNotFound, "old_text 未在文件中匹配到", true)
	}
	if count > 1 {
		return Failure(input, ErrMultipleMatches, fmt.Sprintf("old_text 匹配到 %d 次，必须唯一匹配", count), true)
	}
	updated := strings.Replace(content, oldText, newText, 1)
	if _, err := WriteProjectFile(t.projectRoot, path, []byte(updated)); err != nil {
		return Failure(input, ErrNotFound, fmt.Sprintf("写回文件失败: %v", err), true)
	}
	rel := RelativeToRoot(t.projectRoot, resolved)
	return Success(input, fmt.Sprintf("Edited %s", rel), fmt.Sprintf("Replaced one occurrence in %s", rel), map[string]any{
		"path":         rel,
		"replacements": 1,
	})
}
