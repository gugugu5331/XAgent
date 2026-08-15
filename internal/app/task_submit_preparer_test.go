package app

import (
	"context"
	"testing"
	"time"

	"xagent/internal/command"
	"xagent/internal/conversation"
	"xagent/internal/orchestrator"
	"xagent/internal/subagent"
)

type taskSubmitContextKey struct{}

type taskSubmitPreparerFake struct {
	calls  int
	mode   orchestrator.RunMode
	parent *conversation.Conversation
}

func (fake *taskSubmitPreparerFake) PrepareTUISubagentSubmit(
	ctx context.Context,
	parent *conversation.Conversation,
	mode orchestrator.RunMode,
	input subagent.SubmitInput,
) (context.Context, subagent.SubmitInput, error) {
	fake.calls++
	fake.mode = mode
	fake.parent = parent
	input.Origin = subagent.OriginTUI
	input.Parent = subagent.ParentRef{
		ConversationID: parent.ID, ExecutionID: "tui-request-7", RequestGeneration: 7,
	}
	input.Invocation = subagent.InvocationRef{}
	return context.WithValue(ctx, taskSubmitContextKey{}, "prepared"), input, nil
}

type taskPreparedContextService struct {
	*taskCommandServiceFake
	contextValue any
	input        subagent.SubmitInput
}

func (service *taskPreparedContextService) Submit(ctx context.Context, input subagent.SubmitInput) (subagent.Submission, error) {
	service.contextValue = ctx.Value(taskSubmitContextKey{})
	service.input = input
	return service.taskCommandServiceFake.Submit(ctx, input)
}

func TestTaskIntentSubmitUsesPreparedParentContextBeforeSharedService(t *testing.T) {
	t.Parallel()

	base := &taskCommandServiceFake{submitResult: subagent.Submission{
		ID: "task-prepared", Status: subagent.StatusQueued, Placement: subagent.Background,
	}}
	service := &taskPreparedContextService{taskCommandServiceFake: base}
	preparer := &taskSubmitPreparerFake{}
	parent := conversation.NewConversation("conversation-prepared", time.Unix(1_725_000_000, 0).UTC())
	model := Model{
		deps:         Deps{Tasks: service, TaskSubmitPreparer: preparer},
		conversation: parent,
		mode:         orchestrator.RunModePlan,
	}
	controller := &commandController{model: &model}
	if err := controller.HandleTaskIntent(command.TaskIntent{
		Kind: command.TaskIntentSubmit,
		Submit: subagent.SubmitInput{
			Task: "delegate safely", Type: subagent.TypeDefined, Role: "review",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if preparer.calls != 1 || preparer.parent != parent || preparer.mode != orchestrator.RunModePlan {
		t.Fatalf("preparer call = calls:%d parent:%p mode:%q", preparer.calls, preparer.parent, preparer.mode)
	}
	if service.contextValue != "prepared" || service.input.Parent.RequestGeneration != 7 ||
		service.input.Parent.ExecutionID != "tui-request-7" || service.input.Origin != subagent.OriginTUI {
		t.Fatalf("shared Submit did not receive prepared boundary: value=%v input=%#v", service.contextValue, service.input)
	}
}
