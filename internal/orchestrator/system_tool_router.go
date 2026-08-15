package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/permission"
	"xagent/internal/provider"
	"xagent/internal/skill"
	"xagent/internal/subagent"
	"xagent/internal/tool"
)

// SystemToolRouterOptions binds the Agent system route to the one task
// service and safe result boundary owned by Assembly.
type SystemToolRouterOptions struct {
	Service       subagent.Service
	ResultFactory *tool.ResultFactory
	Limits        subagent.Limits
}

// SystemToolRouteRequest contains trusted execution identity alongside the
// already registry-validated model call. RequestGeneration is the current
// executionState generation and must match Parent exactly.
type SystemToolRouteRequest struct {
	Call              tool.ValidatedCall
	Parent            subagent.ParentRef
	Invocation        subagent.InvocationRef
	RequestGeneration uint64
	Depth             int
	ParentRuntime     *ParentRuntimeSnapshot
}

// SystemToolRouter is the sole model Agent -> subagent.Service.Submit edge.
// It does not execute ordinary tools or implement a second task manager.
type SystemToolRouter struct {
	service subagent.Service
	factory *tool.ResultFactory
	limits  subagent.Limits
}

// ParentRuntimeCaptureInput is the explicit ordered-boundary input shared by
// the model route and the future TUI route. Prompt must already be the exact
// budgeted Provider prefix; this method never starts Provider work to rebuild
// one implicitly.
type ParentRuntimeCaptureInput struct {
	Conversation  *conversation.Conversation
	Prompt        provider.PromptPrefixSnapshot
	Registry      *tool.Registry
	Mode          RunMode
	Profile       skill.ExecutionProfile
	ParentContext context.Context
}

func NewSystemToolRouter(options SystemToolRouterOptions) (*SystemToolRouter, error) {
	if options.Service == nil {
		return nil, errors.New("subagent service is unavailable")
	}
	if options.ResultFactory == nil {
		return nil, errors.New("system tool result factory is unavailable")
	}
	if options.Limits.MaxTaskBytes == 0 {
		options.Limits = subagent.DefaultLimits()
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("system tool limits are invalid: %w", err)
	}
	return &SystemToolRouter{service: options.Service, factory: options.ResultFactory, limits: options.Limits}, nil
}

// CaptureParentRuntimeSnapshot freezes the parent DTOs that RunnerFactory
// Prepare consumes. Callers own the Conversation ordered-commit boundary.
func (o *Orchestrator) CaptureParentRuntimeSnapshot(input ParentRuntimeCaptureInput) (ParentRuntimeSnapshot, error) {
	if o == nil || input.Conversation == nil || input.Registry == nil || !input.Registry.IsSealed() {
		return ParentRuntimeSnapshot{}, errors.New("parent runtime capture boundary is unavailable")
	}
	if input.Mode != RunModeDefault && input.Mode != RunModePlan && input.Mode != RunModeDo {
		return ParentRuntimeSnapshot{}, errors.New("parent runtime mode is invalid")
	}
	prompt, err := clonePromptPrefixSnapshot(input.Prompt)
	if err != nil {
		return ParentRuntimeSnapshot{}, err
	}
	registryPrompt, err := provider.CapturePromptPrefix(prompt.BuildChild(nil, nil, toolDefinitionsFromRegistry(input.Registry)))
	if err != nil || registryPrompt.ToolFingerprint != prompt.ToolFingerprint {
		return ParentRuntimeSnapshot{}, errors.New("parent runtime tool snapshot is inconsistent")
	}
	conversationSnapshot, err := conversation.TakeSnapshot(input.Conversation)
	if err != nil {
		return ParentRuntimeSnapshot{}, err
	}
	permissionMode := o.permissionMode
	if permissionMode == "" {
		permissionMode = permission.ModeDefault
	}
	roots := append([]string(nil), input.Profile.ReadRoots...)
	if projectRoot := strings.TrimSpace(o.projectRoot()); projectRoot != "" {
		roots = append(roots, projectRoot)
	}
	sort.Strings(roots)
	uniqueRoots := roots[:0]
	for _, root := range roots {
		if root == "" || (len(uniqueRoots) > 0 && uniqueRoots[len(uniqueRoots)-1] == root) {
			continue
		}
		uniqueRoots = append(uniqueRoots, root)
	}
	return ParentRuntimeSnapshot{
		Model: prompt.Model, PermissionMode: permissionMode, PlanMode: input.Mode == RunModePlan,
		ReadRoots: append([]string(nil), uniqueRoots...), Registry: input.Registry,
		Conversation: conversationSnapshot, Prompt: prompt, ParentContext: input.ParentContext,
	}, nil
}

