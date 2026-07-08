package tool

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"xagent/internal/redact"
)

type BashTool struct {
	projectRoot string
}

func NewBashTool(projectRoot string) Tool {
	return &BashTool{projectRoot: projectRoot}
}

func (t *BashTool) Name() string { return "Bash" }

func (t *BashTool) Description() string {
	return "Run a shell command with the project root as working directory. Prefer dedicated tools for reading, searching, and editing; commands are not sandboxed and must be chosen cautiously."
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
		"stdout":    redact.Text(stdout.String()),
		"stderr":    redact.Text(stderr.String()),
		"exit_code": exitCode,
		"timed_out": ctx.Err() == context.DeadlineExceeded,
	}
	if ctx.Err() == context.DeadlineExceeded {
		content := bashResultContent(exitCode, data["stdout"].(string), data["stderr"].(string), true)
		return Result{CallID: input.CallID, Name: input.Name, Status: StatusTimeout, Summary: "Command timed out", Content: content, Data: data, Error: &Error{Code: ErrTimeout, Message: "命令执行超时", Recoverable: true}}
	}
	if err != nil {
		content := bashResultContent(exitCode, data["stdout"].(string), data["stderr"].(string), false)
		return Result{CallID: input.CallID, Name: input.Name, Status: StatusError, Summary: fmt.Sprintf("Command exited %d", exitCode), Content: content, Data: data, Error: &Error{Code: ErrCommandFailed, Message: err.Error(), Recoverable: true}}
	}
	return Success(input, fmt.Sprintf("Command exited %d", exitCode), data["stdout"].(string), data)
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
