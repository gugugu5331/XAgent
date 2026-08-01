package tool

// Result contains bounded, model-visible execution data. Raw stdout and
// stderr are not represented as dedicated fields; later capture stages build
// the independent safe views from bounded previews and artifact metadata.
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
	ErrHookDenied                   = "hook_denied"
	ErrNoResults                    = "no_results"
	ErrInternalRoutingRequired      = "internal_routing_required"
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
