package events

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func safeTestText(value string) redact.SafeText {
	return redact.NewRuntimeRedactor().Redact(value)
}

func TestToolResultEventContainsOnlyUserView(t *testing.T) {
	createdAt := time.Date(2026, time.August, 3, 13, 0, 0, 0, time.UTC)
	reference := &artifact.Ref{ID: strings.Repeat("b", 64), Bytes: 91, CreatedAt: createdAt, Available: true, Complete: false}
	view := tool.UserView{
		State:            tool.CancelledAfterStart,
		Status:           tool.StatusTimeout,
		Summary:          safeTestText("safe summary"),
		Preview:          safeTestText("user-preview-only"),
		Artifact:         reference,
		Truncated:        true,
		TruncationReason: safeTestText("capture_canceled"),
		Error:            &tool.SafeError{Code: tool.ErrTimeout, Message: safeTestText("safe timeout"), Recoverable: true},
	}
	event := ToolResultEventFromUserView("call-1", "Bash", safeTestText(`{"command":"safe"}`), view)
	if event.Type != ToolError || event.Tool == nil {
		t.Fatalf("unexpected result event: %#v", event)
	}
	display := event.Tool
	if display.CallID != "call-1" || display.Name != "Bash" || display.Status != ToolDisplayCancelled ||
		display.Summary.Text() != view.Summary.Text() || display.Stdout.Text() != view.Preview.Text() ||
		display.ErrorCode != view.Error.Code || display.Stderr.Text() != view.Error.Message.Text() || !display.Recoverable ||
		display.Truncated != view.Truncated || display.TruncationReason.Text() != view.TruncationReason.Text() ||
		display.Artifact == nil || display.Artifact.ID != reference.ID || display.Artifact.Bytes != reference.Bytes ||
		display.Artifact.Available != reference.Available || display.Artifact.Complete != reference.Complete ||
		!display.Artifact.CreatedAt.Equal(reference.CreatedAt) {
		t.Fatalf("event was not the exact UserView projection: %#v", display)
	}
	reference.ID = strings.Repeat("c", 64)
	view.Error.Code = "mutated"
	if display.Artifact.ID == reference.ID || display.ErrorCode == view.Error.Code {
		t.Fatalf("event aliases mutable UserView fields: %#v", display)
	}
}

func TestEventPayloadsAreZeroValueCompatible(t *testing.T) {
	tool := ToolDisplay{CallID: "call", Name: "Bash", Status: ToolDisplayError, ErrorCode: "exit_1", Stdout: safeTestText("out"), Stderr: safeTestText("err"), Truncated: true, Artifact: &ArtifactRef{ID: "artifact", Bytes: 10, Available: true}}
	if tool.CallID != "call" || !tool.Truncated || tool.Artifact == nil || !tool.Artifact.Available {
		t.Fatalf("unexpected tool display: %#v", tool)
	}
	confirmation := ToolConfirmationRequest{
		CallID: "call", Target: safeTestText("project"), Risk: "high", PermissionMode: "default",
		ScopePreview: safeTestText("Bash(*)"), Scopes: []ConfirmationScopeDisplay{{Scope: "once", Available: true, Description: safeTestText("one call")}},
		RuleLocation: safeTestText("local rules"), Warning: safeTestText("not sandboxed"), RevokeHint: safeTestText("edit local rules"),
	}
	if confirmation.Risk != "high" || confirmation.AllowPermanent {
		t.Fatalf("unexpected confirmation: %#v", confirmation)
	}
	diagnostic := DiagnosticDisplay{Code: "mcp_missing_env", Severity: "warning", Source: "mcp", Message: safeTestText("missing env"), Hint: safeTestText("set env")}
	event := Event{Type: DiagnosticEmitted, Diagnostic: &diagnostic}
	if event.Type != DiagnosticEmitted || event.Diagnostic.Code != "mcp_missing_env" {
		t.Fatalf("unexpected diagnostic event: %#v", event)
	}
	if event.Transient || event.IndependentID != "" {
		t.Fatalf("legacy event unexpectedly marked independent: %#v", event)
	}
}

