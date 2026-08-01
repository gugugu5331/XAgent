package tool

import (
	"context"
	"errors"
	"fmt"
	"io"

	"xagent/internal/budget"
)

const readChunkBytes = 32 * 1024

type ReadTool struct {
	projectRoot string
	limits      readLimits
}

type readLimits struct {
	fileBytes   int64
	lines       int64
	outputBytes int64
}

type readOutcome struct {
	content          []byte
	bytes            int64
	lines            int64
	truncated        bool
	truncationReason string
}

func NewReadTool(projectRoot string) Tool {
	return &ReadTool{projectRoot: projectRoot, limits: defaultReadLimits()}
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
	if ctx == nil || ctx.Err() != nil {
		return readFailure(input, ErrTimeout, "读取已取消")
	}
	path, ok := stringArg(input.Arguments, "path")
	if !ok {
		return Failure(input, ErrInvalidArguments, "path 参数不能为空", true)
	}
	scope, err := effectiveReadScope(ctx, t.projectRoot)
	if err != nil {
		return readFailure(input, errorCode(err), "读取范围无效")
	}
	target, file, closeRoot, err := openReadInScope(ctx, scope, path)
	if err != nil {
		if ctx.Err() != nil {
			return readFailure(input, ErrTimeout, "读取已取消")
		}
		return readFailure(input, errorCode(err), "无法打开请求的文件")
	}

	limits := t.limits
	execution := readExecutionFromContext(ctx)
	if execution.fileBytes != 0 || execution.lines != 0 || execution.outputBytes != 0 {
		limits = readLimits{
			fileBytes:   execution.fileBytes,
			lines:       execution.lines,
			outputBytes: execution.outputBytes,
		}
	}
	outcome, readErr := readBounded(ctx, file, limits)
	closeErr := file.Close()
	closeRoot()
	if readErr == nil && closeErr != nil {
		readErr = errors.New("close read handle")
	}

	content := boundedUTF8Preview(outcome.content, len(outcome.content))
	data := map[string]any{
		"path":  target.display,
		"bytes": outcome.bytes,
		"lines": outcome.lines,
	}
	if outcome.truncationReason != "" {
		data["truncation_reason"] = outcome.truncationReason
	}
	if readErr != nil {
		message := "读取文件时发生受限错误"
		code := ErrNotFound
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) || ctx.Err() != nil {
			message = "读取已取消"
			code = ErrTimeout
		}
		return Result{
			CallID:    input.CallID,
			Name:      input.Name,
			Status:    StatusError,
			Summary:   message,
			Content:   content,
			Data:      data,
			Error:     &Error{Code: code, Message: message, Recoverable: true},
			Truncated: outcome.truncated,
		}
	}

	summary := fmt.Sprintf("Read %s (%d bytes)", target.display, outcome.bytes)
	if outcome.truncated {
		summary = fmt.Sprintf("Read %s (%d bytes，已达到限制)", target.display, outcome.bytes)
	}
	return Result{
		CallID:    input.CallID,
		Name:      input.Name,
		Status:    StatusSuccess,
		Summary:   summary,
		Content:   content,
		Data:      data,
		Truncated: outcome.truncated,
	}
}

