package subagent

import (
	"strings"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
)

func TestTypeEnumsExposeOnlyDocumentedValues(t *testing.T) {
	for _, value := range []ExecutionType{TypeDefined, TypeFork} {
		if !value.Valid() {
			t.Errorf("documented execution type %q is invalid", value)
		}
	}
	if ExecutionType("other").Valid() {
		t.Fatal("unknown execution type is valid")
	}
	for _, value := range []PlacementIntent{PlacementDefault, PlacementForeground, PlacementBackground} {
		if !value.Valid() {
			t.Errorf("documented placement intent %q is invalid", value)
		}
	}
	if PlacementIntent("other").Valid() {
		t.Fatal("unknown placement intent is valid")
	}
	for _, value := range []Placement{Foreground, Background} {
		if !value.Valid() {
			t.Errorf("documented placement %q is invalid", value)
		}
	}
	if Placement("other").Valid() {
		t.Fatal("unknown placement is valid")
	}
	for _, value := range []Origin{OriginModel, OriginTUI} {
		if !value.Valid() {
			t.Errorf("documented origin %q is invalid", value)
		}
	}
	if Origin("other").Valid() {
		t.Fatal("unknown origin is valid")
	}

	input := SubmitInput{
		Task:      "inspect the repository",
		Type:      TypeDefined,
		Role:      "explorer",
		Placement: PlacementForeground,
		Origin:    OriginModel,
		Parent: ParentRef{
			ConversationID:    "conversation",
			ExecutionID:       "execution",
			RequestGeneration: 7,
		},
		Invocation: InvocationRef{ToolCallID: "call"},
	}
	if input.Parent.RequestGeneration != 7 || input.Invocation.ToolCallID != "call" {
		t.Fatalf("submit contract lost trusted invocation identity: %#v", input)
	}
}

func TestTransitionMatrixMatchesTaskLifecycle(t *testing.T) {
	statuses := []Status{
		StatusQueued,
		StatusRunning,
		StatusWaitingConfirmation,
		StatusSettling,
		StatusCompleted,
		StatusFailed,
		StatusCancelled,
		StatusTimedOut,
		StatusLimitReached,
	}
	allowed := map[[2]Status]bool{
		{StatusQueued, StatusRunning}:                   true,
		{StatusQueued, StatusCancelled}:                 true,
		{StatusQueued, StatusSettling}:                  true,
		{StatusRunning, StatusWaitingConfirmation}:      true,
		{StatusRunning, StatusSettling}:                 true,
		{StatusRunning, StatusCompleted}:                true,
		{StatusRunning, StatusFailed}:                   true,
		{StatusRunning, StatusCancelled}:                true,
		{StatusRunning, StatusTimedOut}:                 true,
		{StatusRunning, StatusLimitReached}:             true,
		{StatusWaitingConfirmation, StatusRunning}:      true,
		{StatusWaitingConfirmation, StatusSettling}:     true,
		{StatusWaitingConfirmation, StatusFailed}:       true,
		{StatusWaitingConfirmation, StatusCancelled}:    true,
		{StatusWaitingConfirmation, StatusTimedOut}:     true,
		{StatusWaitingConfirmation, StatusLimitReached}: true,
		{StatusSettling, StatusCompleted}:               true,
		{StatusSettling, StatusFailed}:                  true,
		{StatusSettling, StatusCancelled}:               true,
		{StatusSettling, StatusTimedOut}:                true,
		{StatusSettling, StatusLimitReached}:            true,
	}
	for _, from := range statuses {
		for _, to := range statuses {
			want := allowed[[2]Status{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%q, %q)=%t, want %t", from, to, got, want)
			}
			err := ValidateTransition(from, to)
			if (err == nil) != want {
				t.Errorf("ValidateTransition(%q, %q) error=%v, want allowed=%t", from, to, err, want)
			}
		}
	}
	if CanTransition(Status("unknown"), StatusRunning) || ValidateTransition(StatusQueued, Status("unknown")) == nil {
		t.Fatal("unknown status participated in a transition")
	}
	for _, status := range statuses {
		wantTerminal := status == StatusCompleted || status == StatusFailed || status == StatusCancelled || status == StatusTimedOut || status == StatusLimitReached
		if got := IsTerminal(status); got != wantTerminal {
			t.Errorf("IsTerminal(%q)=%t, want %t", status, got, wantTerminal)
		}
	}
}

