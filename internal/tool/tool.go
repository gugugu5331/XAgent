package tool

import (
	"context"
	"encoding/json"
)

type Tool interface {
	Name() string
	Description() string
	Schema() Schema
	Risk() Risk
	Execute(ctx context.Context, input Input) Result
}

type Schema struct {
	Type       string                    `json:"type,omitempty"`
	Properties map[string]SchemaProperty `json:"properties,omitempty"`
	Required   []string                  `json:"required,omitempty"`
	Raw        json.RawMessage           `json:"-"`
}

func (s Schema) MarshalJSON() ([]byte, error) {
	if len(s.Raw) > 0 {
		return s.Raw, nil
	}
	type schema Schema
	return json.Marshal(schema(s))
}

type SchemaProperty struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type Risk string

const (
	RiskSafe      Risk = "safe"
	RiskDangerous Risk = "dangerous"
)

type Input struct {
	Name         string
	CallID       string
	RawArguments string
	Arguments    map[string]any
}

type Call struct {
	ID            string
	Name          string
	ArgumentsJSON string
}

type Result struct {
	CallID    string         `json:"call_id"`
	Name      string         `json:"name"`
	Status    ResultStatus   `json:"status"`
	Summary   string         `json:"summary"`
	Content   string         `json:"content"`
	Data      map[string]any `json:"data,omitempty"`
	Error     *Error         `json:"error,omitempty"`
	Truncated bool           `json:"truncated"`
}

type ResultStatus string

const (
	StatusSuccess ResultStatus = "success"
	StatusError   ResultStatus = "error"
	StatusDenied  ResultStatus = "denied"
	StatusTimeout ResultStatus = "timeout"
)

type Error struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}

const (
	ErrPathOutsideProject           = "path_outside_project"
	ErrNotFound                     = "not_found"
	ErrMultipleMatches              = "multiple_matches"
	ErrTimeout                      = "timeout"
	ErrInvalidArguments             = "invalid_arguments"
	ErrToolNotFound                 = "tool_not_found"
	ErrMultipleToolCallsUnsupported = "multiple_tool_calls_not_supported"
	ErrCommandFailed                = "command_failed"
	ErrPermissionDenied             = "permission_denied"
	ErrNoResults                    = "no_results"
)

func Success(call Input, summary string, content string, data map[string]any) Result {
	return Result{CallID: call.CallID, Name: call.Name, Status: StatusSuccess, Summary: summary, Content: content, Data: data}
}

func Failure(call Input, code string, message string, recoverable bool) Result {
	return Result{
		CallID:  call.CallID,
		Name:    call.Name,
		Status:  StatusError,
		Summary: message,
		Error:   &Error{Code: code, Message: message, Recoverable: recoverable},
	}
}
