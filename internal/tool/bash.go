package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"xagent/internal/artifact"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
)

type BashTool struct {
	projectRoot string
	runtime     bashRuntime
}

// BashRuntime contains only the narrow owners needed at the protected start
// boundary. It carries no filesystem write capability and cannot construct or
// widen a ProtectionPlan itself.
type BashRuntime struct {
	Runner           proctree.Runner
	Plans            proctree.ProtectionPlanFactory
	WorkingDirectory *safefs.Root
	Capture          func(context.Context, artifact.Metadata) (*Capture, error)
	ResultFactory    *ResultFactory
}

type bashRuntime struct {
	runner           proctree.Runner
	plans            proctree.ProtectionPlanFactory
	workingDirectory *safefs.Root
	capture          func(context.Context, artifact.Metadata) (*Capture, error)
	resultFactory    *ResultFactory
}

type shellSelection struct {
	executable string
	args       []string
	identity   string
}

func makeShellSelection(executable string, fixedArgs []string, command string) shellSelection {
	args := make([]string, 0, len(fixedArgs)+1)
	args = append(args, fixedArgs...)
	args = append(args, command)
	identityParts := make([]string, 0, len(fixedArgs)+1)
	identityParts = append(identityParts, executable)
	identityParts = append(identityParts, fixedArgs...)
	return shellSelection{
		executable: executable,
		args:       args,
		identity:   strings.Join(identityParts, "\x00"),
	}
}

func NewBashTool(projectRoot string) Tool {
	return &BashTool{projectRoot: projectRoot}
}

func NewProtectedBashTool(projectRoot string, runtime BashRuntime) (Tool, error) {
	if strings.TrimSpace(projectRoot) == "" || runtime.Runner == nil || runtime.Plans == nil ||
		runtime.WorkingDirectory == nil || runtime.WorkingDirectory.Identity() == (safefs.Identity{}) ||
		runtime.Capture == nil || runtime.ResultFactory == nil {
		return nil, errors.New("protected Bash runtime is unavailable")
	}
	return &BashTool{
		projectRoot: projectRoot,
		runtime: bashRuntime{
			runner:           runtime.Runner,
			plans:            runtime.Plans,
			workingDirectory: runtime.WorkingDirectory,
			capture:          runtime.Capture,
			resultFactory:    runtime.ResultFactory,
		},
	}, nil
}

func (t *BashTool) Name() string { return "Bash" }

func (t *BashTool) Description() string {
	return "Run a shell command through the protected process-tree runtime with the project root as working directory. Prefer dedicated tools for reading, searching, and editing."
}

func (t *BashTool) Risk() Risk { return RiskDangerous }

func (t *BashTool) UsesSafeResultBoundary() bool {
	return t != nil && t.runtime.valid()
}

func (t *BashTool) Schema() Schema {
	return ObjectSchema([]string{"command"}, map[string]SchemaProperty{
		"command": StringProperty("Shell command to run from the project root."),
	})
}

func (t *BashTool) Execute(ctx context.Context, input Input) Result {
	if t != nil && t.runtime.resultFactory != nil && t.runtime.capture != nil {
		return t.executeCaptured(ctx, input)
	}
	return t.executeLegacy(ctx, input)
}