func TestWorkspaceSummaryValidatesIsolationBoundary(t *testing.T) {
	workspaceID := strings.Repeat("a", 32)
	baseOID := strings.Repeat("b", 40)
	branch := "xagent/worktree/" + workspaceID
	valid := []WorkspaceSummary{
		{},
		{
			Isolation:   "worktree",
			WorkspaceID: workspaceID,
			BaseOID:     baseOID,
			Branch:      branch,
			State:       "deleted",
			Cleanup:     "deleted",
		},
		{
			Isolation:      "worktree",
			WorkspaceID:    workspaceID,
			BaseOID:        strings.Repeat("c", 64),
			Branch:         branch,
			State:          "retained",
			Cleanup:        "retained",
			RetentionCause: "dirty_worktree",
			Dirty:          true,
		},
	}
	for _, summary := range valid {
		if err := summary.Validate(); err != nil {
			t.Errorf("valid workspace summary rejected: %v", err)
		}
	}

	invalid := []WorkspaceSummary{
		{Isolation: "shared"},
		{Isolation: "worktree", BaseOID: baseOID, Branch: branch, State: "deleted", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, Branch: branch, State: "deleted", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: baseOID, State: "deleted", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: baseOID, Branch: branch, Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: baseOID, Branch: branch, State: "deleted"},
		{Isolation: "worktree", WorkspaceID: "/Users/private/repo", BaseOID: baseOID, Branch: branch, State: "deleted", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: strings.Repeat("A", 32), BaseOID: baseOID, Branch: branch, State: "deleted", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: "/Users/private/repo", Branch: branch, State: "deleted", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: strings.Repeat("g", 40), Branch: branch, State: "deleted", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: baseOID, Branch: "/Users/private/repo", State: "deleted", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: baseOID, Branch: "xagent/worktree/" + strings.Repeat("c", 32), State: "deleted", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: baseOID, Branch: branch, State: "active", Cleanup: "deleted"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: baseOID, Branch: branch, State: "deleted", Cleanup: "future"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: baseOID, Branch: branch, State: "retained", Cleanup: "retained", RetentionCause: "/Users/private/repo"},
		{Isolation: "worktree", WorkspaceID: workspaceID, BaseOID: baseOID, Branch: branch, State: "retained", Cleanup: "retained", RetentionCause: strings.Repeat("a", 65)},
	}
	for _, summary := range invalid {
		if err := summary.Validate(); err == nil {
			t.Fatalf("invalid workspace summary accepted: %#v", summary)
		}
	}
}

func TestRunResultAndWorkspaceSummaryCloneDetachErrors(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	run := RunResult{Status: StatusFailed, Error: SafeError(ErrInternal, redactor.Redact("run failed"), false)}
	runClone := run.Clone()
	run.Error.Code = "mutated"
	if runClone.Error == run.Error || runClone.Error.Code != string(ErrInternal) {
		t.Fatalf("run result clone aliased error: %#v", runClone)
	}

	workspace := WorkspaceSummary{
		Isolation: "worktree", WorkspaceID: "workspace-1", BaseOID: "oid", Branch: "branch",
		State: "partial", Cleanup: "partial", Error: SafeError(ErrInternal, redactor.Redact("settle failed"), false),
	}
	workspaceClone := workspace.Clone()
	workspace.Error.Code = "mutated"
	if workspaceClone.Error == workspace.Error || workspaceClone.Error.Code != string(ErrInternal) {
		t.Fatalf("workspace summary clone aliased error: %#v", workspaceClone)
	}
}