func TestEventsContainOnlySafeDTOs(t *testing.T) {
	t.Parallel()

	safeTextType := reflect.TypeOf(redact.SafeText{})
	assertEventDTOFields(t, reflect.TypeOf(Event{}), map[string]reflect.Type{
		"Type": reflect.TypeOf(Type("")), "Text": safeTextType, "Duration": reflect.TypeOf(time.Duration(0)),
		"Err": reflect.TypeOf((*diagnostics.SafeError)(nil)), "Transient": reflect.TypeOf(false), "IndependentID": reflect.TypeOf(""),
		"Tool": reflect.TypeOf((*ToolDisplay)(nil)), "Confirmation": reflect.TypeOf((*ToolConfirmationRequest)(nil)),
		"Diagnostic": reflect.TypeOf((*DiagnosticDisplay)(nil)), "Progress": reflect.TypeOf((*AgentProgress)(nil)),
		"Usage": reflect.TypeOf((*UsageDisplay)(nil)),
	})
	assertEventDTOFields(t, reflect.TypeOf(ToolDisplay{}), map[string]reflect.Type{
		"CallID": reflect.TypeOf(""), "Name": reflect.TypeOf(""), "Arguments": safeTextType, "Summary": safeTextType,
		"Status": reflect.TypeOf(ToolDisplayStatus("")), "ErrorCode": reflect.TypeOf(""), "Stdout": safeTextType,
		"Stderr": safeTextType, "Truncated": reflect.TypeOf(false), "TruncationReason": safeTextType,
		"Recoverable": reflect.TypeOf(false), "Artifact": reflect.TypeOf((*ArtifactRef)(nil)),
	})
	assertEventDTOFields(t, reflect.TypeOf(ToolConfirmationRequest{}), map[string]reflect.Type{
		"ConfirmationID": reflect.TypeOf(""), "CallID": reflect.TypeOf(""), "Name": reflect.TypeOf(""),
		"Arguments": safeTextType, "Prompt": safeTextType, "Target": safeTextType, "Risk": reflect.TypeOf(""),
		"PermissionMode": reflect.TypeOf(""), "ScopePreview": safeTextType,
		"Scopes": reflect.TypeOf([]ConfirmationScopeDisplay(nil)), "RuleLocation": safeTextType,
		"Warning": safeTextType, "RevokeHint": safeTextType, "AllowPermanent": reflect.TypeOf(false),
	})
	assertEventDTOFields(t, reflect.TypeOf(ConfirmationScopeDisplay{}), map[string]reflect.Type{
		"Scope": reflect.TypeOf(""), "Available": reflect.TypeOf(false), "Description": safeTextType,
	})
	assertEventDTOFields(t, reflect.TypeOf(ToolConfirmationDecision{}), map[string]reflect.Type{
		"ConfirmationID": reflect.TypeOf(""), "CallID": reflect.TypeOf(""), "Allowed": reflect.TypeOf(false),
		"Action": reflect.TypeOf(PermissionAction("")),
	})
	assertEventDTOFields(t, reflect.TypeOf(DiagnosticDisplay{}), map[string]reflect.Type{
		"Code": reflect.TypeOf(""), "Severity": reflect.TypeOf(""), "Source": reflect.TypeOf(""),
		"Message": safeTextType, "Hint": safeTextType,
	})
	assertEventDTOFields(t, reflect.TypeOf(AgentProgress{}), map[string]reflect.Type{
		"Iteration": reflect.TypeOf(int(0)), "Max": reflect.TypeOf(int(0)), "StopReason": reflect.TypeOf(""), "Message": safeTextType,
	})
	assertEventDTOFields(t, reflect.TypeOf(UsageDisplay{}), map[string]reflect.Type{
		"InputTokens": reflect.TypeOf(int64(0)), "OutputTokens": reflect.TypeOf(int64(0)),
		"CacheCreationInputTokens": reflect.TypeOf(int64(0)), "CacheReadInputTokens": reflect.TypeOf(int64(0)),
	})
	assertEventDTOFields(t, reflect.TypeOf(ArtifactRef{}), map[string]reflect.Type{
		"ID": reflect.TypeOf(""), "Bytes": reflect.TypeOf(int64(0)), "Available": reflect.TypeOf(false),
		"Complete": reflect.TypeOf(false), "CreatedAt": reflect.TypeOf(time.Time{}),
	})

	source := Event{
		Type: ToolSuccess,
		Err:  &diagnostics.SafeError{Code: "safe", Message: safeTestText("safe error")},
		Tool: &ToolDisplay{CallID: "call", Artifact: &ArtifactRef{ID: "artifact", Available: true}},
		Confirmation: &ToolConfirmationRequest{
			ConfirmationID: "confirmation-1", CallID: "call",
			Scopes: []ConfirmationScopeDisplay{{Scope: "once", Available: true, Description: safeTestText("one call")}},
		},
		Usage: &UsageDisplay{InputTokens: 3},
	}
	cloned := Clone(source)
	source.Err.Code = "mutated"
	source.Tool.CallID = "mutated"
	source.Tool.Artifact.ID = "mutated"
	source.Confirmation.CallID = "mutated"
	source.Confirmation.Scopes[0].Scope = "mutated"
	source.Usage.InputTokens = 99
	if cloned.Err == nil || cloned.Err.Code != "safe" || cloned.Tool == nil || cloned.Tool.CallID != "call" ||
		cloned.Tool.Artifact == nil || cloned.Tool.Artifact.ID != "artifact" || cloned.Confirmation == nil ||
		cloned.Confirmation.CallID != "call" || len(cloned.Confirmation.Scopes) != 1 ||
		cloned.Confirmation.Scopes[0].Scope != "once" || cloned.Usage == nil || cloned.Usage.InputTokens != 3 {
		t.Fatalf("cloned event aliases producer storage: %#v", cloned)
	}
	emptyScopes := Clone(Event{Confirmation: &ToolConfirmationRequest{Scopes: []ConfirmationScopeDisplay{}}})
	if emptyScopes.Confirmation == nil || emptyScopes.Confirmation.Scopes == nil || len(emptyScopes.Confirmation.Scopes) != 0 {
		t.Fatalf("event clone collapsed an explicit empty scopes projection: %#v", emptyScopes.Confirmation)
	}
}

