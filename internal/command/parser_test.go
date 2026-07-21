package command

import (
	"errors"
	"strings"
	"testing"
)

func TestDispatchClassifiesEmptyAndPlainText(t *testing.T) {
	registry := MustNew(Definition{Name: "run", Type: TypeLocal, Handler: noopHandler})
	for _, input := range []string{"", "   ", "\t\n"} {
		if result := registry.Dispatch(input, &testController{}); result.Kind != DispatchEmpty {
			t.Fatalf("expected empty for %q, got %#v", input, result)
		}
	}
	result := registry.Dispatch("  hello  world  ", &testController{})
	if result.Kind != DispatchPlainText || result.Text != "hello  world" {
		t.Fatalf("unexpected plain text result: %#v", result)
	}
}

func TestDispatchMatchesCaseInsensitiveNameAndAlias(t *testing.T) {
	var invocations []Invocation
	registry := MustNew(Definition{Name: "run", Aliases: []string{"r"}, Type: TypeLocal, Handler: func(_ ExecutionContext, invocation Invocation) error {
		invocations = append(invocations, invocation)
		return nil
	}})
	for _, input := range []string{"/RUN", "/R"} {
		result := registry.Dispatch(input, &testController{})
		if result.Kind != DispatchExecuted || result.Err != nil {
			t.Fatalf("unexpected result for %q: %#v", input, result)
		}
	}
	if invocations[0].CanonicalName != "run" || invocations[0].MatchedName != "run" || invocations[1].MatchedName != "r" {
		t.Fatalf("unexpected invocations: %#v", invocations)
	}
}

func TestDispatchPreservesArgumentCaseAndInternalWhitespace(t *testing.T) {
	var invocation Invocation
	registry := MustNew(Definition{Name: "run", Type: TypeLocal, Handler: func(_ ExecutionContext, current Invocation) error {
		invocation = current
		return nil
	}})
	result := registry.Dispatch("  /RuN\tArg One  TWO  ", &testController{})
	if result.Kind != DispatchExecuted || invocation.Args != "Arg One  TWO" || invocation.Raw != "/RuN\tArg One  TWO" {
		t.Fatalf("argument was not preserved: result=%#v invocation=%#v", result, invocation)
	}
}

func TestDispatchUnknownNeverSendsUserMessage(t *testing.T) {
	controller := &testController{}
	registry := MustNew(Definition{Name: "known", Type: TypeLocal, Handler: noopHandler})
	result := registry.Dispatch("/unknown value", controller)
	if result.Kind != DispatchUnknown || result.Err == nil || len(controller.sent) != 0 || !strings.Contains(controller.lastError(), "/help") {
		t.Fatalf("unexpected unknown result: %#v controller=%#v", result, controller)
	}
}

func TestDispatchReportsHandlerErrorAsCommandResult(t *testing.T) {
	controller := &testController{}
	want := errors.New("failed")
	registry := MustNew(Definition{Name: "fail", Type: TypeLocal, Handler: func(ExecutionContext, Invocation) error { return want }})
	result := registry.Dispatch("/fail", controller)
	if result.Kind != DispatchExecuted || !errors.Is(result.Err, want) || controller.lastError() != "failed" {
		t.Fatalf("unexpected error result: %#v controller=%#v", result, controller)
	}
}
