package command

import (
	"reflect"
	"strings"
	"testing"

	"xagent/internal/events"
	"xagent/internal/subagent"
)

type recordingTaskIntentController struct {
	*testController
	intents []TaskIntent
}

func (controller *recordingTaskIntentController) HandleTaskIntent(intent TaskIntent) error {
	controller.intents = append(controller.intents, intent)
	return nil
}

func TestAgentTaskIntentParsesValueOnlySubmit(t *testing.T) {
	controller := &recordingTaskIntentController{testController: &testController{}}
	registry := MustNew(Builtins()...)
	result := registry.Dispatch(`/agent defined --role=explore --placement=background -- inspect the repository`, controller)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	want := TaskIntent{
		Kind: TaskIntentSubmit,
		Submit: subagent.SubmitInput{
			Task: "inspect the repository", Type: subagent.TypeDefined,
			Role: "explore", Placement: subagent.PlacementBackground,
		},
	}
	if !reflect.DeepEqual(controller.intents, []TaskIntent{want}) {
		t.Fatalf("task intents = %#v, want %#v", controller.intents, []TaskIntent{want})
	}
	if controller.intents[0].Submit.Origin != "" || controller.intents[0].Submit.Parent != (subagent.ParentRef{}) ||
		controller.intents[0].Submit.Invocation != (subagent.InvocationRef{}) {
		t.Fatalf("command intent captured trusted routing fields: %#v", controller.intents[0].Submit)
	}
}

func TestAgentTaskIntentSupportsStablePlacementAliasesAndPreservesTask(t *testing.T) {
	tests := []struct {
		input     string
		typeOf    subagent.ExecutionType
		placement subagent.PlacementIntent
		task      string
	}{
		{input: `/agent defined --foreground fix  two spaces`, typeOf: subagent.TypeDefined, placement: subagent.PlacementForeground, task: "fix  two spaces"},
		{input: `/agent fork --background -- --literal task`, typeOf: subagent.TypeFork, placement: subagent.PlacementBackground, task: "--literal task"},
		{input: `/agent defined plain task`, typeOf: subagent.TypeDefined, placement: subagent.PlacementDefault, task: "plain task"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			intent, err := ParseAgentTaskIntent(test.input[len("/agent "):])
			if err != nil {
				t.Fatal(err)
			}
			if intent.Kind != TaskIntentSubmit || intent.Submit.Type != test.typeOf ||
				intent.Submit.Placement != test.placement || intent.Submit.Task != test.task {
				t.Fatalf("intent = %#v", intent)
			}
		})
	}
}

func TestAgentTaskIntentRejectsMalformedInputBeforeSink(t *testing.T) {
	invalid := []string{
		``, `defined`, `unknown task`, `defined --role task`, `defined --role= task`,
		`defined --placement=sideways task`, `defined --foreground --background task`,
		`defined --unknown=value task`, `defined --`,
	}
	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseAgentTaskIntent(input); err == nil {
				t.Fatalf("ParseAgentTaskIntent(%q) succeeded", input)
			}
		})
	}

	controller := &recordingTaskIntentController{testController: &testController{}}
	result := MustNew(Builtins()...).Dispatch(`/agent defined --placement=sideways task`, controller)
	if result.Err == nil || len(controller.intents) != 0 {
		t.Fatalf("malformed command reached sink: result=%#v intents=%#v", result, controller.intents)
	}
}

func TestTasksAndTaskCommandsProduceTypedOperations(t *testing.T) {
	tests := []struct {
		input string
		want  TaskIntent
	}{
		{input: `/tasks`, want: TaskIntent{Kind: TaskIntentList}},
		{input: `/task task-1`, want: TaskIntent{Kind: TaskIntentDetail, TaskID: "task-1"}},
		{input: `/task task-1 cancel`, want: TaskIntent{Kind: TaskIntentCancel, TaskID: "task-1"}},
		{input: `/task task-1 background`, want: TaskIntent{Kind: TaskIntentMoveToBackground, TaskID: "task-1"}},
		{input: `/task task-1 confirm confirmation-1 call-1 allow_once`, want: TaskIntent{
			Kind: TaskIntentResolveConfirmation, TaskID: "task-1",
			Decision: events.ToolConfirmationDecision{ConfirmationID: "confirmation-1", CallID: "call-1", Action: events.PermissionAllowOnce, Allowed: true},
		}},
		{input: `/task task-1 confirm confirmation-1 call-1 deny`, want: TaskIntent{
			Kind: TaskIntentResolveConfirmation, TaskID: "task-1",
			Decision: events.ToolConfirmationDecision{ConfirmationID: "confirmation-1", CallID: "call-1", Action: events.PermissionDeny},
		}},
	}
	controller := &recordingTaskIntentController{testController: &testController{}}
	registry := MustNew(Builtins()...)
	for _, test := range tests {
		result := registry.Dispatch(test.input, controller)
		if result.Err != nil {
			t.Fatalf("Dispatch(%q): %v", test.input, result.Err)
		}
		got := controller.intents[len(controller.intents)-1]
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("Dispatch(%q) = %#v, want %#v", test.input, got, test.want)
		}
	}
}

func TestTaskIntentRejectsInvalidOperationsAndPermanentAuthorization(t *testing.T) {
	invalid := []string{
		`/tasks extra`, `/task`, `/task task-1 unknown`, `/task task-1 cancel extra`,
		`/task task-1 confirm`, `/task task-1 confirm confirmation call allow_permanent`,
		`/task task-1 confirm confirmation call unknown`,
	}
	controller := &recordingTaskIntentController{testController: &testController{}}
	registry := MustNew(Builtins()...)
	for _, input := range invalid {
		before := len(controller.intents)
		result := registry.Dispatch(input, controller)
		if result.Err == nil || len(controller.intents) != before {
			t.Fatalf("invalid %q reached sink: result=%#v intents=%#v", input, result, controller.intents)
		}
	}
}

func TestTaskIntentValidatesSyntaxBeforeRequiringSink(t *testing.T) {
	controller := &testController{}
	for _, input := range []string{
		`/agent defined --unknown=value task`,
		`/tasks extra`,
		`/task task-1 confirm confirmation call allow_permanent`,
	} {
		result := MustNew(Builtins()...).Dispatch(input, controller)
		if result.Err == nil || strings.Contains(result.Err.Error(), "控制器不可用") {
			t.Fatalf("%q checked sink before syntax: %v", input, result.Err)
		}
	}
}
