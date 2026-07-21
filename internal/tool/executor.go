package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"xagent/internal/permission"
)

type Executor struct {
	Registry       *Registry
	ProjectRoot    string
	Timeout        time.Duration
	MaxOutputBytes int
}

func NewExecutor(registry *Registry, projectRoot string, timeout time.Duration, maxOutputBytes int) *Executor {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = 32 * 1024
	}
	return &Executor{Registry: registry, ProjectRoot: projectRoot, Timeout: timeout, MaxOutputBytes: maxOutputBytes}
}

func (e *Executor) NeedsConfirmation(call Call) bool {
	tool, ok := e.Registry.Get(call.Name)
	return ok && tool.Risk() == RiskDangerous
}

func (e *Executor) ExecuteAuthorized(ctx context.Context, call Call, grant permission.Grant) Result {
	permissionCall := permission.Call{ID: call.ID, Name: call.Name, ArgumentsJSON: call.ArgumentsJSON}
	var readRoots []string
	if scope, scopeErr := effectiveReadScope(ctx, e.ProjectRoot); scopeErr == nil {
		readRoots = scope.ExtraRoots
	} else {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Tool read scope is invalid for permission checking.", Recoverable: true})
	}
	normalized, err := permission.NormalizeCallWithReadRoots(permissionCall, e.ProjectRoot, readRoots)
	if err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Tool arguments are invalid for permission checking.", Recoverable: true})
	}
	if grant.Tool != call.Name || grant.Fingerprint != permission.Fingerprint(normalized) {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonRuleDeny, ModelMessage: "Permission grant does not match this tool call.", Recoverable: true})
	}
	if grant.CallID != "" && grant.CallID != call.ID && grant.Scope == permission.GrantOnce {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonRuleDeny, ModelMessage: "Permission grant does not match this tool call.", Recoverable: true})
	}
	return e.Execute(ctx, call)
}

func (e *Executor) Execute(ctx context.Context, call Call) Result {
	registeredTool, ok := e.Registry.Get(call.Name)
	if !ok {
		return Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  StatusError,
			Summary: fmt.Sprintf("未知工具: %s", call.Name),
			Error:   &Error{Code: ErrToolNotFound, Message: fmt.Sprintf("工具 %q 未注册", call.Name), Recoverable: true},
		}
	}

	arguments := map[string]any{}
	if strings.TrimSpace(call.ArgumentsJSON) != "" {
		if err := json.Unmarshal([]byte(call.ArgumentsJSON), &arguments); err != nil {
			return Result{
				CallID:  call.ID,
				Name:    call.Name,
				Status:  StatusError,
				Summary: "工具参数不是有效 JSON",
				Error:   &Error{Code: ErrInvalidArguments, Message: err.Error(), Recoverable: true},
			}
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()

	resultCh := make(chan Result, 1)
	go func() {
		resultCh <- registeredTool.Execute(execCtx, Input{Name: call.Name, CallID: call.ID, RawArguments: call.ArgumentsJSON, Arguments: arguments})
	}()

	select {
	case <-execCtx.Done():
		return Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  StatusTimeout,
			Summary: "工具执行超时",
			Error:   &Error{Code: ErrTimeout, Message: "工具执行超过时间限制", Recoverable: true},
			Data:    map[string]any{"timed_out": true},
		}
	case result := <-resultCh:
		return e.truncate(result)
	}
}

func (e *Executor) permissionDenied(call Call, decision permission.Decision) Result {
	message := permission.DeniedModelMessage(decision)
	return Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  StatusDenied,
		Summary: "Permission denied before executing " + call.Name,
		Content: message,
		Data:    permission.DeniedResultData(decision),
		Error:   &Error{Code: ErrPermissionDenied, Message: message, Recoverable: true},
	}
}

func (e *Executor) Denied(call Call) Result {
	return Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  StatusDenied,
		Summary: "用户拒绝执行工具",
		Content: "用户拒绝执行该工具调用。",
		Error:   &Error{Code: ErrPermissionDenied, Message: "用户拒绝执行该工具调用", Recoverable: true},
	}
}

func (e *Executor) truncate(result Result) Result {
	content, truncated := truncateString(result.Content, e.MaxOutputBytes)
	result.Content = content
	if truncated {
		result.Truncated = true
	}
	for _, key := range []string{"stdout", "stderr", "content"} {
		if value, ok := result.Data[key].(string); ok {
			truncatedValue, wasTruncated := truncateString(value, e.MaxOutputBytes/2)
			result.Data[key] = truncatedValue
			if wasTruncated {
				result.Truncated = true
			}
		}
	}
	if result.Truncated && !strings.Contains(result.Summary, "截断") {
		result.Summary += "（输出已截断）"
	}
	return result
}

func truncateString(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value, false
	}
	truncated := value[:maxBytes]
	for !utf8.ValidString(truncated) && len(truncated) > 0 {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + "\n...[truncated]", true
}