func TestResultNotificationCloneDetachesWorkspaceSummary(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	notification := ResultNotification{Workspace: WorkspaceSummary{
		Error: SafeError(ErrInternal, redactor.Redact("settlement failed"), false),
	}}
	cloned := notification.Clone()
	notification.Workspace.Error.Code = "mutated"
	if cloned.Workspace.Error == notification.Workspace.Error || cloned.Workspace.Error.Code != string(ErrInternal) {
		t.Fatalf("result notification clone aliased workspace: %#v", cloned)
	}
}

func TestTypeCompletionValidatesTerminalStatusReasonAndErrorCombination(t *testing.T) {
	endedAt := time.Date(2026, time.August, 15, 8, 0, 0, 0, time.UTC)
	redactor := redact.NewRuntimeRedactor()
	valid := []Completion{
		{ID: "completed", Status: StatusCompleted, StopReason: StopCompleted, Summary: redactor.Redact("done"), EndedAt: endedAt},
		{ID: "iteration-limit", Status: StatusLimitReached, StopReason: StopMaxIterations, Summary: redactor.Redact("limited"), Error: SafeError(ErrLimitReached, redactor.Redact("limit"), true), EndedAt: endedAt},
		{ID: "unknown-tool-limit", Status: StatusLimitReached, StopReason: StopUnknownToolLimit, Summary: redactor.Redact("limited"), Error: SafeError(ErrLimitReached, redactor.Redact("limit"), true), EndedAt: endedAt},
		{ID: "cancelled", Status: StatusCancelled, StopReason: StopCancelled, Summary: redactor.Redact("cancelled"), Error: SafeError(ErrCancelled, redactor.Redact("cancelled"), true), EndedAt: endedAt},
		{ID: "closed", Status: StatusCancelled, StopReason: StopApplicationClosed, Summary: redactor.Redact("closed"), Error: SafeError(ErrCancelled, redactor.Redact("closed"), true), EndedAt: endedAt},
		{ID: "timeout", Status: StatusTimedOut, StopReason: StopTaskTimeout, Summary: redactor.Redact("timeout"), Error: SafeError(ErrTimedOut, redactor.Redact("timeout"), true), EndedAt: endedAt},
		{ID: "provider", Status: StatusFailed, StopReason: StopProviderError, Summary: redactor.Redact("provider"), Error: SafeError(ErrProviderFailed, redactor.Redact("provider"), true), EndedAt: endedAt},
		{ID: "tool", Status: StatusFailed, StopReason: StopToolError, Summary: redactor.Redact("tool"), Error: SafeError(ErrToolFailed, redactor.Redact("tool"), true), EndedAt: endedAt},
		{ID: "internal", Status: StatusFailed, StopReason: StopInternalError, Summary: redactor.Redact("internal"), Error: SafeError(ErrInternal, redactor.Redact("internal"), false), EndedAt: endedAt},
	}
	for _, completion := range valid {
		if err := completion.Validate(); err != nil {
			t.Errorf("valid completion %q rejected: %v", completion.ID, err)
		}
	}

	invalid := []struct {
		name       string
		completion Completion
	}{
		{name: "missing ID", completion: Completion{Status: StatusCompleted, StopReason: StopCompleted, EndedAt: endedAt}},
		{name: "nonterminal", completion: Completion{ID: "task", Status: StatusRunning, StopReason: StopCompleted, EndedAt: endedAt}},
		{name: "missing end time", completion: Completion{ID: "task", Status: StatusCompleted, StopReason: StopCompleted}},
		{name: "completed with error", completion: Completion{ID: "task", Status: StatusCompleted, StopReason: StopCompleted, Error: SafeError(ErrInternal, redactor.Redact("bad"), false), EndedAt: endedAt}},
		{name: "failed without error", completion: Completion{ID: "task", Status: StatusFailed, StopReason: StopInternalError, EndedAt: endedAt}},
		{name: "wrong status reason", completion: Completion{ID: "task", Status: StatusTimedOut, StopReason: StopCancelled, Error: SafeError(ErrTimedOut, redactor.Redact("timeout"), true), EndedAt: endedAt}},
		{name: "wrong error code", completion: Completion{ID: "task", Status: StatusFailed, StopReason: StopProviderError, Error: SafeError(ErrToolFailed, redactor.Redact("tool"), true), EndedAt: endedAt}},
		{name: "negative usage", completion: Completion{ID: "task", Status: StatusCompleted, StopReason: StopCompleted, Usage: Usage{InputTokens: -1}, EndedAt: endedAt}},
		{name: "reason without truncation", completion: Completion{ID: "task", Status: StatusCompleted, StopReason: StopCompleted, TruncationReason: redactor.Redact("max_bytes"), EndedAt: endedAt}},
		{name: "truncation without reason", completion: Completion{ID: "task", Status: StatusCompleted, StopReason: StopCompleted, SummaryTruncated: true, EndedAt: endedAt}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := test.completion.Validate(); err == nil {
				t.Fatalf("invalid completion accepted: %#v", test.completion)
			}
		})
	}
}

