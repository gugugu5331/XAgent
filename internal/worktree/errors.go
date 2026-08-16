package worktree

import "errors"

type ErrorCode string

const (
	CodeInvalidConfig       ErrorCode = "worktree_invalid_config"
	CodeInvalidLogicalName  ErrorCode = "worktree_invalid_logical_name"
	CodeUnsafePath          ErrorCode = "worktree_unsafe_path"
	CodeIdentityMismatch    ErrorCode = "worktree_identity_mismatch"
	CodeInvalidMetadata     ErrorCode = "worktree_invalid_metadata"
	CodeUnknownSchema       ErrorCode = "worktree_unknown_schema"
	CodeIntegrityMismatch   ErrorCode = "worktree_integrity_mismatch"
	CodeRevisionConflict    ErrorCode = "worktree_revision_conflict"
	CodeGitCommandRejected  ErrorCode = "worktree_git_command_rejected"
	CodeGitCommandFailed    ErrorCode = "worktree_git_command_failed"
	CodeGitOutputLimit      ErrorCode = "worktree_git_output_limit"
	CodeUnsupportedPlatform ErrorCode = "worktree_unsupported_platform"
	CodeLifecycleFailed     ErrorCode = "worktree_lifecycle_failed"
)

var (
	ErrInvalidConfig       = errors.New("invalid worktree configuration")
	ErrInvalidLogicalName  = errors.New("invalid logical name")
	ErrUnsafePath          = errors.New("unsafe managed path")
	ErrIdentityMismatch    = errors.New("identity mismatch")
	ErrInvalidMetadata     = errors.New("invalid worktree metadata")
	ErrUnknownSchema       = errors.New("unknown worktree metadata schema")
	ErrIntegrityMismatch   = errors.New("worktree metadata integrity mismatch")
	ErrMetadataTooLarge    = errors.New("worktree metadata exceeds limit")
	ErrRevisionConflict    = errors.New("worktree record revision conflict")
	ErrAlreadyExists       = errors.New("worktree record already exists")
	ErrNotFound            = errors.New("worktree record not found")
	ErrForbiddenGitCommand = errors.New("forbidden git command")
	ErrGitOutputLimit      = errors.New("git output exceeds limit")
)

type SafeError struct {
	Code        ErrorCode `json:"code"`
	Message     string    `json:"message"`
	Recoverable bool      `json:"recoverable,omitempty"`
	cause       error
}

func NewSafeError(code ErrorCode, message string, cause error) *SafeError {
	return &SafeError{Code: code, Message: message, cause: cause}
}

func (e *SafeError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *SafeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
