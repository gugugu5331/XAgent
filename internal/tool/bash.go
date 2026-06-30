package tool

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

type BashTool struct {
	projectRoot string
}

func NewBashTool(projectRoot string) Tool {
	return &BashTool{projectRoot: projectRoot}
}

func (t *BashTool) Name() string { return "Bash" }

func (t *BashTool) Description() string {
	return "Run a shell command with the project root as working directory. This is not a sandbox."
}

func (t *BashTool) Risk() Risk { return RiskDangerous }

func (t *BashTool) Schema() Schema {
	return ObjectSchema([]string{"command"}, map[string]SchemaProperty{
		"command": StringProperty("Shell command to run from the project root."),
	})
}

func (t *BashTool) Execute(ctx context.Context, input Input) Result {
	command, ok := stringArg(input.Arguments, "command")
	if !ok {
		return Failure(input, ErrInvalidArguments, "command 参数不能为空", true)
	}

	shell := "/bin/sh"
	args := []string{"-c", command}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", command}
	}

	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Dir = t.projectRoot
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	data := map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": exitCode,
		"timed_out": ctx.Err() == context.DeadlineExceeded,
	}
	if ctx.Err() == context.DeadlineExceeded {
		return Result{CallID: input.CallID, Name: input.Name, Status: StatusTimeout, Summary: "Command timed out", Content: stderr.String(), Data: data, Error: &Error{Code: ErrTimeout, Message: "命令执行超时", Recoverable: true}}
	}
	if err != nil {
		return Result{CallID: input.CallID, Name: input.Name, Status: StatusError, Summary: fmt.Sprintf("Command exited %d", exitCode), Content: stderr.String(), Data: data, Error: &Error{Code: ErrCommandFailed, Message: err.Error(), Recoverable: true}}
	}
	return Success(input, fmt.Sprintf("Command exited %d", exitCode), stdout.String(), data)
}
