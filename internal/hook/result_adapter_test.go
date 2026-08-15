package hook

import (
	"context"
	"reflect"
	"testing"

	"xagent/internal/artifact"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestSyntheticResultAdapterRetainsOnlyResultFactory(t *testing.T) {
	if adapter, err := NewSyntheticResultAdapter(nil); err == nil || adapter != nil {
		t.Fatal("synthetic Hook adapter accepted a missing ResultFactory")
	}
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret("hook-adapter-secret")
	factory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal("create shared ResultFactory failed")
	}
	adapter, err := NewSyntheticResultAdapter(factory)
	if err != nil {
		t.Fatal("create synthetic Hook adapter failed")
	}
	result, err := adapter.Build(tool.ResultFactoryInput{
		CallID: "hook-denied", Name: "Write", State: tool.Rejected, Status: tool.StatusDenied,
		Summary: "Hook denied hook-adapter-secret", Preview: "hook-adapter-secret",
		Error: &tool.Error{Code: tool.ErrHookDenied, Message: "hook-adapter-secret", Recoverable: true},
	})
	if err != nil || result.UserView().Preview.Text() != "[redacted]" || result.UserView().Error == nil ||
		result.UserView().Error.Message.Text() != "[redacted]" {
		t.Fatalf("synthetic Hook result did not use the shared ResultFactory: result=%#v err=%v", result, err)
	}

	adapterType := reflect.TypeOf(*adapter)
	if adapterType.NumField() != 1 || adapterType.Field(0).Type != reflect.TypeOf((*tool.ResultFactory)(nil)) {
		t.Fatalf("synthetic Hook adapter capability shape changed: %v", adapterType)
	}
	for _, forbidden := range []reflect.Type{
		reflect.TypeOf((*artifact.Store)(nil)).Elem(),
		reflect.TypeOf((func(context.Context, artifact.Metadata) (*tool.Capture, error))(nil)),
	} {
		for index := 0; index < adapterType.NumField(); index++ {
			if adapterType.Field(index).Type == forbidden {
				t.Fatalf("synthetic Hook adapter retained forbidden capability %v", forbidden)
			}
		}
	}
}

func TestToolResultHookAdapterContainsOnlyUserView(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	view := tool.UserView{
		State:   tool.Completed,
		Status:  tool.StatusError,
		Summary: redactor.Redact("safe summary"),
		Preview: redactor.Redact("user preview"),
		Error: &tool.SafeError{
			Code:        "safe_error",
			Message:     redactor.Redact("safe message"),
			Recoverable: true,
		},
	}
	output := ToolOutputFromUserView(view)
	if output.Status != ToolErrorStatus || output.Content.Text() != view.Preview.Text() || output.Error == nil ||
		output.Error.Code != view.Error.Code || output.Error.Message.Text() != view.Error.Message.Text() || !output.Error.Recoverable {
		t.Fatalf("hook output was not derived from UserView: %#v", output)
	}
	view.Error.Code = "mutated"
	if output.Error.Code == view.Error.Code {
		t.Fatalf("hook output aliases UserView error: %#v", output)
	}

	withoutPreview := tool.UserView{State: tool.Completed, Status: tool.StatusSuccess, Summary: redactor.Redact("summary fallback")}
	fallback := ToolOutputFromUserView(withoutPreview)
	if fallback.Status != ToolSuccess || fallback.Content.Text() != withoutPreview.Summary.Text() || fallback.Error != nil {
		t.Fatalf("hook summary fallback was not derived from UserView: %#v", fallback)
	}
}
