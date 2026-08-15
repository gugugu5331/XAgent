package tool

import (
	"encoding/hex"
	"errors"
	"strings"

	"xagent/internal/artifact"
	"xagent/internal/redact"
)

// Result contains the four bounded safe views built together by ResultFactory.
// Raw stdout and stderr are not represented as dedicated fields.
type Result struct {
	CallID    string         `json:"call_id"`
	Name      string         `json:"name"`
	Status    ResultStatus   `json:"status"`
	Summary   string         `json:"summary"`
	Content   string         `json:"content"`
	Data      map[string]any `json:"data,omitempty"`
	Error     *Error         `json:"error,omitempty"`
	Truncated bool           `json:"truncated"`

	state            ExecutionState
	modelContent     redact.SafeText
	userView         UserView
	persistedContent redact.SafeText
	outputMeta       OutputMeta
}

// UserView is the user-facing projection of a tool result. Preview text and
// all descriptive metadata have already crossed the runtime redaction
// boundary. Artifact is an opaque reference, never a filesystem path.
type UserView struct {
	State            ExecutionState
	Status           ResultStatus
	Summary          redact.SafeText
	Preview          redact.SafeText
	Artifact         *artifact.Ref
	Truncated        bool
	TruncationReason redact.SafeText
	Error            *SafeError
}

// OutputMeta describes bounded output without retaining its raw bytes.
// CapturedBytes is the number of raw bytes accepted by Capture.
type OutputMeta struct {
	Artifact         *artifact.Ref
	Truncated        bool
	TruncationReason redact.SafeText
	CapturedBytes    int64
}

// SafeError is an error projection whose message has crossed the runtime
// redaction boundary.
type SafeError struct {
	Code        string
	Message     redact.SafeText
	Recoverable bool
}

// ExecutionState returns the lifecycle state recorded by ResultFactory.
func (r Result) ExecutionState() ExecutionState {
	return r.state
}

// ModelContent returns the bounded, redacted model projection.
func (r Result) ModelContent() redact.SafeText {
	return r.modelContent
}

// UserView returns an independent copy of the bounded, redacted user
// projection.
func (r Result) UserView() UserView {
	view := r.userView
	view.Artifact = cloneArtifactRef(view.Artifact)
	view.Error = cloneSafeError(view.Error)
	return view
}

// PersistedContent returns the redacted persistence projection. It excludes
// the user/model preview and contains only result metadata and an opaque
// artifact reference.
func (r Result) PersistedContent() redact.SafeText {
	return r.persistedContent
}

// OutputMeta returns an independent copy of artifact and truncation metadata.
func (r Result) OutputMeta() OutputMeta {
	meta := r.outputMeta
	meta.Artifact = cloneArtifactRef(meta.Artifact)
	return meta
}

func cloneArtifactRef(ref *artifact.Ref) *artifact.Ref {
	if ref == nil {
		return nil
	}
	cloned := *ref
	return &cloned
}

func cloneSafeError(safe *SafeError) *SafeError {
	if safe == nil {
		return nil
	}
	cloned := *safe
	return &cloned
}

func validateOpaqueArtifactRef(ref *artifact.Ref, capturedBytes int64) error {
	if ref == nil {
		return errors.New("artifact reference is unavailable")
	}
	if len(ref.ID) != 64 || ref.ID != strings.ToLower(ref.ID) {
		return errors.New("artifact reference identity is invalid")
	}
	decoded, err := hex.DecodeString(ref.ID)
	if err != nil || len(decoded) != 32 {
		return errors.New("artifact reference identity is invalid")
	}
	if capturedBytes <= 0 || ref.Bytes != capturedBytes {
		return errors.New("artifact reference byte count is invalid")
	}
	if ref.CreatedAt.IsZero() || !ref.Available {
		return errors.New("artifact reference is unavailable")
	}
	return nil
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