func (o *Orchestrator) routeAgentSystemTool(
	ctx context.Context,
	conv *conversation.Conversation,
	state *executionState,
	checkpoint conversationCheckpoint,
	profile skill.ExecutionProfile,
	mode RunMode,
	validated tool.ValidatedCall,
) tool.Result {
	call := validated.Call
	if o == nil || o.systemToolRouter == nil || state == nil || conv == nil {
		return o.syntheticToolResult(tool.ResultFactoryInput{
			CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusError,
			Summary: "Subagent system route is unavailable",
			Error:   &tool.Error{Code: string(subagent.ErrInternal), Message: "Subagent system route is unavailable", Recoverable: false},
		})
	}
	parent := subagent.ParentRef{
		ConversationID: conv.ID, ExecutionID: state.ref.ExecutionID, RequestGeneration: state.requestGeneration,
	}
	request := SystemToolRouteRequest{
		Call: validated, Parent: parent, Invocation: subagent.InvocationRef{ToolCallID: call.ID},
		RequestGeneration: state.requestGeneration, Depth: profile.IndependentDepth,
	}
	if request.Depth == 0 {
		prompt, err := state.currentParentPrompt(state.ref)
		if err != nil {
			return o.parentSnapshotFailure(call)
		}
		parentRegistry, err := o.registryForProfile(mode, profile)
		if err != nil || parentRegistry == nil {
			return o.parentSnapshotFailure(call)
		}
		parentConversation, err := conversationAtCheckpoint(conv, checkpoint)
		if err != nil {
			return o.parentSnapshotFailure(call)
		}
		runtime, err := o.CaptureParentRuntimeSnapshot(ParentRuntimeCaptureInput{
			Conversation: parentConversation, Prompt: prompt, Registry: parentRegistry, Mode: mode,
			Profile: profile, ParentContext: ctx,
		})
		if err != nil {
			return o.parentSnapshotFailure(call)
		}
		request.ParentRuntime = &runtime
	}
	return o.systemToolRouter.RouteSystem(ctx, request)
}

func conversationAtCheckpoint(conv *conversation.Conversation, checkpoint conversationCheckpoint) (*conversation.Conversation, error) {
	if conv == nil || checkpoint.messageCount < 0 || checkpoint.messageCount > len(conv.Messages) {
		return nil, errors.New("parent conversation checkpoint is invalid")
	}
	cloned := *conv
	cloned.Messages = conv.Messages[:checkpoint.messageCount]
	cloned.UpdatedAt = checkpoint.updatedAt
	return &cloned, nil
}

func (o *Orchestrator) parentSnapshotFailure(call tool.Call) tool.Result {
	return o.syntheticToolResult(tool.ResultFactoryInput{
		CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusError,
		Summary: "Subagent parent snapshot is unavailable",
		Error: &tool.Error{
			Code: string(subagent.ErrParentSnapshotUnavailable), Message: "Subagent parent snapshot is unavailable", Recoverable: true,
		},
	})
}

