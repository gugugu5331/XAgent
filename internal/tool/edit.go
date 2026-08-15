package tool

import (
	"context"
	"fmt"
	"io"
	"strings"
)

type EditTool struct {
	projectRoot   string
	resultFactory *ResultFactory
}

// NewEditTool is the isolated legacy adapter retained until T4.29a.
func NewEditTool(projectRoot string) Tool {
	return &EditTool{projectRoot: projectRoot}
}

func NewEditToolWithResultFactory(projectRoot string, factory *ResultFactory) (Tool, error) {
	if factory == nil {
		return nil, fmt.Errorf("safe Edit result factory is unavailable")
	}
	return &EditTool{projectRoot: projectRoot, resultFactory: factory}, nil
}

func (t *EditTool) Name() string { return "Edit" }

func (t *EditTool) Description() string {
	return "Replace one unique text occurrence in a project file. Read the file first so old_text matches exactly; paths must stay within the project."
}

func (t *EditTool) Risk() Risk { return RiskDangerous }

func (t *EditTool) UsesSafeResultBoundary() bool {
	return t != nil && t.resultFactory != nil
}

func (t *EditTool) Schema() Schema {
	return ObjectSchema([]string{"path", "old_text", "new_text"}, map[string]SchemaProperty{
		"path":     StringProperty("File path relative to the project root."),
		"old_text": StringProperty("Exact text to replace. Must occur exactly once."),
		"new_text": StringProperty("Replacement text."),
	})
}

func (t *EditTool) Execute(ctx context.Context, input Input) Result {
	if t != nil && t.resultFactory != nil {
		return t.executeSafe(ctx, input)
	}
	return t.executeLegacy(ctx, input)
}

func (t *EditTool) executeSafe(ctx context.Context, input Input) Result {
	failure := func(code, message string) Result {
		return buildSyntheticResult(t.resultFactory, ResultFactoryInput{CallID: input.CallID, Name: input.Name, State: Completed, Status: StatusError, Summary: message, Error: &Error{Code: code, Message: message, Recoverable: true}})
	}
	path, ok := stringArg(input.Arguments, "path")
	if !ok {
		return failure(ErrInvalidArguments, "path 参数不能为空")
	}
	oldText, ok := input.Arguments["old_text"].(string)
	if !ok || oldText == "" {
		return failure(ErrInvalidArguments, "old_text 参数不能为空")
	}
	newText, ok := input.Arguments["new_text"].(string)
	if !ok {
		return failure(ErrInvalidArguments, "new_text 参数必须是字符串")
	}
	execution, relative, err := writeTarget(ctx, path)
	if err != nil {
		return failure(errorCode(err), "读取文件失败: 目标不可读")
	}
	file, err := execution.root.OpenRead(ctx, relative)
	if err != nil {
		return failure(ErrPathOutsideProject, "读取文件失败: 目标不可读")
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return failure(ErrNotFound, "读取文件失败: 文件不可用")
	}
	content := string(data)
	count := strings.Count(content, oldText)
	if count == 0 {
		return failure(ErrNotFound, "old_text 未在文件中匹配到")
	}
	if count > 1 {
		return failure(ErrMultipleMatches, "old_text 必须唯一匹配")
	}
	if err := atomicWriteText(ctx, execution, relative, strings.Replace(content, oldText, newText, 1)); err != nil {
		return failure(ErrPathOutsideProject, "写回文件失败: 目标不可写")
	}
	return buildSyntheticResult(t.resultFactory, ResultFactoryInput{CallID: input.CallID, Name: input.Name, State: Completed, Status: StatusSuccess, Summary: "文件编辑完成", Preview: "Replaced one occurrence"})
}

func (t *EditTool) executeLegacy(ctx context.Context, input Input) Result {
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
	execution, relative, err := writeTarget(ctx, path)
	if err != nil {
		return Failure(input, errorCode(err), "读取文件失败: 目标不可读", true)
	}
	file, err := execution.root.OpenRead(ctx, relative)
	if err != nil {
		return Failure(input, ErrPathOutsideProject, "读取文件失败: 目标不可读", true)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return Failure(input, ErrNotFound, "读取文件失败: 文件不可用", true)
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
	if err := atomicWriteText(ctx, execution, relative, updated); err != nil {
		return Failure(input, ErrPathOutsideProject, "写回文件失败: 目标不可写", true)
	}
	return Success(input, fmt.Sprintf("Edited %s", relative), fmt.Sprintf("Replaced one occurrence in %s", relative), map[string]any{
		"path":         relative,
		"replacements": 1,
	})
}
