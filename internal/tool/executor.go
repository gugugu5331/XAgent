package tool

import (
	"context"
	"encoding/json"
	"errors"
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
	TicketVerifier permission.TicketVerifier
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

func (e *Executor) ExecuteAuthorized(ctx context.Context, call Call, ticket permission.ExecutionTicket) Result {
	validated, err := e.Registry.ValidateCall(call)
	if err != nil {
		return e.validationFailure(call, err)
	}
	return e.ExecuteValidatedAuthorized(ctx, validated, ticket)
}

// ExecuteValidatedAuthorized verifies and consumes a single-use execution
// ticket before executing the already parsed call.
func (e *Executor) ExecuteValidatedAuthorized(ctx context.Context, validated ValidatedCall, ticket permission.ExecutionTicket) Result {
	call := validated.Call
	registeredTool, ok := e.Registry.Get(call.Name)
	if !ok {
		return e.validationFailure(call, fmt.Errorf("%w: %q", errToolNotRegistered, call.Name))
	}
	if validated.Tool == nil || validated.Tool.Name() != call.Name || validated.Arguments == nil {
		return e.validationFailure(call, fmt.Errorf("validated call is inconsistent"))
	}
	// Always execute the registry member, even if a caller manually assembled a
	// ValidatedCall with another Tool implementation bearing the same name.
	validated.Tool = registeredTool
	var readRoots []string
	if scope, scopeErr := effectiveReadScope(ctx, e.ProjectRoot); scopeErr == nil {
		readRoots = scope.ExtraRoots
	} else {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Tool read scope is invalid for permission checking.", Recoverable: true})
	}
	normalized, err := permission.NormalizeArguments(
		permission.Call{ID: call.ID, Name: call.Name, ArgumentsJSON: call.ArgumentsJSON},
		validated.Arguments,
		permission.Context{ProjectRoot: e.ProjectRoot, ReadRoots: readRoots},
	)
	if err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Tool arguments are invalid for permission checking.", Recoverable: true})
	}
	canonicalArguments, err := json.Marshal(normalized.Arguments)
	if err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Tool arguments are invalid for permission checking.", Recoverable: true})
	}
	identity, err := permission.NewCallIdentity(permission.CallIdentityInput{ToolName: call.Name, CanonicalArguments: canonicalArguments})
	if err != nil || e.TicketVerifier == nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Execution ticket verification is unavailable.", Recoverable: true})
	}
	if err := e.TicketVerifier.VerifyAndConsume(ticket, call.ID, identity); err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonRuleDeny, ModelMessage: "Execution ticket does not match this tool call.", Recoverable: true})
	}
	return e.executeValidated(ctx, validated)
}

func (e *Executor) Execute(ctx context.Context, call Call) Result {
	validated, err := e.Registry.ValidateCall(call)
	if err != nil {
		return e.validationFailure(call, err)
	}
	return e.executeValidated(ctx, validated)
}

func (e *Executor) executeValidated(ctx context.Context, validated ValidatedCall) Result {
	call := validated.Call
	execCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()

	resultCh := make(chan Result, 1)
	go func() {
		resultCh <- validated.Tool.Execute(execCtx, Input{Name: call.Name, CallID: call.ID, RawArguments: call.ArgumentsJSON, Arguments: validated.Arguments})
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

func (e *Executor) validationFailure(call Call, err error) Result {
	if errors.Is(err, errToolNotRegistered) {
		return Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  StatusError,
			Summary: fmt.Sprintf("未知工具: %s", call.Name),
			Error:   &Error{Code: ErrToolNotFound, Message: fmt.Sprintf("工具 %q 未注册", call.Name), Recoverable: true},
		}
	}
	return Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  StatusError,
		Summary: "工具参数不是有效 JSON",
		Error:   &Error{Code: ErrInvalidArguments, Message: err.Error(), Recoverable: true},
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
	outputLimit := e.MaxOutputBytes
	if outputLimit <= 0 {
		outputLimit = 32 * 1024
	}
	fieldLimit := outputLimit
	if fieldLimit < len("（输出已截断）") {
		fieldLimit = len("（输出已截断）")
	}
	originalSummary := result.Summary
	result.Summary, _ = truncateString(result.Summary, fieldLimit)
	if result.Error != nil {
		cloned := *result.Error
		cloned.Message, result.Truncated = truncateAndMark(cloned.Message, fieldLimit, result.Truncated)
		result.Error = &cloned
	}
	content, truncated := truncateString(result.Content, outputLimit)
	result.Content = content
	if truncated {
		result.Truncated = true
	}
	dataLimit := outputLimit / 2
	if dataLimit < 1 {
		dataLimit = 1
	}
	for _, key := range []string{"stdout", "stderr", "content"} {
		if value, ok := result.Data[key].(string); ok {
			truncatedValue, wasTruncated := truncateString(value, dataLimit)
			result.Data[key] = truncatedValue
			if wasTruncated {
				result.Truncated = true
			}
		}
	}
	if len(originalSummary) > fieldLimit {
		result.Truncated = true
	}
	if result.Truncated && !strings.Contains(result.Summary, "截断") {
		result.Summary = truncatedSummary(originalSummary, fieldLimit)
	}
	return result
}

func truncateAndMark(value string, maxBytes int, alreadyTruncated bool) (string, bool) {
	value, truncated := truncateString(value, maxBytes)
	return value, alreadyTruncated || truncated
}

func truncatedSummary(value string, maxBytes int) string {
	const suffix = "（输出已截断）"
	if maxBytes < len(suffix) {
		maxBytes = len(suffix)
	}
	prefix, _ := utf8Prefix(value, maxBytes-len(suffix))
	return prefix + suffix
}

func truncateString(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value, false
	}
	const marker = "\n...[truncated]"
	if maxBytes <= len(marker) {
		return marker[:maxBytes], true
	}
	prefix, _ := utf8Prefix(value, maxBytes-len(marker))
	return prefix + marker, true
}

func utf8Prefix(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return "", value != ""
	}
	if len(value) <= maxBytes {
		return value, false
	}
	prefix := value[:maxBytes]
	for !utf8.ValidString(prefix) && len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix, true
}