func readBounded(ctx context.Context, file io.Reader, limits readLimits) (readOutcome, error) {
	fileCounter, err := newReadCounter(budget.FilesReadMaxBytes, budget.Bytes, limits.fileBytes)
	if err != nil {
		return readOutcome{}, err
	}
	lineCounter, err := newReadCounter(budget.FilesScanMaxLines, budget.Lines, limits.lines)
	if err != nil {
		return readOutcome{}, err
	}
	outputCounter, err := newReadCounter(budget.ToolInlineOutputBytes, budget.Bytes, limits.outputBytes)
	if err != nil {
		return readOutcome{}, err
	}

	capacity := limits.outputBytes
	if capacity > 4*1024 {
		capacity = 4 * 1024
	}
	if capacity < 0 {
		capacity = 0
	}
	outcome := readOutcome{content: make([]byte, 0, int(capacity))}
	buffer := make([]byte, readChunkBytes)
	atLineStart := true

	for {
		if err := ctx.Err(); err != nil {
			outcome.truncated = len(outcome.content) > 0
			return outcome, err
		}
		fileRemaining := fileCounter.Remaining(budget.Bytes)
		outputRemaining := outputCounter.Remaining(budget.Bytes)
		remaining := fileRemaining
		reason := string(budget.FilesReadMaxBytes)
		if outputRemaining < remaining {
			remaining = outputRemaining
			reason = string(budget.ToolInlineOutputBytes)
		}
		request := remaining + 1
		if request > int64(len(buffer)) {
			request = int64(len(buffer))
		}
		if request < 1 {
			request = 1
		}

		if err := ctx.Err(); err != nil {
			outcome.truncated = len(outcome.content) > 0
			return outcome, err
		}
		count, readErr := file.Read(buffer[:int(request)])
		if count < 0 || count > int(request) {
			outcome.truncated = len(outcome.content) > 0
			return outcome, errors.New("read handle returned an invalid byte count")
		}
		accepted := int64(count)
		if accepted > remaining {
			accepted = remaining
			outcome.truncated = true
			outcome.truncationReason = reason
		}

		acceptedBytes := buffer[:int(accepted)]
		acceptedBytes, lineLimited := retainReadLines(lineCounter, acceptedBytes, &atLineStart, &outcome.lines)
		if lineLimited {
			outcome.truncated = true
			outcome.truncationReason = string(budget.FilesScanMaxLines)
		}
		if len(acceptedBytes) > 0 {
			amount := int64(len(acceptedBytes))
			if err := fileCounter.Consume(budget.Bytes, amount); err != nil {
				return outcome, err
			}
			if err := outputCounter.Consume(budget.Bytes, amount); err != nil {
				return outcome, err
			}
			outcome.content = append(outcome.content, acceptedBytes...)
			outcome.bytes += amount
		}

		if outcome.truncated {
			return outcome, nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return outcome, nil
			}
			outcome.truncated = len(outcome.content) > 0
			return outcome, errors.New("read handle failed")
		}
		if count == 0 {
			outcome.truncated = len(outcome.content) > 0
			return outcome, errors.New("read handle made no progress")
		}
	}
}

func retainReadLines(counter *budget.Counter, input []byte, atLineStart *bool, lines *int64) ([]byte, bool) {
	for index, value := range input {
		if *atLineStart {
			if err := counter.Consume(budget.Lines, 1); err != nil {
				return input[:index], true
			}
			(*lines)++
			*atLineStart = false
		}
		if value == '\n' {
			*atLineStart = true
		}
	}
	return input, false
}

func newReadCounter(scope budget.Scope, dimension budget.Dimension, value int64) (*budget.Counter, error) {
	var selected budget.Spec
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == scope {
			selected = spec
			break
		}
	}
	if selected.Scope == "" || selected.Dimension != dimension {
		return nil, errors.New("read budget specification is unavailable")
	}
	resolved, err := selected.Resolve(&value)
	if err != nil {
		return nil, errors.New("read budget is invalid")
	}
	effective, err := budget.NewLimits(budget.Limit{Dimension: dimension, Value: resolved})
	if err != nil {
		return nil, errors.New("read budget is invalid")
	}
	hard, err := budget.NewLimits(budget.Limit{Dimension: dimension, Value: selected.HardCap})
	if err != nil {
		return nil, errors.New("read budget hard limit is invalid")
	}
	return budget.NewCounter(effective, hard)
}

func defaultReadLimits() readLimits {
	return readLimits{
		fileBytes:   defaultBudgetValue(budget.FilesReadMaxBytes),
		lines:       defaultBudgetValue(budget.FilesScanMaxLines),
		outputBytes: defaultBudgetValue(budget.ToolInlineOutputBytes),
	}
}

func defaultBudgetValue(scope budget.Scope) int64 {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == scope {
			return spec.Default
		}
	}
	return 0
}

func readFailure(input Input, code, message string) Result {
	return Failure(input, code, message, true)
}
