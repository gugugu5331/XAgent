package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"

	"xagent/internal/contextmgr"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/subagent"
)

// ResultClaimService is the narrow process-local capability required at the
// Provider request boundary. subagent.Service implements this interface; the
// projector cannot submit, inspect, cancel, or otherwise mutate tasks.
type ResultClaimService interface {
	ClaimResults(context.Context, subagent.ResultClaimOptions) (subagent.ResultClaim, error)
	AckResults(context.Context, string, subagent.ParentRef) error
	ReleaseResults(context.Context, string, subagent.ParentRef) error
}

type ResultProjectorOptions struct {
	Service                  ResultClaimService
	Budgeter                 contextmgr.RequestBudgeter
	RuntimeRedactor          *redact.RuntimeRedactor
	MaxRequestPlanningTokens int64
	MaxResultBytes           int64
	MaxResultsPerClaim       int
}

// ResultProjector owns the claim lease from request construction through the
// Provider acceptance boundary. It is immutable and safe for concurrent main
// Agent requests; ResultInbox serializes leases per conversation.
type ResultProjector struct {
	service                  ResultClaimService
	budgeter                 contextmgr.RequestBudgeter
	redactor                 *redact.RuntimeRedactor
	maxRequestPlanningTokens int64
	maxResultBytes           int64
	maxResultsPerClaim       int
	maxClaimBytes            int64
}

func NewResultProjector(options ResultProjectorOptions) (*ResultProjector, error) {
	if options.Service == nil || options.RuntimeRedactor == nil {
		return nil, errors.New("subagent result projector dependencies are unavailable")
	}
	if err := options.Budgeter.Validate(); err != nil {
		return nil, fmt.Errorf("subagent result projector budgeter is invalid: %w", err)
	}
	if options.MaxRequestPlanningTokens <= 0 || options.MaxRequestPlanningTokens > 10_000_000 ||
		options.MaxResultBytes <= 0 || options.MaxResultBytes > 1<<20 ||
		options.MaxResultsPerClaim <= 0 || options.MaxResultsPerClaim > 1024 {
		return nil, errors.New("subagent result projector limits are invalid")
	}
	if options.MaxResultBytes > math.MaxInt64/int64(options.MaxResultsPerClaim) {
		return nil, errors.New("subagent result projector claim capacity overflows")
	}
	return &ResultProjector{
		service:                  options.Service,
		budgeter:                 options.Budgeter,
		redactor:                 options.RuntimeRedactor,
		maxRequestPlanningTokens: options.MaxRequestPlanningTokens,
		maxResultBytes:           options.MaxResultBytes,
		maxResultsPerClaim:       options.MaxResultsPerClaim,
		maxClaimBytes:            options.MaxResultBytes * int64(options.MaxResultsPerClaim),
	}, nil
}

// ReservePlanningTokens is the conservative context-space reservation for at
// least one maximum-sized fixed-schema result message, including framing.
func (projector *ResultProjector) ReservePlanningTokens() int64 {
	if projector == nil || projector.maxResultBytes <= 0 {
		return 0
	}
	tokens, err := subagent.ResultPlanningReserveTokens(projector.maxResultBytes)
	if err != nil {
		return 0
	}
	return tokens
}

