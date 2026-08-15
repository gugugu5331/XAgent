package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/hook"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/sessionctx"
	"xagent/internal/skill"
	"xagent/internal/subagent"
)

func TestResultProjectorAcksOnlyAfterProviderAcceptsRequest(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	owner := subagent.ParentRef{ConversationID: "conversation-1", ExecutionID: "execution-1", RequestGeneration: 7}
	notification := completedResultNotification(redactor, owner, "task-1", 11, "safe summary")
	payload, err := subagent.MarshalResultMessage(notification)
	if err != nil {
		t.Fatal(err)
	}
	service := &recordingResultClaimService{claim: subagent.ResultClaim{
		ClaimID:         "claim-1",
		Owner:           owner,
		SerializedBytes: int64(len(payload)),
		Notifications:   []subagent.ResultNotification{notification},
	}}
	projector := mustResultProjector(t, service, redactor, 100_000)
	base := provider.ChatRequest{
		Model:    "model",
		Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("continue")}},
	}
	stream := &projectionTestStream{events: make(chan provider.StreamEvent)}
	var handedOff provider.ChatRequest
	got, err := projector.StreamChat(context.Background(), owner, base, func(_ context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
		handedOff = request
		if service.ackCount() != 0 {
			t.Fatal("result claim was acknowledged before Provider accepted the request")
		}
		return stream, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != stream || service.ackCount() != 1 || service.releaseCount() != 0 {
		t.Fatalf("handoff lifecycle = stream:%t ack:%d release:%d", got == stream, service.ackCount(), service.releaseCount())
	}
	if len(handedOff.Messages) != 2 {
		t.Fatalf("projected message count = %d, want 2", len(handedOff.Messages))
	}
	result := handedOff.Messages[1]
	if result.Role != provider.ModelMessageRoleSubagentResult || result.Content.Text() != string(payload) ||
		result.ToolCallID != "" || result.ToolName != "" || result.ArgumentsJSON.Text() != "" || result.ToolResult.Text() != "" {
		t.Fatalf("projected result message = %#v", result)
	}
	if err := handedOff.Validate(); err != nil {
		t.Fatalf("projected Provider request is invalid: %v", err)
	}
	if len(base.Messages) != 1 {
		t.Fatal("projection mutated the caller request")
	}
}

func TestResultProjectorReleasesOnConstructionOrProviderStartFailure(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	owner := subagent.ParentRef{ConversationID: "conversation-2", ExecutionID: "execution-2", RequestGeneration: 8}
	notification := completedResultNotification(redactor, owner, "task-2", 12, "safe summary")
	payload, err := subagent.MarshalResultMessage(notification)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("provider start", func(t *testing.T) {
		service := &recordingResultClaimService{claim: subagent.ResultClaim{
			ClaimID: "claim-provider", Owner: owner, SerializedBytes: int64(len(payload)),
			Notifications: []subagent.ResultNotification{notification},
		}}
		projector := mustResultProjector(t, service, redactor, 100_000)
		startErr := errors.New("provider rejected request")
		_, err := projector.StreamChat(context.Background(), owner, provider.ChatRequest{Model: "model"}, func(_ context.Context, _ provider.ChatRequest) (provider.ChatStream, error) {
			return nil, startErr
		})
		if !errors.Is(err, startErr) || service.ackCount() != 0 || service.releaseCount() != 1 {
			t.Fatalf("start failure lifecycle = err:%v ack:%d release:%d", err, service.ackCount(), service.releaseCount())
		}
	})

	t.Run("claim byte mismatch", func(t *testing.T) {
		service := &recordingResultClaimService{claim: subagent.ResultClaim{
			ClaimID: "claim-invalid", Owner: owner, SerializedBytes: int64(len(payload) + 1),
			Notifications: []subagent.ResultNotification{notification},
		}}
		projector := mustResultProjector(t, service, redactor, 100_000)
		started := false
		_, err := projector.StreamChat(context.Background(), owner, provider.ChatRequest{Model: "model"}, func(_ context.Context, _ provider.ChatRequest) (provider.ChatStream, error) {
			started = true
			return &projectionTestStream{events: make(chan provider.StreamEvent)}, nil
		})
		if err == nil || started || service.ackCount() != 0 || service.releaseCount() != 1 {
			t.Fatalf("invalid claim lifecycle = err:%v started:%t ack:%d release:%d", err, started, service.ackCount(), service.releaseCount())
		}
	})
}

func TestResultProjectorRequestCancellationCannotAutoReleaseBeforeAcceptedAck(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	owner := subagent.ParentRef{ConversationID: "conversation-cancel", ExecutionID: "execution-cancel", RequestGeneration: 10}
	notification := completedResultNotification(redactor, owner, "task-cancel", 13, "safe summary")
	payload, err := subagent.MarshalResultMessage(notification)
	if err != nil {
		t.Fatal(err)
	}
	service := &recordingResultClaimService{claim: subagent.ResultClaim{
		ClaimID: "claim-cancel", Owner: owner, SerializedBytes: int64(len(payload)),
		Notifications: []subagent.ResultNotification{notification},
	}}
	projector := mustResultProjector(t, service, redactor, 100_000)
	requestCtx, cancel := context.WithCancel(context.Background())
	stream := &projectionTestStream{events: make(chan provider.StreamEvent)}
	got, err := projector.StreamChat(requestCtx, owner, provider.ChatRequest{Model: "model"}, func(_ context.Context, _ provider.ChatRequest) (provider.ChatStream, error) {
		cancel()
		claimCtx := service.claimContext()
		if claimCtx == nil {
			t.Fatal("claim context was not captured")
		}
		select {
		case <-claimCtx.Done():
			t.Fatal("result lease context ended before the accepted Provider request could be acknowledged")
		case <-time.After(10 * time.Millisecond):
		}
		return stream, nil
	})
	if err != nil || got != stream || service.ackCount() != 1 || service.releaseCount() != 0 {
		t.Fatalf("cancellation race lifecycle = stream:%t err:%v ack:%d release:%d", got == stream, err, service.ackCount(), service.releaseCount())
	}
}

func TestResultProjectorLeavesPendingResultsWhenNoBudgetRemains(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	service := &recordingResultClaimService{}
	base := provider.ChatRequest{
		Model:    "model",
		Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("already full")}},
	}
	measure, err := contextmgr.NewRequestBudgeter().MeasureRequest(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	projector := mustResultProjector(t, service, redactor, measure.PlanningTokens)
	stream := &projectionTestStream{events: make(chan provider.StreamEvent)}
	owner := subagent.ParentRef{ConversationID: "conversation-3", ExecutionID: "execution-3", RequestGeneration: 9}
	got, err := projector.StreamChat(context.Background(), owner, base, func(_ context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
		if len(request.Messages) != len(base.Messages) {
			t.Fatal("no-budget handoff injected a result")
		}
		return stream, nil
	})
	if err != nil || got != stream {
		t.Fatalf("no-budget handoff = (%T,%v)", got, err)
	}
	if service.claimCount() != 0 || service.ackCount() != 0 || service.releaseCount() != 0 {
		t.Fatalf("no-budget lifecycle = claim:%d ack:%d release:%d", service.claimCount(), service.ackCount(), service.releaseCount())
	}
}