func TestCloneDetachesCompletionSnapshotAndCollections(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	startedAt := time.Date(2026, time.August, 15, 8, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(time.Minute)
	confirmation := &events.ToolConfirmationRequest{
		ConfirmationID: "confirmation",
		CallID:         "call",
		Scopes:         []events.ConfirmationScopeDisplay{{Scope: "once", Available: true, Description: redactor.Redact("once")}},
	}
	source := TaskSnapshot{
		ID: "task", Revision: 9, Type: TypeDefined, Origin: OriginModel, Role: "reviewer",
		RoleSource: agentrole.SourceProject, RoleSourceID: "project", RoleProviderID: "provider",
		RoleOrigin: redactor.Redact("role.md"), RoleGeneration: 3, Placement: Foreground,
		Status: StatusWaitingConfirmation, Parent: ParentRef{ConversationID: "conversation", ExecutionID: "execution", RequestGeneration: 4},
		CreatedAt: startedAt, StartedAt: &startedAt, EndedAt: &endedAt, Iteration: 2, MaxIterations: 8,
		Error: SafeError(ErrInternal, redactor.Redact("safe"), false), PendingConfirmation: confirmation,
	}
	cloned := source.Clone()
	*source.StartedAt = source.StartedAt.Add(time.Hour)
	*source.EndedAt = source.EndedAt.Add(time.Hour)
	source.Error.Code = "mutated"
	source.PendingConfirmation.CallID = "mutated"
	source.PendingConfirmation.Scopes[0].Scope = "mutated"
	if cloned.StartedAt == source.StartedAt || cloned.EndedAt == source.EndedAt || cloned.Error == source.Error || cloned.PendingConfirmation == source.PendingConfirmation {
		t.Fatal("task snapshot clone retained producer pointers")
	}
	if cloned.StartedAt.Equal(*source.StartedAt) || cloned.EndedAt.Equal(*source.EndedAt) || cloned.Error.Code != string(ErrInternal) ||
		cloned.PendingConfirmation.CallID != "call" || cloned.PendingConfirmation.Scopes[0].Scope != "once" {
		t.Fatalf("task snapshot clone changed with source: %#v", cloned)
	}

	completion := Completion{ID: "task", Status: StatusFailed, StopReason: StopInternalError, Error: SafeError(ErrInternal, redactor.Redact("safe"), false), EndedAt: endedAt}
	completionClone := completion.Clone()
	completion.Error.Code = "mutated"
	if completionClone.Error == completion.Error || completionClone.Error.Code != string(ErrInternal) {
		t.Fatalf("completion clone aliased error: %#v", completionClone)
	}

	listSource := TaskListSnapshot{Watermark: 10, Tasks: []TaskSnapshot{source}}
	listClone := listSource.Clone()
	listSource.Tasks[0].Role = "mutated"
	if listClone.Tasks[0].Role != "reviewer" {
		t.Fatalf("task list clone aliased tasks: %#v", listClone)
	}
	emptyList := (TaskListSnapshot{Tasks: []TaskSnapshot{}}).Clone()
	if emptyList.Tasks == nil {
		t.Fatal("task list clone collapsed explicit empty slice")
	}
}

func TestAgentEventCloneAndValidationRejectUnsafeInnerEvents(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	source := AgentEvent{
		Kind: events.ToolWaitingConfirmation,
		Payload: events.Event{
			Type: events.ToolWaitingConfirmation,
			Confirmation: &events.ToolConfirmationRequest{
				ConfirmationID: "confirmation",
				CallID:         "call",
				Scopes:         []events.ConfirmationScopeDisplay{{Scope: "once", Available: true, Description: redactor.Redact("one call")}},
			},
		},
	}
	if err := source.Validate(); err != nil {
		t.Fatalf("safe Agent event rejected: %v", err)
	}
	cloned := source.Clone()
	source.Payload.Confirmation.CallID = "mutated"
	source.Payload.Confirmation.Scopes[0].Scope = "mutated"
	if cloned.Payload.Confirmation == source.Payload.Confirmation || cloned.Payload.Confirmation.CallID != "call" || cloned.Payload.Confirmation.Scopes[0].Scope != "once" {
		t.Fatalf("Agent event clone aliased payload: %#v", cloned)
	}

	unsafe := []AgentEvent{
		{Kind: events.Done, Payload: events.Event{Type: events.Done}},
		{Kind: events.MainTraceReset, Payload: events.Event{Type: events.MainTraceReset}},
		{Kind: events.TextDelta, Payload: events.Event{Type: events.TextDelta, IndependentID: "parent-trace"}},
		{Kind: events.TextDelta, Payload: events.Event{Type: events.ThinkingDelta}},
		{Kind: events.ToolSuccess, Payload: events.Event{Type: events.ToolSuccess}, Range: &DeltaRange{From: 0, To: 1}},
		{Kind: events.TextDelta, Payload: events.Event{Type: events.TextDelta}, Range: &DeltaRange{From: 2, To: 1}},
	}
	for index, event := range unsafe {
		if err := event.Validate(); err == nil {
			t.Errorf("unsafe Agent event %d accepted: %#v", index, event)
		}
	}
}

func TestTypeEventValidateOneOfUsesStrictKindPayloadMapping(t *testing.T) {
	snapshot := &TaskSnapshot{ID: "task"}
	agent := &AgentEvent{Kind: events.TextDelta, Payload: events.Event{Type: events.TextDelta}}
	completion := &Completion{ID: "task"}
	result := &ResultNotification{TaskID: "task"}
	gap := &GapDescriptor{FromRevision: 1, ToRevision: 2}
	placement := &PlacementChange{From: Foreground, To: Background, Reason: "manual"}
	decision := &ConfirmationDecisionDisplay{ConfirmationID: "confirmation", CallID: "call", Action: events.PermissionAllowOnce, Allowed: true}

	valid := []Event{
		{Kind: EventQueued, Snapshot: snapshot},
		{Kind: EventRunning, Snapshot: snapshot},
		{Kind: EventWaitingConfirmation, Snapshot: snapshot},
		{Kind: EventSettling, Snapshot: snapshot},
		{Kind: EventIterationStarted, Snapshot: snapshot},
		{Kind: EventProgress, Snapshot: snapshot},
		{Kind: EventTextDelta, Agent: agent},
		{Kind: EventThinkingDelta, Agent: agent},
		{Kind: EventTool, Agent: agent},
		{Kind: EventConfirmationRequested, Agent: agent},
		{Kind: EventUsage, Agent: agent},
		{Kind: EventDiagnostic, Agent: agent},
		{Kind: EventConfirmationResolved, Snapshot: snapshot, Decision: decision},
		{Kind: EventPlacementChanged, Snapshot: snapshot, Placement: placement},
		{Kind: EventCompletion, Snapshot: snapshot, Completion: completion},
		{Kind: EventFailure, Snapshot: snapshot, Completion: completion},
		{Kind: EventCancellation, Snapshot: snapshot, Completion: completion},
		{Kind: EventTimeout, Snapshot: snapshot, Completion: completion},
		{Kind: EventLimit, Snapshot: snapshot, Completion: completion},
		{Kind: EventResultPublished, Result: result},
		{Kind: EventGap, Gap: gap},
	}
	for _, event := range valid {
		if err := event.ValidateOneOf(); err != nil {
			t.Errorf("valid %q event rejected: %v", event.Kind, err)
		}
	}
	invalid := []Event{
		{Kind: EventQueued},
		{Kind: EventQueued, Snapshot: snapshot, Agent: agent},
		{Kind: EventConfirmationResolved, Snapshot: snapshot},
		{Kind: EventPlacementChanged, Snapshot: snapshot, Placement: placement, Completion: completion},
		{Kind: EventCompletion, Completion: completion},
		{Kind: EventResultPublished, Result: result, Snapshot: snapshot},
		{Kind: EventKind("unknown"), Snapshot: snapshot},
	}
	for index, event := range invalid {
		if err := event.ValidateOneOf(); err == nil {
			t.Errorf("invalid one-of event %d accepted: %#v", index, event)
		}
	}
}

func TestEventAndResultCloneDetachEveryPayload(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	event := Event{
		Agent:      &AgentEvent{Payload: events.Event{Err: &diagnostics.SafeError{Code: "safe"}}, Range: &DeltaRange{From: 1, To: 2}},
		Snapshot:   &TaskSnapshot{Role: "role", PendingConfirmation: &events.ToolConfirmationRequest{Scopes: []events.ConfirmationScopeDisplay{}}},
		Completion: &Completion{Error: SafeError(ErrInternal, redactor.Redact("safe"), false)},
		Result:     &ResultNotification{Error: SafeError(ErrInternal, redactor.Redact("safe"), false)},
		Gap:        &GapDescriptor{Reason: "slow"},
		Placement:  &PlacementChange{Reason: "manual"},
		Decision:   &ConfirmationDecisionDisplay{CallID: "call"},
	}
	cloned := event.Clone()
	event.Agent.Payload.Err.Code = "mutated"
	event.Agent.Range.From = 99
	event.Snapshot.Role = "mutated"
	event.Completion.Error.Code = "mutated"
	event.Result.Error.Code = "mutated"
	event.Gap.Reason = "mutated"
	event.Placement.Reason = "mutated"
	event.Decision.CallID = "mutated"
	if cloned.Agent == event.Agent || cloned.Agent.Payload.Err.Code != "safe" || cloned.Agent.Range.From != 1 ||
		cloned.Snapshot == event.Snapshot || cloned.Snapshot.Role != "role" || cloned.Snapshot.PendingConfirmation.Scopes == nil ||
		cloned.Completion == event.Completion || cloned.Completion.Error.Code != string(ErrInternal) ||
		cloned.Result == event.Result || cloned.Result.Error.Code != string(ErrInternal) ||
		cloned.Gap == event.Gap || cloned.Gap.Reason != "slow" || cloned.Placement == event.Placement || cloned.Placement.Reason != "manual" ||
		cloned.Decision == event.Decision || cloned.Decision.CallID != "call" {
		t.Fatalf("event clone retained mutable payload storage: %#v", cloned)
	}

	notification := ResultNotification{Error: SafeError(ErrInternal, redactor.Redact("safe"), false)}
	notificationClone := notification.Clone()
	notification.Error.Code = "mutated"
	if notificationClone.Error == notification.Error || notificationClone.Error.Code != string(ErrInternal) {
		t.Fatalf("result notification clone aliased error: %#v", notificationClone)
	}

	detail := TaskDetailSnapshot{Task: TaskSnapshot{Role: "role"}, RecentEvents: []Event{event}}
	detailClone := detail.Clone()
	detail.Task.Role = "mutated"
	detail.RecentEvents[0].Gap.Reason = "again"
	if detailClone.Task.Role != "role" || detailClone.RecentEvents[0].Gap.Reason != "mutated" {
		t.Fatalf("task detail clone aliased nested data: %#v", detailClone)
	}
	emptyDetail := (TaskDetailSnapshot{RecentEvents: []Event{}}).Clone()
	if emptyDetail.RecentEvents == nil {
		t.Fatal("task detail clone collapsed explicit empty events")
	}
}
