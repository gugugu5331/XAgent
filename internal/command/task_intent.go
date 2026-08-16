package command

import (
	"fmt"
	"strings"
	"unicode"

	"xagent/internal/events"
	"xagent/internal/redact"
	"xagent/internal/subagent"
)

type TaskIntentKind string

const (
	TaskIntentSubmit              TaskIntentKind = "submit"
	TaskIntentList                TaskIntentKind = "list"
	TaskIntentDetail              TaskIntentKind = "detail"
	TaskIntentCancel              TaskIntentKind = "cancel"
	TaskIntentMoveToBackground    TaskIntentKind = "move_to_background"
	TaskIntentResolveConfirmation TaskIntentKind = "resolve_confirmation"
)

// TaskIntent contains only values supplied by the local user. App owns every
// trusted routing field before crossing the subagent.Service boundary.
type TaskIntent struct {
	Kind     TaskIntentKind
	Submit   subagent.SubmitInput
	TaskID   subagent.ID
	Decision events.ToolConfirmationDecision
}

// TaskIntentSink is the narrow command-to-App task boundary.
type TaskIntentSink interface {
	HandleTaskIntent(TaskIntent) error
}

func ParseAgentTaskIntent(input string) (TaskIntent, error) {
	typeToken, remainder := taskIntentToken(input)
	var executionType subagent.ExecutionType
	switch typeToken {
	case string(subagent.TypeDefined):
		executionType = subagent.TypeDefined
	case string(subagent.TypeFork):
		executionType = subagent.TypeFork
	default:
		return TaskIntent{}, agentTaskIntentError(subagent.ErrInvalidType, "用法: /agent defined|fork [--role=<name>] [--foreground|--background] [--] <task>")
	}

	inputValue := subagent.SubmitInput{Type: executionType, Placement: subagent.PlacementDefault}
	roleSeen := false
	placementSeen := false
	for {
		remainder = strings.TrimLeftFunc(remainder, unicode.IsSpace)
		if remainder == "" {
			return TaskIntent{}, agentTaskIntentError(subagent.ErrInvalidTask, "/agent 任务不能为空")
		}
		token, rest := taskIntentToken(remainder)
		switch {
		case token == "--":
			task := strings.TrimSpace(rest)
			if task == "" {
				return TaskIntent{}, agentTaskIntentError(subagent.ErrInvalidTask, "/agent 任务不能为空")
			}
			inputValue.Task = task
			return TaskIntent{Kind: TaskIntentSubmit, Submit: inputValue}, nil
		case token == "--foreground":
			if placementSeen {
				return TaskIntent{}, agentTaskIntentError(subagent.ErrInvalidPlacement, "/agent placement 只能指定一次")
			}
			inputValue.Placement = subagent.PlacementForeground
			placementSeen = true
			remainder = rest
		case token == "--background":
			if placementSeen {
				return TaskIntent{}, agentTaskIntentError(subagent.ErrInvalidPlacement, "/agent placement 只能指定一次")
			}
			inputValue.Placement = subagent.PlacementBackground
			placementSeen = true
			remainder = rest
		case strings.HasPrefix(token, "--placement="):
			if placementSeen {
				return TaskIntent{}, agentTaskIntentError(subagent.ErrInvalidPlacement, "/agent placement 只能指定一次")
			}
			placement, ok := parseTaskPlacement(strings.TrimPrefix(token, "--placement="))
			if !ok {
				return TaskIntent{}, agentTaskIntentError(subagent.ErrInvalidPlacement, "/agent placement 无效")
			}
			inputValue.Placement = placement
			placementSeen = true
			remainder = rest
		case strings.HasPrefix(token, "--role="):
			if roleSeen {
				return TaskIntent{}, agentTaskIntentError(subagent.ErrUnknownRole, "/agent role 只能指定一次")
			}
			role := strings.TrimPrefix(token, "--role=")
			if role == "" {
				return TaskIntent{}, agentTaskIntentError(subagent.ErrUnknownRole, "/agent role 不能为空")
			}
			inputValue.Role = role
			roleSeen = true
			remainder = rest
		case strings.HasPrefix(token, "--"):
			return TaskIntent{}, agentTaskIntentError(subagent.ErrInvalidTask, "/agent 参数无效")
		default:
			inputValue.Task = strings.TrimSpace(remainder)
			if inputValue.Task == "" {
				return TaskIntent{}, agentTaskIntentError(subagent.ErrInvalidTask, "/agent 任务不能为空")
			}
			return TaskIntent{Kind: TaskIntentSubmit, Submit: inputValue}, nil
		}
	}
}