func TestOrchestratorProjectsClaimAtItsSingleProviderHandoff(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	owner := subagent.ParentRef{ConversationID: "conversation-integrated", ExecutionID: "execution-integrated", RequestGeneration: 17}
	notification := completedResultNotification(redactor, owner, "task-integrated", 21, "integrated result")
	payload, err := subagent.MarshalResultMessage(notification)
	if err != nil {
		t.Fatal(err)
	}
	service := &recordingResultClaimService{claim: subagent.ResultClaim{
		ClaimID: "claim-integrated", Owner: owner, SerializedBytes: int64(len(payload)),
		Notifications: []subagent.ResultNotification{notification},
	}}
	projector := mustResultProjector(t, service, redactor, 100_000)
	providerRecorder := &projectionRecordingProvider{stream: &projectionTestStream{events: make(chan provider.StreamEvent)}}
	preparer := &projectionPrepareRecorder{}
	orch := NewWithOptions(OrchestratorOptions{
		Provider:        providerRecorder,
		RuntimeRedactor: redactor,
		ResultProjector: projector,
		SessionContext:  preparer,
	})
	conv := conversation.NewConversation(owner.ConversationID, time.Now())
	state := &executionState{
		profile:           skill.ExecutionProfile{Model: "model"},
		ref:               hook.ExecutionRef{ExecutionID: owner.ExecutionID, Kind: hook.ExecutionMain},
		requestGeneration: owner.RequestGeneration,
	}
	stream, err := orch.streamWithExecutionState(context.Background(), conv, RunModeDefault, state.profile, false, 1, state.ref, state)
	if err != nil {
		t.Fatal(err)
	}
	if stream != providerRecorder.stream || service.ackCount() != 1 || service.releaseCount() != 0 {
		t.Fatalf("integrated handoff = stream:%t ack:%d release:%d", stream == providerRecorder.stream, service.ackCount(), service.releaseCount())
	}
	request := providerRecorder.request()
	if len(request.Messages) != 1 || request.Messages[0].Role != provider.ModelMessageRoleSubagentResult || request.Messages[0].Content.Text() != string(payload) {
		t.Fatalf("integrated request messages = %#v", request.Messages)
	}
	if got, want := preparer.reserve(), projector.ReservePlanningTokens(); got != want || got <= 0 {
		t.Fatalf("context result reserve = %d, want %d", got, want)
	}
}