func (t *BashTool) executeCaptured(ctx context.Context, input Input) Result {
	failure := func(state ExecutionState, status ResultStatus, code, summary, message string) Result {
		return buildSyntheticResult(t.runtime.resultFactory, ResultFactoryInput{CallID: input.CallID, Name: input.Name, State: state, Status: status, Summary: summary, Error: &Error{Code: code, Message: message, Recoverable: true}})
	}
	command, ok := stringArg(input.Arguments, "command")
	if !ok {
		return failure(Completed, StatusError, ErrInvalidArguments, "command 参数不能为空", "command 参数不能为空")
	}
	selection, err := selectShell(command)
	if err != nil {
		return failure(Rejected, StatusDenied, ErrPermissionDenied, "Shell execution is unavailable on this platform", "当前平台不支持 Shell 执行")
	}
	if ctx == nil || ctx.Err() != nil {
		return Result{}
	}
	capturedOutput, err := t.runtime.capture(ctx, artifact.Metadata{MediaType: "text/plain"})
	if err != nil || capturedOutput == nil {
		return failure(Completed, StatusError, ErrCommandFailed, "无法建立受限命令输出采集", "无法建立受限命令输出采集")
	}
	plan, err := t.runtime.plans.Create(ctx)
	if err != nil {
		captured, finishErr := capturedOutput.Finish(context.Background())
		return buildCapturedResult(t.runtime.resultFactory, ResultFactoryInput{CallID: input.CallID, Name: input.Name, State: Rejected, Status: StatusDenied, Summary: "Protected shell plan is unavailable", Error: &Error{Code: ErrPermissionDenied, Message: "受保护 Shell 执行不可用", Recoverable: true}}, captured, finishErr)
	}
	process, err := t.runtime.runner.Start(ctx, proctree.Request{
		Executable: selection.executable,
		Args:       append([]string(nil), selection.args...),
		WorkingDir: t.runtime.workingDirectory,
		Env:        append([]string(nil), os.Environ()...),
		Mode:       proctree.ProtectionRequired,
		Protection: plan,
	})
	if err != nil || process == nil {
		if process != nil {
			_ = process.Close(context.Background())
		}
		captured, finishErr := capturedOutput.Finish(context.Background())
		status := StatusDenied
		code := ErrPermissionDenied
		summary := "Protected shell start failed"
		state := Rejected
		if errors.Is(err, context.DeadlineExceeded) {
			status, code, summary = StatusTimeout, ErrTimeout, "Protected shell start timed out"
		}
		return buildCapturedResult(t.runtime.resultFactory, ResultFactoryInput{CallID: input.CallID, Name: input.Name, State: state, Status: status, Summary: summary, Error: &Error{Code: code, Message: "受保护 Shell 启动失败", Recoverable: true}}, captured, finishErr)
	}
	processResult, waitErr, captureErr, closeErr := runBashProcess(ctx, process, capturedOutput)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded) || processResult.TimedOut
	cancelled := errors.Is(ctx.Err(), context.Canceled) || processResult.Cancelled
	if captureErr == nil {
		switch {
		case timedOut, cancelled:
			_ = capturedOutput.MarkIncomplete(context.Canceled, CaptureTruncatedCanceled)
		case waitErr != nil || closeErr != nil:
			_ = capturedOutput.MarkIncomplete(errors.New("protected process output completion is uncertain"), CaptureTruncatedWriteFailure)
		}
	}
	captured, finishErr := capturedOutput.Finish(context.Background())
	resultInput := ResultFactoryInput{CallID: input.CallID, Name: input.Name, State: Completed, Status: StatusSuccess, Summary: fmt.Sprintf("Command exited %d", processResult.ExitCode)}
	switch {
	case timedOut:
		resultInput.State, resultInput.Status = CancelledAfterStart, StatusTimeout
		resultInput.Summary = "Command timed out"
		resultInput.Error = &Error{Code: ErrTimeout, Message: "命令执行超时", Recoverable: true}
	case cancelled:
		resultInput.State, resultInput.Status = CancelledAfterStart, StatusError
		resultInput.Summary = "Command cancelled"
		resultInput.Error = &Error{Code: ErrCommandFailed, Message: "命令执行已取消", Recoverable: true}
	case captured.TruncationReason == CaptureTruncatedHardLimit:
		resultInput.Status = StatusError
		resultInput.Summary = "Command output exceeded the hard limit"
		resultInput.Error = &Error{Code: ErrCommandFailed, Message: "命令输出超过硬上限", Recoverable: true}
	case waitErr != nil || captureErr != nil || finishErr != nil || closeErr != nil:
		resultInput.Status = StatusError
		resultInput.Summary = "Command execution failed"
		resultInput.Error = &Error{Code: ErrCommandFailed, Message: "受保护命令执行失败", Recoverable: true}
	case processResult.ExitCode != 0:
		resultInput.Status = StatusError
		resultInput.Error = &Error{Code: ErrCommandFailed, Message: "命令退出状态非零", Recoverable: true}
	}
	return buildCapturedResult(t.runtime.resultFactory, resultInput, captured, finishErr)
}