// RouteSystem routes a validated Agent call. Recursive and forged identities
// are rejected before Submit, so they cannot reserve an inbox slot, enter the
// queue, or start a Provider request.
func (router *SystemToolRouter) RouteSystem(ctx context.Context, request SystemToolRouteRequest) tool.Result {
	call := request.Call.Call
	if router == nil || router.service == nil || router.factory == nil {
		return routerFailure(router, call, tool.Rejected, subagent.ErrInternal, "subagent system route is unavailable", false)
	}
	if ctx == nil {
		return routerFailure(router, call, tool.Rejected, subagent.ErrInvalidTransition, "subagent routing context is invalid", false)
	}
	if call.Name != tool.AgentToolName || request.Call.Tool == nil || request.Call.Tool.Name() != tool.AgentToolName {
		return routerFailure(router, call, tool.Rejected, subagent.ErrInvalidTask, "system route does not contain a validated Agent call", true)
	}
	if request.Depth > 0 {
		return routerFailure(router, call, tool.Rejected, subagent.ErrRecursiveDelegate, "subagents cannot delegate another Agent task", true)
	}
	if request.Depth < 0 || !router.validModelIdentity(request) {
		return routerFailure(router, call, tool.Rejected, subagent.ErrInvalidParent, "subagent model invocation identity is invalid", true)
	}

	input, err := router.agentInput(request.Call)
	if err != nil {
		return routerFailure(router, call, tool.Rejected, subagent.ErrInvalidTask, "Agent tool input is invalid", true)
	}
	input.Origin = subagent.OriginModel
	input.Parent = request.Parent
	input.Invocation = request.Invocation

	submitCtx := ctx
	if request.ParentRuntime != nil {
		runtime := *request.ParentRuntime
		if runtime.ParentContext == nil {
			runtime.ParentContext = ctx
		}
		submitCtx, err = WithParentRuntimeSnapshot(ctx, request.Parent, runtime)
		if err != nil {
			return routerFailure(router, call, tool.Rejected, subagent.ErrParentSnapshotUnavailable, "subagent parent runtime snapshot is invalid", true)
		}
	}

	submission, err := router.service.Submit(submitCtx, input)
	if err != nil {
		return routerErrorResult(router, call, tool.Rejected, err)
	}
	if !router.validSubmission(submission, input) {
		return routerFailure(router, call, tool.Completed, subagent.ErrInternal, "subagent service returned an invalid submission", false)
	}
	if submission.Placement == subagent.Background {
		return router.acceptedResult(call, submission)
	}

	outcome, err := router.service.AwaitForeground(ctx, submission.ID)
	if err != nil {
		return routerErrorResult(router, call, tool.CancelledAfterStart, err)
	}
	if outcome.Detached {
		if outcome.Completion != nil {
			return routerFailure(router, call, tool.Completed, subagent.ErrInternal, "subagent foreground outcome is inconsistent", false)
		}
		detached := submission
		detached.Placement = subagent.Background
		return router.acceptedResult(call, detached)
	}
	if outcome.Completion == nil {
		return routerFailure(router, call, tool.Completed, subagent.ErrInternal, "subagent foreground completion is unavailable", false)
	}
	return router.completionResult(call, submission, *outcome.Completion)
}

func (router *SystemToolRouter) validModelIdentity(request SystemToolRouteRequest) bool {
	maxIDBytes := router.limits.MaxIDBytes
	return request.RequestGeneration > 0 && request.Parent.RequestGeneration == request.RequestGeneration &&
		validSystemIdentifier(request.Parent.ConversationID, maxIDBytes) &&
		validSystemIdentifier(request.Parent.ExecutionID, maxIDBytes) &&
		validSystemIdentifier(request.Invocation.ToolCallID, maxIDBytes) &&
		request.Invocation.ToolCallID == request.Call.Call.ID
}

