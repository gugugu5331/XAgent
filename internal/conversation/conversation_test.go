package conversation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestContextMessagesIncludeSummaryBoundaryAndExternalPreview(t *testing.T) {
	conv := NewConversation("c1", time.Now())
	AppendContextSummaryMessage(conv, "summary")
	AppendContextBoundaryMessage(conv, "boundary")
	AppendToolResultMessage(conv, "call", "Read", "success", "", "full secret details", "", false, nil, nil)
	conv.Messages[2].Externalized = true
	conv.Messages[2].ExternalPath = "/tmp/result.json"
	conv.Messages[2].ExternalBytes = 12
	conv.Messages[2].ExternalPreview = "preview"

	messages := ContextMessages(conv)
	if len(messages) != 3 {
		t.Fatalf("expected 3 context messages, got %d", len(messages))
	}
	if messages[0].Role != RoleContextSummary || messages[1].Role != RoleContextBoundary {
		t.Fatalf("missing summary/boundary roles: %#v", messages)
	}
	if !strings.Contains(messages[2].ToolResultContent, "preview") || !strings.Contains(messages[2].ToolResultContent, "artifact_id") {
		t.Fatalf("externalized result not represented safely: %#v", messages[2])
	}
	if strings.Contains(messages[2].ToolResultContent, conv.Messages[2].ExternalPath) {
		t.Fatalf("externalized context leaked path: %#v", messages[2])
	}
}

func TestOrdinaryConversationJSONLAllowsUserContentButRedactsDiagnostics(t *testing.T) {
	conv := NewConversation("c1", time.Now())
	AppendUserMessage(conv, "用户明确输入 token=ordinary-secret")
	record := JSONLRecord{
		Version:     JSONLVersion,
		Type:        RecordTypeMessage,
		SessionID:   conv.ID,
		CreatedAt:   time.Now(),
		Message:     &conv.Messages[0],
		Diagnostics: []JSONLDiagnostic{newJSONLDiagnostic("jsonl_bad_line", "坏行 token=diagnostic-secret", JSONLSeverityWarning)},
		Error:       "error api_key=error-secret",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "ordinary-secret") {
		t.Fatalf("ordinary user content should be preserved for session recovery: %s", text)
	}
	for _, secret := range []string{"diagnostic-secret", "error-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("jsonl record leaked derived secret %q: %s", secret, text)
		}
	}
}

func TestAppendToolResultMessageStoresStructuredDataAndError(t *testing.T) {
	conv := NewConversation("c1", time.Now())
	data := json.RawMessage(`{"permission_denied":true,"reason":"user_cancelled"}`)
	errorData := json.RawMessage(`{"code":"permission_denied","message":"cancelled","recoverable":true}`)
	AppendToolResultMessage(conv, "call", "Bash", "cancelled", "用户取消工具确认", `{"status":"denied"}`, "permission_denied", false, data, errorData)
	if len(conv.Messages) != 1 {
		t.Fatalf("expected one message, got %d", len(conv.Messages))
	}
	message := conv.Messages[0]
	if message.ToolResultStatus != "cancelled" || message.ToolErrorCode != "permission_denied" {
		t.Fatalf("unexpected message fields: %#v", message)
	}
	if !strings.Contains(string(message.ToolResultData), "user_cancelled") {
		t.Fatalf("missing structured data: %s", message.ToolResultData)
	}
	if !strings.Contains(string(message.ToolResultError), "permission_denied") {
		t.Fatalf("missing structured error: %s", message.ToolResultError)
	}
}
