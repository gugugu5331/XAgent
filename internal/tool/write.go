package tool

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"xagent/internal/safefs"
)

var protectedPermissionSlots = []string{
	".xagent/permissions.yaml",
	".xagent/permissions.local.yaml",
}

// ProjectFilesystemPolicy returns a detached policy for the assembly root.
// The Capabilities container returned by Bootstrap must remain outside the
// tool package; Write and Edit receive only its Ordinary capability.
func ProjectFilesystemPolicy() safefs.Policy {
	return safefs.Policy{ProtectedSlots: append([]string(nil), protectedPermissionSlots...)}
}

type writeExecution struct {
	root       *safefs.Root
	rootPath   string
	capability safefs.Capability
}

type writeExecutionContextKey struct{}

func withWriteExecution(ctx context.Context, execution writeExecution) context.Context {
	return context.WithValue(ctx, writeExecutionContextKey{}, execution)
}

func writeExecutionFromContext(ctx context.Context) writeExecution {
	if ctx == nil {
		return writeExecution{}
	}
	execution, _ := ctx.Value(writeExecutionContextKey{}).(writeExecution)
	return execution
}

func writeTarget(ctx context.Context, requestedPath string) (writeExecution, string, error) {
	execution := writeExecutionFromContext(ctx)
	if execution.root == nil || execution.rootPath == "" {
		return writeExecution{}, "", fmt.Errorf("%s: write capability is unavailable", ErrPathOutsideProject)
	}
	relative, err := rootRelativeBindingPath(requestedPath, execution.rootPath)
	if err != nil {
		return writeExecution{}, "", fmt.Errorf("%s: invalid write target", ErrPathOutsideProject)
	}
	return execution, filepath.ToSlash(relative), nil
}

func atomicWriteText(ctx context.Context, execution writeExecution, relative, content string) error {
	return execution.root.AtomicWrite(ctx, execution.capability, relative, 0o600, func(destination io.Writer) error {
		_, err := io.WriteString(destination, content)
		return err
	})
}

type WriteTool struct {
	projectRoot   string
	resultFactory *ResultFactory
}

// NewWriteTool is the isolated legacy adapter retained until T4.29a.
func NewWriteTool(projectRoot string) Tool {
	return &WriteTool{projectRoot: projectRoot}
}

func NewWriteToolWithResultFactory(projectRoot string, factory *ResultFactory) (Tool, error) {
	if factory == nil {
		return nil, fmt.Errorf("safe Write result factory is unavailable")
	}
	return &WriteTool{projectRoot: projectRoot, resultFactory: factory}, nil
}

func (t *WriteTool) Name() string { return "Write" }

func (t *WriteTool) Description() string {
	return "Write text content to a file inside the project. Read the target or related file first when modifying existing content; paths must stay within the project."
}

func (t *WriteTool) Risk() Risk { return RiskDangerous }

func (t *WriteTool) UsesSafeResultBoundary() bool {
	return t != nil && t.resultFactory != nil
}

func (t *WriteTool) Schema() Schema {
	return ObjectSchema([]string{"path", "content"}, map[string]SchemaProperty{
		"path":    StringProperty("Path to write, relative to the project root."),
		"content": StringProperty("Text content to write."),
	})
}

func (t *WriteTool) Execute(ctx context.Context, input Input) Result {
	if t != nil && t.resultFactory != nil {
		return t.executeSafe(ctx, input)
	}
	return t.executeLegacy(ctx, input)
}

func (t *WriteTool) executeSafe(ctx context.Context, input Input) Result {
	failure := func(code, message string) Result {
		return buildSyntheticResult(t.resultFactory, ResultFactoryInput{CallID: input.CallID, Name: input.Name, State: Completed, Status: StatusError, Summary: message, Error: &Error{Code: code, Message: message, Recoverable: true}})
	}
	path, ok := stringArg(input.Arguments, "path")
	if !ok {
		return failure(ErrInvalidArguments, "path 参数不能为空")
	}
	content, ok := input.Arguments["content"].(string)
	if !ok {
		return failure(ErrInvalidArguments, "content 参数必须是字符串")
	}
	execution, relative, err := writeTarget(ctx, path)
	if err != nil {
		return failure(errorCode(err), "写入文件失败: 目标不可写")
	}
	if err := atomicWriteText(ctx, execution, relative, content); err != nil {
		return failure(ErrPathOutsideProject, "写入文件失败: 目标不可写")
	}
	return buildSyntheticResult(t.resultFactory, ResultFactoryInput{CallID: input.CallID, Name: input.Name, State: Completed, Status: StatusSuccess, Summary: "文件写入完成", Preview: fmt.Sprintf("Wrote %d bytes", len(content))})
}

func (t *WriteTool) executeLegacy(ctx context.Context, input Input) Result {
	path, ok := stringArg(input.Arguments, "path")
	if !ok {
		return Failure(input, ErrInvalidArguments, "path 参数不能为空", true)
	}
	content, ok := input.Arguments["content"].(string)
	if !ok {
		return Failure(input, ErrInvalidArguments, "content 参数必须是字符串", true)
	}
	execution, relative, err := writeTarget(ctx, path)
	if err != nil {
		return Failure(input, errorCode(err), "写入文件失败: 目标不可写", true)
	}
	if err := atomicWriteText(ctx, execution, relative, content); err != nil {
		return Failure(input, ErrPathOutsideProject, "写入文件失败: 目标不可写", true)
	}
	return Success(input, fmt.Sprintf("Wrote %s (%d bytes)", relative, len(content)), fmt.Sprintf("Wrote %d bytes to %s", len(content), relative), map[string]any{
		"path":  relative,
		"bytes": len(content),
	})
}