func assertEventDTOFields(t *testing.T, valueType reflect.Type, want map[string]reflect.Type) {
	t.Helper()
	if valueType.Kind() != reflect.Struct || valueType.NumField() != len(want) {
		t.Fatalf("%v field count = %d, want exact safe schema of %d fields", valueType, valueType.NumField(), len(want))
	}
	for index := 0; index < valueType.NumField(); index++ {
		field := valueType.Field(index)
		lowerName := strings.ToLower(field.Name)
		if strings.Contains(lowerName, "raw") || strings.Contains(lowerName, "payload") || strings.Contains(lowerName, "path") {
			t.Fatalf("event DTO field %s.%s exposes raw/path/payload data", valueType, field.Name)
		}
		wantType, exists := want[field.Name]
		if !exists {
			t.Fatalf("event DTO %v has unapproved field %s of type %v", valueType, field.Name, field.Type)
		}
		if field.Type != wantType {
			t.Fatalf("event DTO %s.%s type = %v, want %v", valueType, field.Name, field.Type, wantType)
		}
	}
}

func TestEventMarksIndependentTransientFlow(t *testing.T) {
	event := Event{Type: TextDelta, Text: safeTestText("working"), Transient: true, IndependentID: "run-1"}
	if !event.Transient || event.IndependentID != "run-1" {
		t.Fatalf("independent marker was not retained: %#v", event)
	}

	final := Event{Type: TextDelta, Text: safeTestText("summary"), IndependentID: "run-1"}
	if final.Transient {
		t.Fatalf("final summary must remain persistent: %#v", final)
	}
}
