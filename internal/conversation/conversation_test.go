package conversation

import (
	"strings"
	"testing"
	"time"

	"xagent/internal/tool"
)

func TestContextMessagesUseOnlySafePrimaryMessages(t *testing.T) {
	conv := NewConversation("c1", time.Now())
	AppendContextSummaryMessage(conv, "summary")
	AppendContextBoundaryMessage(conv, "boundary")
	AppendToolResultMessage(conv, "call", "Read", "success", "safe summary", "safe result", "", false, nil, nil)
	messages := ContextMessages(conv)
	if len(messages) != 3 || messages[0].Role != RoleContextSummary || messages[1].Role != RoleContextBoundary {
		t.Fatalf("safe context messages = %#v", messages)
	}
	if messages[2].Tool == nil || messages[2].Tool.Result.Text() != "safe result" {
		t.Fatalf("safe tool result = %#v", messages[2])
	}
}

func TestPrimaryConversationRedactsCompatibilityTextInputs(t *testing.T) {
	conv := NewConversation("c1", time.Now())
	AppendUserMessage(conv, "ordinary input")
	if got := conv.Messages[0].Content.Text(); got != "ordinary input" {
		t.Fatalf("content = %q", got)
	}
	if strings.TrimSpace(conv.Title.Text()) == "" {
		t.Fatal("title is empty")
	}
}

func TestAppendToolResultMessageStoresSafeState(t *testing.T) {
	conv := NewConversation("c1", time.Now())
	AppendToolResultMessage(conv, "call", "Bash", "error", "cancelled", "denied", "permission_denied", false, nil, []byte(`{"message":"cancelled"}`))
	message := conv.Messages[0]
	if message.Tool == nil || message.Tool.Status != tool.StatusError || message.Tool.Error == nil || message.Tool.Error.Code != "permission_denied" {
		t.Fatalf("unexpected safe tool state: %#v", message)
	}
}
