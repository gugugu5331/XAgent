package orchestrator

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/provider"
	"xagent/internal/subagent"
	"xagent/internal/tool"
)

func TestPrepareTUISubagentSubmitCapturesDetachedBudgetedParent(t *testing.T) {
	t.Parallel()

	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	orchestrator := NewWithOptions(OrchestratorOptions{
		Registry: registry, DefaultModel: "model-default",
	})
	now := time.Unix(1_725_000_000, 0).UTC()
	parent := conversation.NewConversation("conversation-tui-parent", now)
	conversation.AppendUserMessage(parent, "committed parent message")

	preparedContext, preparedInput, err := orchestrator.PrepareTUISubagentSubmit(
		context.Background(), parent, RunModePlan,
		subagent.SubmitInput{
			Task: "inspect the parent", Type: subagent.TypeFork,
			Placement: subagent.PlacementForeground,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if preparedInput.Origin != subagent.OriginTUI || preparedInput.Invocation != (subagent.InvocationRef{}) {
		t.Fatalf("trusted TUI identity = %#v", preparedInput)
	}
	if preparedInput.Parent.ConversationID != parent.ID || preparedInput.Parent.ExecutionID == "" ||
		preparedInput.Parent.RequestGeneration == 0 {
		t.Fatalf("parent identity = %#v", preparedInput.Parent)
	}

	runtime, err := ParentRuntimeSnapshotFromSubmitContext(preparedContext, preparedInput.Parent)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Model != "model-default" || !runtime.PlanMode || runtime.Registry == nil || !runtime.Registry.IsSealed() {
		t.Fatalf("runtime baseline = %#v", runtime)
	}
	if _, ok := runtime.Registry.Get("Write"); ok {
		t.Fatalf("Plan parent registry exposed Write: %v", runtime.Registry.Names())
	}
	if _, ok := runtime.Registry.Get("Read"); !ok {
		t.Fatalf("Plan parent registry lost Read: %v", runtime.Registry.Names())
	}
	if err := runtime.Prompt.Validate(); err != nil {
		t.Fatalf("captured prompt is invalid: %v", err)
	}
	if len(runtime.Prompt.Messages) != 1 || runtime.Prompt.Messages[0].Content.Text() != "committed parent message" {
		t.Fatalf("captured messages = %#v", runtime.Prompt.Messages)
	}
	got, want := promptToolNames(runtime.Prompt), runtime.Registry.Names()
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prompt tools = %v, registry = %v", got, want)
	}

	conversation.AppendAssistantMessage(parent, "later parent mutation")
	detached := runtime.Conversation.MaterializeEphemeral("task-audit-copy", now.Add(time.Minute))
	if detached == nil || len(detached.Messages) != 1 || detached.Messages[0].Content.Text() != "committed parent message" {
		t.Fatalf("conversation snapshot was not detached: %#v", detached)
	}

	_, second, err := orchestrator.PrepareTUISubagentSubmit(
		context.Background(), parent, RunModeDefault,
		subagent.SubmitInput{Task: "second", Type: subagent.TypeDefined, Role: "review"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.Parent.RequestGeneration <= preparedInput.Parent.RequestGeneration ||
		second.Parent.ExecutionID == preparedInput.Parent.ExecutionID {
		t.Fatalf("TUI parent generation did not advance: first=%#v second=%#v", preparedInput.Parent, second.Parent)
	}
}

func promptToolNames(snapshot provider.PromptPrefixSnapshot) []string {
	names := make([]string, len(snapshot.Tools))
	for index := range snapshot.Tools {
		names[index] = snapshot.Tools[index].Name
	}
	return names
}
