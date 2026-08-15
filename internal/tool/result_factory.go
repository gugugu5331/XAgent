package tool

import (
	"encoding/json"
	"errors"
	"fmt"

	"xagent/internal/artifact"
	"xagent/internal/redact"
)

// ResultFactory is the only constructor for the four safe result views. It
// retains the shared runtime redactor, never the preview or artifact payload.
type ResultFactory struct {
	redactor *redact.RuntimeRedactor
}

// ResultFactoryInput contains only bounded preview text and opaque artifact
// metadata. It deliberately has no raw stdout, raw stderr, buffer, or path
// field.
type ResultFactoryInput struct {
	CallID           string
	Name             string
	State            ExecutionState
	Status           ResultStatus
	Summary          string
	Preview          string
	Artifact         *artifact.Ref
	CapturedBytes    int64
	Truncated        bool
	TruncationReason string
	Error            *Error
}

func NewResultFactory(runtimeRedactor *redact.RuntimeRedactor) (*ResultFactory, error) {
	if runtimeRedactor == nil {
		return nil, errors.New("result factory requires a runtime redactor")
	}
	return &ResultFactory{redactor: runtimeRedactor}, nil
}

// Build creates the model, user, persistence, and output metadata projections
// together. Calls cancelled before their start boundary are intentionally not
// representable as Result values.
func (f *ResultFactory) Build(input ResultFactoryInput) (Result, error) {
	if f == nil || f.redactor == nil {
		return Result{}, errors.New("result factory is unavailable")
	}
	if !input.State.Valid() {
		return Result{}, fmt.Errorf("invalid execution state %q", input.State)
	}
	if !input.State.CanProduceResult() {
		return Result{}, fmt.Errorf("execution state %q cannot produce a result", input.State)
	}
	if !input.Status.valid() {
		return Result{}, fmt.Errorf("invalid result status %q", input.Status)
	}
	if input.CapturedBytes < 0 {
		return Result{}, errors.New("captured byte count is invalid")
	}
	ref := cloneArtifactRef(input.Artifact)
	if err := validateResultOutput(input, ref); err != nil {
		return Result{}, err
	}

	safeSummary := f.redactor.Redact(input.Summary)
	safePreview := f.redactor.Redact(input.Preview)
	safeReason := f.redactor.Redact(input.TruncationReason)
	safeError, compatibilityError := f.safeErrors(input.Error)

	modelPayload := resultModelPayload{
		State:            input.State,
		Status:           input.Status,
		Summary:          safeSummary.Text(),
		Preview:          safePreview.Text(),
		Artifact:         cloneArtifactRef(ref),
		Truncated:        input.Truncated,
		TruncationReason: safeReason.Text(),
		Error:            safeErrorPayloadFrom(safeError),
	}
	modelJSON, err := json.Marshal(modelPayload)
	if err != nil {
		return Result{}, errors.New("build model result view")
	}
	persistedPayload := resultPersistedPayload{
		State:            input.State,
		Status:           input.Status,
		Summary:          safeSummary.Text(),
		Artifact:         cloneArtifactRef(ref),
		Truncated:        input.Truncated,
		TruncationReason: safeReason.Text(),
		Error:            safeErrorPayloadFrom(safeError),
	}
	persistedJSON, err := json.Marshal(persistedPayload)
	if err != nil {
		return Result{}, errors.New("build persisted result view")
	}

	userView := UserView{
		State:            input.State,
		Status:           input.Status,
		Summary:          safeSummary,
		Preview:          safePreview,
		Artifact:         cloneArtifactRef(ref),
		Truncated:        input.Truncated,
		TruncationReason: safeReason,
		Error:            cloneSafeError(safeError),
	}
	outputMeta := OutputMeta{
		Artifact:         cloneArtifactRef(ref),
		Truncated:        input.Truncated,
		TruncationReason: safeReason,
		CapturedBytes:    input.CapturedBytes,
	}
	return Result{
		CallID:           input.CallID,
		Name:             input.Name,
		Status:           input.Status,
		Summary:          safeSummary.Text(),
		Content:          safePreview.Text(),
		Error:            compatibilityError,
		Truncated:        input.Truncated,
		state:            input.State,
		modelContent:     f.redactor.Redact(string(modelJSON)),
		userView:         userView,
		persistedContent: f.redactor.Redact(string(persistedJSON)),
		outputMeta:       outputMeta,
	}, nil
}

func (s ResultStatus) valid() bool {
	switch s {
	case StatusSuccess, StatusError, StatusDenied, StatusTimeout:
		return true
	default:
		return false
	}
}

func validateResultOutput(input ResultFactoryInput, ref *artifact.Ref) error {
	reason := CaptureTruncationReason(input.TruncationReason)
	if ref == nil {
		if input.Truncated {
			return errors.New("truncated output requires an artifact reference")
		}
		if reason != CaptureNotTruncated {
			return errors.New("untruncated output cannot have a truncation reason")
		}
		return nil
	}
	if err := validateOpaqueArtifactRef(ref, input.CapturedBytes); err != nil {
		return err
	}
	if !input.Truncated {
		return errors.New("artifact output must be truncated")
	}
	if ref.Complete {
		if reason != CaptureTruncatedInline {
			return errors.New("complete artifact requires inline preview truncation")
		}
		return nil
	}
	switch reason {
	case CaptureTruncatedHardLimit, CaptureTruncatedArtifact, CaptureTruncatedWriteFailure, CaptureTruncatedCanceled:
		return nil
	default:
		return errors.New("incomplete artifact has an invalid truncation reason")
	}
}

func (f *ResultFactory) safeErrors(input *Error) (*SafeError, *Error) {
	if input == nil {
		return nil, nil
	}
	message := f.redactor.Redact(input.Message)
	return &SafeError{
			Code:        input.Code,
			Message:     message,
			Recoverable: input.Recoverable,
		}, &Error{
			Code:        input.Code,
			Message:     message.Text(),
			Recoverable: input.Recoverable,
		}
}

type resultModelPayload struct {
	State            ExecutionState    `json:"state"`
	Status           ResultStatus      `json:"status"`
	Summary          string            `json:"summary"`
	Preview          string            `json:"preview,omitempty"`
	Artifact         *artifact.Ref     `json:"artifact,omitempty"`
	Truncated        bool              `json:"truncated,omitempty"`
	TruncationReason string            `json:"truncation_reason,omitempty"`
	Error            *safeErrorPayload `json:"error,omitempty"`
}

type resultPersistedPayload struct {
	State            ExecutionState    `json:"state"`
	Status           ResultStatus      `json:"status"`
	Summary          string            `json:"summary"`
	Artifact         *artifact.Ref     `json:"artifact,omitempty"`
	Truncated        bool              `json:"truncated,omitempty"`
	TruncationReason string            `json:"truncation_reason,omitempty"`
	Error            *safeErrorPayload `json:"error,omitempty"`
}

type safeErrorPayload struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}

func safeErrorPayloadFrom(input *SafeError) *safeErrorPayload {
	if input == nil {
		return nil
	}
	return &safeErrorPayload{
		Code:        input.Code,
		Message:     input.Message.Text(),
		Recoverable: input.Recoverable,
	}
}