func (t *BashTool) executeLegacy(ctx context.Context, input Input) Result {
	command, ok := stringArg(input.Arguments, "command")
	if !ok {
		return Failure(input, ErrInvalidArguments, "command 参数不能为空", true)
	}

	selection, err := selectShell(command)
	if err != nil {
		return Result{
			CallID:  input.CallID,
			Name:    input.Name,
			Status:  StatusDenied,
			Summary: "Shell execution is unavailable on this platform",
			Error:   &Error{Code: ErrPermissionDenied, Message: "当前平台不支持 Shell 执行", Recoverable: false},
		}
	}
	if ctx == nil || t == nil || !t.runtime.valid() {
		return bashDenied(input, "Protected shell runtime is unavailable")
	}
	capture, err := t.runtime.capture(ctx, artifact.Metadata{MediaType: "text/plain"})
	if err != nil || capture == nil {
		return Failure(input, ErrCommandFailed, "无法建立受限命令输出采集", true)
	}
	plan, err := t.runtime.plans.Create(ctx)
	if err != nil {
		_, _ = capture.Finish(context.Background())
		return bashDenied(input, "Protected shell plan is unavailable")
	}
	process, err := t.runtime.runner.Start(ctx, proctree.Request{
		Executable: selection.executable,
		Args:       append([]string(nil), selection.args...),
		WorkingDir: t.runtime.workingDirectory,
		Env:        append([]string(nil), os.Environ()...),
		Mode:       proctree.ProtectionRequired,
		Protection: plan,
	})
	if err != nil || process == nil {
		if process != nil {
			_ = process.Close(context.Background())
		}
		_, _ = capture.Finish(context.Background())
		var startErr *proctree.StartError
		if errors.As(err, &startErr) && !startErr.TargetStarted {
			return bashDenied(input, "Protected shell start was refused")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return bashPreStartTimeout(input)
		}
		return bashDenied(input, "Protected shell start failed")
	}

	processResult, waitErr, captureErr, closeErr := runBashProcess(ctx, process, capture)
	captured, finishErr := capture.Finish(context.Background())
	preview := redact.Text(captured.Preview)
	exitCode := processResult.ExitCode
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded) || processResult.TimedOut
	cancelled := errors.Is(ctx.Err(), context.Canceled) || processResult.Cancelled
	data := map[string]any{
		"exit_code":         exitCode,
		"timed_out":         timedOut,
		"cancelled":         cancelled,
		"captured_bytes":    captured.CapturedBytes,
		"truncation_reason": string(captured.TruncationReason),
	}
	if captured.Artifact != nil {
		data["artifact"] = *cloneArtifactRef(captured.Artifact)
	}
	result := Result{
		CallID:    input.CallID,
		Name:      input.Name,
		Status:    StatusSuccess,
		Summary:   fmt.Sprintf("Command exited %d", exitCode),
		Content:   preview,
		Data:      data,
		Truncated: captured.Truncated,
		outputMeta: OutputMeta{
			Artifact:         cloneArtifactRef(captured.Artifact),
			Truncated:        captured.Truncated,
			TruncationReason: redact.NewRuntimeRedactor().Redact(string(captured.TruncationReason)),
		},
	}
	switch {
	case timedOut:
		result.Status = StatusTimeout
		result.Summary = "Command timed out"
		result.Error = &Error{Code: ErrTimeout, Message: "命令执行超时", Recoverable: true}
	case cancelled:
		result.Status = StatusError
		result.Summary = "Command cancelled"
		result.Error = &Error{Code: ErrCommandFailed, Message: "命令执行已取消", Recoverable: true}
	case captured.TruncationReason == CaptureTruncatedHardLimit:
		result.Status = StatusError
		result.Summary = "Command output exceeded the hard limit"
		result.Error = &Error{Code: ErrCommandFailed, Message: "命令输出超过硬上限", Recoverable: true}
	case waitErr != nil || captureErr != nil || finishErr != nil || closeErr != nil:
		result.Status = StatusError
		result.Summary = "Command execution failed"
		result.Error = &Error{Code: ErrCommandFailed, Message: "受保护命令执行失败", Recoverable: true}
	case exitCode != 0:
		result.Status = StatusError
		result.Error = &Error{Code: ErrCommandFailed, Message: fmt.Sprintf("命令退出码为 %d", exitCode), Recoverable: true}
	}
	return result
}

