package tui

import (
	"strings"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/redact"
)

func TestConversationListPreservesRecoveryAvailability(t *testing.T) {
	runtimeRedactor := redact.NewRuntimeRedactor()
	model := NewConversationList([]conversation.ListEntry{
		{
			Summary:   conversation.ConversationSummary{ID: "partial", Title: runtimeRedactor.Redact("Partial session"), UpdatedAt: time.Unix(2, 0)},
			Available: true,
			Recovery:  conversation.RecoveryReport{Status: conversation.RecoveryPartial},
		},
		{
			Summary:   conversation.ConversationSummary{ID: "placeholder", Title: runtimeRedactor.Redact("Unavailable session"), UpdatedAt: time.Unix(1, 0)},
			Available: false,
			Recovery:  conversation.RecoveryReport{Status: conversation.RecoveryPlaceholder},
		},
	})

	if got := len(model.Items()); got != 3 {
		t.Fatalf("list items = %d, want new plus two persisted entries", got)
	}
	partial, ok := model.Items()[1].(ConversationItem)
	if !ok || !partial.Available || partial.RecoveryStatus != string(conversation.RecoveryPartial) || !strings.Contains(partial.Description(), "部分恢复") {
		t.Fatalf("partial entry lost recovery metadata: %#v", model.Items()[1])
	}
	placeholder, ok := model.Items()[2].(ConversationItem)
	if !ok || placeholder.Available || placeholder.RecoveryStatus != string(conversation.RecoveryPlaceholder) || !strings.Contains(placeholder.Description(), "不可用") {
		t.Fatalf("placeholder entry lost availability metadata: %#v", model.Items()[2])
	}
}