// StreamChat appends the oldest contiguous result prefix and performs the
// Provider handoff. A lease is acknowledged only after start returns a live
// stream. Every earlier failure releases it so the next main request can retry.
func (projector *ResultProjector) StreamChat(
	ctx context.Context,
	owner subagent.ParentRef,
	request provider.ChatRequest,
	start func(context.Context, provider.ChatRequest) (provider.ChatStream, error),
) (provider.ChatStream, error) {
	if projector == nil || projector.service == nil || projector.redactor == nil || start == nil {
		return nil, errors.New("subagent result projection is unavailable")
	}
	if ctx == nil {
		return nil, errors.New("subagent result projection context is nil")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if !validResultProjectionOwner(owner) {
		return nil, errors.New("subagent result projection owner is invalid")
	}

	// ResultInbox automatically releases when its claim context ends. Keep that
	// context transaction-owned until Ack/Release is linearized; otherwise a
	// request cancellation racing a successful Provider start could requeue a
	// result the model has already seen.
	claimCtx, cancelClaim := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelClaim()
	projected, claim, err := projector.prepare(ctx, claimCtx, owner, request)
	if err != nil {
		return nil, err
	}
	stream, startErr := start(ctx, projected)
	if startErr != nil || stream == nil {
		if startErr == nil {
			startErr = errors.New("Provider accepted a subagent result request without a stream")
		}
		if claim.ClaimID != "" {
			startErr = errors.Join(startErr, projector.release(ctx, claim))
		}
		return nil, startErr
	}
	if claim.ClaimID == "" {
		return stream, nil
	}
	if err := projector.service.AckResults(projectionCleanupContext(ctx), claim.ClaimID, claim.Owner); err != nil {
		_ = closeProviderStream(stream)
		return nil, fmt.Errorf("acknowledge subagent result claim after Provider acceptance: %w", err)
	}
	return stream, nil
}

func (projector *ResultProjector) prepare(ctx, claimCtx context.Context, owner subagent.ParentRef, request provider.ChatRequest) (provider.ChatRequest, subagent.ResultClaim, error) {
	baseMeasure, err := projector.budgeter.MeasureRequest(ctx, request)
	if err != nil {
		return provider.ChatRequest{}, subagent.ResultClaim{}, fmt.Errorf("measure Provider request before subagent result projection: %w", err)
	}
	if baseMeasure.PlanningTokens > projector.maxRequestPlanningTokens {
		return provider.ChatRequest{}, subagent.ResultClaim{}, errors.New("Provider request exceeds the configured planning budget")
	}
	remainingTokens := projector.maxRequestPlanningTokens - baseMeasure.PlanningTokens
	if remainingTokens == 0 {
		return request, subagent.ResultClaim{}, nil
	}
	if remainingTokens > math.MaxInt64/4 {
		return provider.ChatRequest{}, subagent.ResultClaim{}, errors.New("subagent result projection budget overflows")
	}
	availableBytes := remainingTokens * 4
	overhead, overflow := checkedResultProjectionMultiply(subagent.ResultProjectionMessageOverheadBytes, int64(projector.maxResultsPerClaim))
	if overflow || availableBytes <= overhead {
		return request, subagent.ResultClaim{}, nil
	}
	claimBytes := availableBytes - overhead
	if claimBytes > projector.maxClaimBytes {
		claimBytes = projector.maxClaimBytes
	}
	if claimBytes <= 0 {
		return request, subagent.ResultClaim{}, nil
	}

	claim, err := projector.service.ClaimResults(claimCtx, subagent.ResultClaimOptions{
		Owner:            owner,
		MaxNotifications: projector.maxResultsPerClaim,
		MaxBytes:         claimBytes,
	})
	if err != nil {
		return provider.ChatRequest{}, subagent.ResultClaim{}, err
	}
	if len(claim.Notifications) == 0 {
		if claim.ClaimID != "" || claim.SerializedBytes != 0 || claim.Owner != owner {
			return provider.ChatRequest{}, subagent.ResultClaim{}, errors.New("empty subagent result claim is invalid")
		}
		return request, subagent.ResultClaim{}, nil
	}
	fail := func(cause error) (provider.ChatRequest, subagent.ResultClaim, error) {
		return provider.ChatRequest{}, subagent.ResultClaim{}, errors.Join(cause, projector.release(ctx, claim))
	}
	if claim.ClaimID == "" || claim.Owner != owner || claim.SerializedBytes <= 0 || claim.SerializedBytes > claimBytes ||
		len(claim.Notifications) > projector.maxResultsPerClaim {
		return fail(errors.New("subagent result claim metadata is invalid"))
	}

	messages := make([]provider.ModelMessage, 0, len(claim.Notifications))
	var serializedBytes int64
	var previous *subagent.ResultNotification
	for index := range claim.Notifications {
		notification := claim.Notifications[index]
		if notification.Parent.ConversationID != owner.ConversationID || !resultNotificationOrderedAfter(previous, notification) {
			return fail(errors.New("subagent result claim order or owner is invalid"))
		}
		payload, marshalErr := subagent.MarshalResultMessage(notification)
		if marshalErr != nil {
			return fail(marshalErr)
		}
		if int64(len(payload)) > projector.maxResultBytes || int64(len(payload)) > math.MaxInt64-serializedBytes {
			return fail(errors.New("subagent result projection exceeds its byte limit"))
		}
		serializedBytes += int64(len(payload))
		safe := projector.redactor.Redact(string(payload))
		if safe.Text() != string(payload) {
			return fail(errors.New("subagent result projection did not cross the safe-text boundary unchanged"))
		}
		messages = append(messages, provider.ModelMessage{Role: provider.ModelMessageRoleSubagentResult, Content: safe})
		copy := notification
		previous = &copy
	}
	if serializedBytes != claim.SerializedBytes {
		return fail(errors.New("subagent result claim serialized byte count is inconsistent"))
	}

	projected := request
	projected.Messages = append(append([]provider.ModelMessage(nil), request.Messages...), messages...)
	if err := projected.Validate(); err != nil {
		return fail(fmt.Errorf("validate Provider request with subagent results: %w", err))
	}
	finalMeasure, err := projector.budgeter.MeasureRequest(ctx, projected)
	if err != nil {
		return fail(fmt.Errorf("measure Provider request with subagent results: %w", err))
	}
	if finalMeasure.PlanningTokens > projector.maxRequestPlanningTokens {
		return fail(errors.New("subagent result projection exceeds the Provider request budget"))
	}
	return projected, claim.Clone(), nil
}

func (projector *ResultProjector) release(ctx context.Context, claim subagent.ResultClaim) error {
	if claim.ClaimID == "" {
		return nil
	}
	if err := projector.service.ReleaseResults(projectionCleanupContext(ctx), claim.ClaimID, claim.Owner); err != nil {
		return fmt.Errorf("release subagent result claim: %w", err)
	}
	return nil
}

func projectionCleanupContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func validResultProjectionOwner(owner subagent.ParentRef) bool {
	if strings.TrimSpace(owner.ConversationID) != owner.ConversationID || owner.ConversationID == "" ||
		strings.TrimSpace(owner.ExecutionID) != owner.ExecutionID || owner.ExecutionID == "" || owner.RequestGeneration == 0 {
		return false
	}
	for _, value := range []string{owner.ConversationID, owner.ExecutionID} {
		for _, character := range value {
			if unicode.IsControl(character) {
				return false
			}
		}
	}
	return true
}

func resultNotificationOrderedAfter(previous *subagent.ResultNotification, current subagent.ResultNotification) bool {
	if previous == nil {
		return true
	}
	if current.CompletionRevision != previous.CompletionRevision {
		return current.CompletionRevision > previous.CompletionRevision
	}
	return string(current.TaskID) > string(previous.TaskID)
}

func checkedResultProjectionMultiply(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || (left != 0 && right > math.MaxInt64/left) {
		return 0, true
	}
	return left * right, false
}