func agentTaskIntentError(code subagent.ErrorCode, message string) error {
	return subagent.SafeError(code, redact.NewRuntimeRedactor().Redact(message), true)
}

func ParseTaskIntent(input string) (TaskIntent, error) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return TaskIntent{}, fmt.Errorf("用法: /task <task-id> [cancel|background|confirm ...]")
	}
	taskID := subagent.ID(fields[0])
	if len(fields) == 1 {
		return TaskIntent{Kind: TaskIntentDetail, TaskID: taskID}, nil
	}
	switch fields[1] {
	case "cancel":
		if len(fields) != 2 {
			break
		}
		return TaskIntent{Kind: TaskIntentCancel, TaskID: taskID}, nil
	case "background":
		if len(fields) != 2 {
			break
		}
		return TaskIntent{Kind: TaskIntentMoveToBackground, TaskID: taskID}, nil
	case "confirm":
		if len(fields) != 5 {
			break
		}
		decision, ok := parseTaskConfirmation(fields[2], fields[3], fields[4])
		if !ok {
			break
		}
		return TaskIntent{Kind: TaskIntentResolveConfirmation, TaskID: taskID, Decision: decision}, nil
	}
	return TaskIntent{}, fmt.Errorf("用法: /task <task-id> [cancel|background|confirm <confirmation-id> <call-id> <allow_once|allow_session|deny|cancel>]")
}

func taskIntentHandler(context ExecutionContext, invocation Invocation) error {
	var (
		intent TaskIntent
		err    error
	)
	switch invocation.CanonicalName {
	case "agent":
		intent, err = ParseAgentTaskIntent(invocation.Args)
	case "tasks":
		if strings.TrimSpace(invocation.Args) != "" {
			err = fmt.Errorf("用法: /tasks")
		} else {
			intent = TaskIntent{Kind: TaskIntentList}
		}
	case "task":
		intent, err = ParseTaskIntent(invocation.Args)
	default:
		err = fmt.Errorf("任务命令无效")
	}
	if err != nil {
		return err
	}
	sink, ok := context.Controller.(TaskIntentSink)
	if !ok {
		return fmt.Errorf("/%s 的任务控制器不可用", invocation.CanonicalName)
	}
	return sink.HandleTaskIntent(intent)
}

func taskIntentToken(input string) (string, string) {
	input = strings.TrimLeftFunc(input, unicode.IsSpace)
	if input == "" {
		return "", ""
	}
	index := strings.IndexFunc(input, unicode.IsSpace)
	if index < 0 {
		return input, ""
	}
	return input[:index], input[index:]
}

func parseTaskPlacement(value string) (subagent.PlacementIntent, bool) {
	switch subagent.PlacementIntent(value) {
	case subagent.PlacementDefault:
		return subagent.PlacementDefault, true
	case subagent.PlacementForeground:
		return subagent.PlacementForeground, true
	case subagent.PlacementBackground:
		return subagent.PlacementBackground, true
	default:
		return "", false
	}
}

func parseTaskConfirmation(confirmationID, callID, action string) (events.ToolConfirmationDecision, bool) {
	if confirmationID == "" || callID == "" {
		return events.ToolConfirmationDecision{}, false
	}
	decision := events.ToolConfirmationDecision{ConfirmationID: confirmationID, CallID: callID}
	switch events.PermissionAction(action) {
	case events.PermissionAllowOnce:
		decision.Action = events.PermissionAllowOnce
		decision.Allowed = true
	case events.PermissionAllowSession:
		decision.Action = events.PermissionAllowSession
		decision.Allowed = true
	case events.PermissionDeny:
		decision.Action = events.PermissionDeny
	case events.PermissionCancel:
		decision.Action = events.PermissionCancel
	default:
		// Permanent authorization is intentionally rejected together with every
		// unknown action; a child task cannot mutate persistent permission state.
		return events.ToolConfirmationDecision{}, false
	}
	return decision, true
}