func (router *SystemToolRouter) agentInput(validated tool.ValidatedCall) (subagent.SubmitInput, error) {
	canonical, err := json.Marshal(validated.Arguments)
	if err != nil || !bytes.Equal(canonical, validated.CanonicalArguments()) {
		return subagent.SubmitInput{}, errors.New("Agent validation snapshot is inconsistent")
	}
	if len(validated.Arguments) < 2 || len(validated.Arguments) > 4 {
		return subagent.SubmitInput{}, errors.New("Agent field count is invalid")
	}
	for name := range validated.Arguments {
		switch name {
		case "task", "type", "role", "placement":
		default:
			return subagent.SubmitInput{}, errors.New("Agent field is invalid")
		}
	}
	task, ok := validated.Arguments["task"].(string)
	if !ok {
		return subagent.SubmitInput{}, errors.New("Agent task is invalid")
	}
	task = strings.TrimSpace(task)
	maxTaskBytes := router.limits.MaxTaskBytes
	if maxTaskBytes > tool.AgentToolMaxTaskBytes {
		maxTaskBytes = tool.AgentToolMaxTaskBytes
	}
	if task == "" || !utf8.ValidString(task) || strings.ContainsRune(task, '\x00') || int64(len(task)) > maxTaskBytes {
		return subagent.SubmitInput{}, errors.New("Agent task is invalid")
	}
	typeText, ok := validated.Arguments["type"].(string)
	if !ok {
		return subagent.SubmitInput{}, errors.New("Agent type is invalid")
	}
	taskType := subagent.ExecutionType(typeText)
	if !taskType.Valid() {
		return subagent.SubmitInput{}, errors.New("Agent type is invalid")
	}

	role := ""
	if value, exists := validated.Arguments["role"]; exists {
		var roleOK bool
		role, roleOK = value.(string)
		role = strings.ToLower(strings.TrimSpace(role))
		if !roleOK || !validOptionalSystemText(role, router.limits.MaxRoleNameBytes) {
			return subagent.SubmitInput{}, errors.New("Agent role is invalid")
		}
	}
	placement := subagent.PlacementDefault
	if value, exists := validated.Arguments["placement"]; exists {
		placementText, placementOK := value.(string)
		if !placementOK {
			return subagent.SubmitInput{}, errors.New("Agent placement is invalid")
		}
		placement = subagent.PlacementIntent(placementText)
	}
	if !placement.Valid() {
		return subagent.SubmitInput{}, errors.New("Agent placement is invalid")
	}
	return subagent.SubmitInput{Task: task, Type: taskType, Role: role, Placement: placement}, nil
}

func (router *SystemToolRouter) validSubmission(submission subagent.Submission, input subagent.SubmitInput) bool {
	return validSystemIdentifier(string(submission.ID), router.limits.MaxIDBytes) &&
		submission.Type == input.Type && submission.Role == input.Role && submission.Origin == subagent.OriginModel &&
		submission.Parent == input.Parent && submission.Placement.Valid() && submission.Status == subagent.StatusQueued &&
		submission.Revision > 0 && !submission.CreatedAt.IsZero()
}

func (router *SystemToolRouter) acceptedResult(call tool.Call, submission subagent.Submission) tool.Result {
	payload, err := json.Marshal(struct {
		TaskID    subagent.ID            `json:"task_id"`
		Type      subagent.ExecutionType `json:"type"`
		Placement subagent.Placement     `json:"placement"`
		Status    string                 `json:"status"`
	}{TaskID: submission.ID, Type: submission.Type, Placement: submission.Placement, Status: "accepted"})
	if err != nil || int64(len(payload)) > router.limits.MaxResultBytes {
		return routerFailure(router, call, tool.Completed, subagent.ErrInternal, "subagent acceptance result is unavailable", false)
	}
	return routerBuild(router, tool.ResultFactoryInput{
		CallID: call.ID, Name: call.Name, State: tool.Completed, Status: tool.StatusSuccess,
		Summary: "Subagent task accepted", Preview: string(payload),
	})
}