func (r bashRuntime) valid() bool {
	return r.runner != nil && r.plans != nil && r.workingDirectory != nil &&
		r.workingDirectory.Identity() != (safefs.Identity{}) && r.capture != nil && r.resultFactory != nil
}

type bashWaitResult struct {
	result proctree.Result
	err    error
}

func runBashProcess(ctx context.Context, process proctree.Process, capture *Capture) (proctree.Result, error, error, error) {
	if process == nil || capture == nil {
		return proctree.Result{}, errors.New("protected Bash process is unavailable"), nil, nil
	}
	pipes := process.Pipes()
	if pipes.Stdin == nil || pipes.Stdout == nil || pipes.Stderr == nil {
		closeErr := process.Close(context.Background())
		return proctree.Result{}, errors.New("protected Bash pipes are unavailable"), nil, closeErr
	}
	if err := process.CloseStdin(); err != nil {
		closeErr := process.Close(context.Background())
		return proctree.Result{}, errors.New("protected Bash stdin close failed"), nil, closeErr
	}

	copyResults := make(chan error, 2)
	go func() {
		_, err := io.Copy(capture, pipes.Stdout)
		copyResults <- err
	}()
	go func() {
		_, err := io.Copy(capture, pipes.Stderr)
		copyResults <- err
	}()
	waitResults := make(chan bashWaitResult, 1)
	go func() {
		result, err := process.Wait(context.Background())
		waitResults <- bashWaitResult{result: result, err: err}
	}()

	var closeOnce sync.Once
	var closeErr error
	closeProcess := func() {
		closeOnce.Do(func() { closeErr = process.Close(context.Background()) })
	}
	ctxDone := ctx.Done()
	copyCount := 0
	var captureErr error
	var waited bashWaitResult
	waitDone := false
	for copyCount < 2 || !waitDone {
		select {
		case <-ctxDone:
			ctxDone = nil
			closeProcess()
		case err := <-copyResults:
			copyCount++
			if err != nil && captureErr == nil {
				captureErr = err
				closeProcess()
			}
		case waited = <-waitResults:
			waitDone = true
		}
	}
	closeProcess()
	return waited.result, waited.err, captureErr, closeErr
}

func bashDenied(input Input, summary string) Result {
	return Result{
		CallID:  input.CallID,
		Name:    input.Name,
		Status:  StatusDenied,
		Summary: summary,
		Data:    map[string]any{"started": false},
		Error:   &Error{Code: ErrPermissionDenied, Message: "受保护 Shell 执行不可用", Recoverable: true},
	}
}

func bashPreStartTimeout(input Input) Result {
	return Result{
		CallID:  input.CallID,
		Name:    input.Name,
		Status:  StatusTimeout,
		Summary: "Protected shell start timed out",
		Data:    map[string]any{"started": false, "timed_out": true},
		Error:   &Error{Code: ErrTimeout, Message: "受保护 Shell 启动超时", Recoverable: true},
	}
}

func bashResultContent(exitCode int, stdout string, stderr string, timedOut bool) string {
	parts := []string{fmt.Sprintf("exit_code: %d", exitCode)}
	if timedOut {
		parts = append(parts, "timed_out: true")
	}
	parts = append(parts, "stdout:")
	if strings.TrimSpace(stdout) == "" {
		parts = append(parts, "(empty)")
	} else {
		parts = append(parts, stdout)
	}
	parts = append(parts, "stderr:")
	if strings.TrimSpace(stderr) == "" {
		parts = append(parts, "(empty)")
	} else {
		parts = append(parts, stderr)
	}
	return strings.Join(parts, "\n")
}