func TestConfigureCandidateStateAlwaysAssignsRequestGeneration(t *testing.T) {
	orch := NewWithOptions(OrchestratorOptions{})
	state := &executionState{}
	if err := orch.configureCandidateState(state, "conversation-generation"); err != nil {
		t.Fatal(err)
	}
	if state.requestGeneration == 0 {
		t.Fatal("main request generation was not assigned without tool-result candidate mode")
	}
}

func mustResultProjector(t *testing.T, service ResultClaimService, redactor *redact.RuntimeRedactor, maxTokens int64) *ResultProjector {
	t.Helper()
	projector, err := NewResultProjector(ResultProjectorOptions{
		Service:                  service,
		Budgeter:                 contextmgr.NewRequestBudgeter(),
		RuntimeRedactor:          redactor,
		MaxRequestPlanningTokens: maxTokens,
		MaxResultBytes:           subagent.DefaultLimits().MaxResultBytes,
		MaxResultsPerClaim:       subagent.DefaultLimits().MaxResultsPerClaim,
	})
	if err != nil {
		t.Fatal(err)
	}
	return projector
}

func completedResultNotification(redactor *redact.RuntimeRedactor, parent subagent.ParentRef, taskID string, revision uint64, summary string) subagent.ResultNotification {
	return subagent.ResultNotification{
		NotificationID:     "notification-" + taskID,
		CompletionRevision: revision,
		CompletionSequence: 4,
		CreatedAt:          time.Unix(1_700_000_000, 0).UTC(),
		TaskID:             subagent.ID(taskID),
		Parent:             parent,
		Status:             subagent.StatusCompleted,
		Summary:            redactor.Redact(summary),
		StopReason:         subagent.StopCompleted,
		Usage:              subagent.Usage{InputTokens: 3, OutputTokens: 5},
	}
}

type recordingResultClaimService struct {
	mu       sync.Mutex
	claim    subagent.ResultClaim
	claimCtx context.Context
	claims   int
	acks     int
	releases int
}

func (service *recordingResultClaimService) ClaimResults(ctx context.Context, options subagent.ResultClaimOptions) (subagent.ResultClaim, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.claimCtx = ctx
	service.claims++
	claim := service.claim.Clone()
	if claim.Owner == (subagent.ParentRef{}) {
		claim.Owner = options.Owner
	}
	return claim, nil
}

func (service *recordingResultClaimService) claimContext() context.Context {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.claimCtx
}

func (service *recordingResultClaimService) AckResults(_ context.Context, _ string, _ subagent.ParentRef) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.acks++
	return nil
}

func (service *recordingResultClaimService) ReleaseResults(_ context.Context, _ string, _ subagent.ParentRef) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.releases++
	return nil
}

func (service *recordingResultClaimService) claimCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.claims
}

func (service *recordingResultClaimService) ackCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.acks
}

func (service *recordingResultClaimService) releaseCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.releases
}

type projectionTestStream struct {
	events chan provider.StreamEvent
}

func (stream *projectionTestStream) Events() <-chan provider.StreamEvent { return stream.events }
func (stream *projectionTestStream) Close(context.Context) error         { return nil }

type projectionRecordingProvider struct {
	mu       sync.Mutex
	captured provider.ChatRequest
	stream   provider.ChatStream
}

type projectionPrepareRecorder struct {
	mu                    sync.Mutex
	reservePlanningTokens int64
}

func (recorder *projectionPrepareRecorder) Prepare(context.Context, *conversation.Conversation, sessionctx.PrepareMode) (sessionctx.PreparedContext, error) {
	return sessionctx.PreparedContext{}, nil
}

func (recorder *projectionPrepareRecorder) PrepareWithOptions(_ context.Context, _ *conversation.Conversation, options contextmgr.PrepareOptions) (sessionctx.PreparedContext, error) {
	recorder.mu.Lock()
	recorder.reservePlanningTokens = options.ReservePlanningTokens
	recorder.mu.Unlock()
	return sessionctx.PreparedContext{}, nil
}

func (recorder *projectionPrepareRecorder) reserve() int64 {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.reservePlanningTokens
}

func (recorder *projectionRecordingProvider) StreamChat(_ context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	recorder.mu.Lock()
	recorder.captured = request
	recorder.mu.Unlock()
	return recorder.stream, nil
}

func (*projectionRecordingProvider) Name() string { return "projection-recorder" }

func (recorder *projectionRecordingProvider) request() provider.ChatRequest {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.captured
}