func (router *SystemToolRouter) completionResult(call tool.Call, submission subagent.Submission, completion subagent.Completion) tool.Result {
	if err := completion.Validate(); err != nil || completion.ID != submission.ID ||
		!validSystemSafeText(completion.Summary.Text(), router.limits.MaxResultBytes) ||
		!validSystemSafeText(completion.TruncationReason.Text(), router.limits.MaxResultBytes) ||
		(completion.Error != nil && !validSystemSafeText(completion.Error.Message.Text(), router.limits.MaxResultBytes)) {
		return routerFailure(router, call, tool.Completed, subagent.ErrInternal, "subagent foreground completion is invalid", false)
	}
	input := tool.ResultFactoryInput{
		CallID: call.ID, Name: call.Name, State: tool.Completed,
		Summary: completion.Summary.Text(), Preview: completion.Summary.Text(), Status: tool.StatusSuccess,
	}
	if completion.Status != subagent.StatusCompleted {
		input.Status = tool.StatusError
		if completion.Status == subagent.StatusCancelled {
			input.State = tool.CancelledAfterStart
		}
		if completion.Status == subagent.StatusTimedOut {
			input.Status = tool.StatusTimeout
			input.State = tool.CancelledAfterStart
		}
		input.Error = &tool.Error{
			Code: string(subagent.ErrInternal), Message: "subagent task failed", Recoverable: false,
		}
		if completion.Error != nil && subagent.ErrorCode(completion.Error.Code).Valid() && completion.Error.Source == "subagent" {
			input.Error = &tool.Error{
				Code: completion.Error.Code, Message: completion.Error.Message.Text(), Recoverable: completion.Error.Recoverable,
			}
		}
	}
	return routerBuild(router, input)
}

func routerErrorResult(router *SystemToolRouter, call tool.Call, state tool.ExecutionState, err error) tool.Result {
	if errors.Is(err, context.Canceled) {
		return routerFailure(router, call, state, subagent.ErrCancelled, "subagent operation was cancelled", true)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return routerFailure(router, call, state, subagent.ErrTimedOut, "subagent operation timed out", true)
	}
	var safe *diagnostics.SafeError
	if errors.As(err, &safe) && safe != nil && safe.Source == "subagent" && subagent.ErrorCode(safe.Code).Valid() &&
		router != nil && validSystemSafeText(safe.Message.Text(), router.limits.MaxResultBytes) {
		return routerBuild(router, tool.ResultFactoryInput{
			CallID: call.ID, Name: call.Name, State: state, Status: tool.StatusError,
			Summary: safe.Message.Text(), Preview: safe.Message.Text(),
			Error: &tool.Error{Code: safe.Code, Message: safe.Message.Text(), Recoverable: safe.Recoverable},
		})
	}
	return routerFailure(router, call, state, subagent.ErrInternal, "subagent operation failed internally", false)
}

func routerFailure(router *SystemToolRouter, call tool.Call, state tool.ExecutionState, code subagent.ErrorCode, message string, recoverable bool) tool.Result {
	return routerBuild(router, tool.ResultFactoryInput{
		CallID: call.ID, Name: call.Name, State: state, Status: tool.StatusError,
		Summary: message, Preview: message,
		Error: &tool.Error{Code: string(code), Message: message, Recoverable: recoverable},
	})
}

func routerBuild(router *SystemToolRouter, input tool.ResultFactoryInput) tool.Result {
	if router == nil || router.factory == nil {
		return tool.Result{}
	}
	result, err := router.factory.Build(input)
	if err != nil {
		return tool.Result{}
	}
	return result
}

