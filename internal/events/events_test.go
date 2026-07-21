package events

import "testing"

func TestEventPayloadsAreZeroValueCompatible(t *testing.T) {
	tool := ToolDisplay{CallID: "call", Name: "Bash", Status: ToolDisplayError, ErrorCode: "exit_1", Stdout: "out", Stderr: "err", Truncated: true, ArtifactID: "artifact", ArtifactBytes: 10, ArtifactAvailable: true}
	if tool.CallID != "call" || !tool.Truncated || !tool.ArtifactAvailable {
		t.Fatalf("unexpected tool display: %#v", tool)
	}
	confirmation := ToolConfirmationRequest{CallID: "call", Risk: "high", PermissionMode: "default", ScopePreview: "Bash(*)", Warning: "not sandboxed", RevokeHint: "edit local rules"}
	if confirmation.Risk != "high" || confirmation.AllowPermanent {
		t.Fatalf("unexpected confirmation: %#v", confirmation)
	}
	diagnostic := DiagnosticDisplay{Code: "mcp_missing_env", Severity: "warning", Source: "mcp", Message: "missing env", Hint: "set env"}
	event := Event{Type: DiagnosticEmitted, Diagnostic: &diagnostic}
	if event.Type != DiagnosticEmitted || event.Diagnostic.Code != "mcp_missing_env" {
		t.Fatalf("unexpected diagnostic event: %#v", event)
	}
	if event.Transient || event.IndependentID != "" {
		t.Fatalf("legacy event unexpectedly marked independent: %#v", event)
	}
}

func TestEventMarksIndependentTransientFlow(t *testing.T) {
	event := Event{Type: TextDelta, Text: "working", Transient: true, IndependentID: "run-1"}
	if !event.Transient || event.IndependentID != "run-1" {
		t.Fatalf("independent marker was not retained: %#v", event)
	}

	final := Event{Type: TextDelta, Text: "summary", IndependentID: "run-1"}
	if final.Transient {
		t.Fatalf("final summary must remain persistent: %#v", final)
	}
}