func validSystemIdentifier(value string, maxBytes int64) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) || int64(len(value)) > maxBytes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validOptionalSystemText(value string, maxBytes int64) bool {
	if value == "" || !utf8.ValidString(value) || int64(len(value)) > maxBytes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func validSystemSafeText(value string, maxBytes int64) bool {
	return utf8.ValidString(value) && int64(len(value)) <= maxBytes && !strings.ContainsRune(value, '\x00')
}

type parentRuntimeSnapshotContextKey struct{}

type parentRuntimeSnapshotEnvelope struct {
	parent   subagent.ParentRef
	snapshot ParentRuntimeSnapshot
}

// WithParentRuntimeSnapshot binds one immutable parent snapshot to the
// synchronous Submit -> RunnerFactory.Prepare call chain. It is exported so
// the TUI/App route can use the same submit boundary without a global cache.
func WithParentRuntimeSnapshot(ctx context.Context, parent subagent.ParentRef, snapshot ParentRuntimeSnapshot) (context.Context, error) {
	modelParent := parent.ExecutionID != "" && validSystemIdentifier(parent.ExecutionID, 1<<10) && parent.RequestGeneration > 0
	tuiParent := parent.ExecutionID == "" && parent.RequestGeneration == 0
	if ctx == nil || !validSystemIdentifier(parent.ConversationID, 1<<10) || (!modelParent && !tuiParent) {
		return nil, errors.New("parent runtime identity is invalid")
	}
	cloned, err := cloneParentRuntimeSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, parentRuntimeSnapshotContextKey{}, parentRuntimeSnapshotEnvelope{parent: parent, snapshot: cloned}), nil
}

// ParentRuntimeSnapshotFromSubmitContext is the Assembly-ready
// ParentRuntimeSnapshotter implementation paired with
// WithParentRuntimeSnapshot.
func ParentRuntimeSnapshotFromSubmitContext(ctx context.Context, parent subagent.ParentRef) (ParentRuntimeSnapshot, error) {
	if ctx == nil {
		return ParentRuntimeSnapshot{}, errors.New("parent runtime context is unavailable")
	}
	envelope, ok := ctx.Value(parentRuntimeSnapshotContextKey{}).(parentRuntimeSnapshotEnvelope)
	if !ok || envelope.parent != parent {
		return ParentRuntimeSnapshot{}, errors.New("parent runtime snapshot is unavailable")
	}
	return cloneParentRuntimeSnapshot(envelope.snapshot)
}

func cloneParentRuntimeSnapshot(snapshot ParentRuntimeSnapshot) (ParentRuntimeSnapshot, error) {
	if snapshot.Registry == nil || !snapshot.Registry.IsSealed() {
		return ParentRuntimeSnapshot{}, errors.New("parent runtime registry is unavailable or unsealed")
	}
	if _, ok := permission.ParseMode(string(snapshot.PermissionMode)); !ok {
		return ParentRuntimeSnapshot{}, errors.New("parent runtime permission mode is invalid")
	}
	cloned := snapshot
	cloned.ReadRoots = append([]string(nil), snapshot.ReadRoots...)
	if snapshot.Prompt.Fingerprint != "" {
		prompt, err := clonePromptPrefixSnapshot(snapshot.Prompt)
		if err != nil {
			return ParentRuntimeSnapshot{}, err
		}
		registryPrompt, err := provider.CapturePromptPrefix(prompt.BuildChild(nil, nil, toolDefinitionsFromRegistry(snapshot.Registry)))
		if err != nil || registryPrompt.ToolFingerprint != prompt.ToolFingerprint {
			return ParentRuntimeSnapshot{}, errors.New("parent runtime tool snapshot is inconsistent")
		}
		if snapshot.Model != "" && snapshot.Model != prompt.Model {
			return ParentRuntimeSnapshot{}, errors.New("parent runtime model snapshot is inconsistent")
		}
		cloned.Model = prompt.Model
		cloned.Prompt = prompt
	} else if !reflect.DeepEqual(snapshot.Prompt, provider.PromptPrefixSnapshot{}) {
		return ParentRuntimeSnapshot{}, errors.New("parent runtime prompt snapshot is incomplete")
	}
	// ConversationSnapshot has private fields and immutable-by-API behavior;
	// copying the value cannot expose or mutate its backing storage.
	return cloned, nil
}

func clonePromptPrefixSnapshot(snapshot provider.PromptPrefixSnapshot) (provider.PromptPrefixSnapshot, error) {
	if err := snapshot.Validate(); err != nil {
		return provider.PromptPrefixSnapshot{}, err
	}
	return provider.CapturePromptPrefix(snapshot.BuildChild(nil, nil, snapshot.Tools))
}
