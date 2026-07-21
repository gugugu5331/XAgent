package skill

import (
	"encoding/json"
	"reflect"
	"testing"

	"xagent/internal/conversation"
)

func TestRecentCompleteTurns(t *testing.T) {
	conv := &conversation.Conversation{Messages: []conversation.Message{
		{Role: conversation.RoleContextSummary, Content: "summary"},
		{Role: conversation.RoleContextBoundary, Content: "boundary"},
		{Role: conversation.RoleUser, Content: "first"},
		{Role: conversation.RoleAssistant, Content: "checking"},
		{Role: conversation.RoleToolCall, ToolCallID: "call-1", ToolName: "Read"},
		{Role: conversation.RoleToolResult, ToolCallID: "call-1", ToolName: "Read", ToolResultData: json.RawMessage(`{"ok":true}`)},
		{Role: conversation.RoleAssistant, Content: "first done"},
		{Role: conversation.RoleThinking, Content: "not in provider context"},
		{Role: conversation.RoleUser, Content: "second"},
		{Role: conversation.RoleAssistant, Content: "second done"},
		{Role: conversation.RoleUser, Content: "unfinished"},
		{Role: conversation.RoleAssistant, Content: "calling"},
		{Role: conversation.RoleToolCall, ToolCallID: "call-2", ToolName: "Bash"},
		{Role: conversation.RoleToolResult, ToolCallID: "call-2", ToolName: "Bash"},
	}}

	one := RecentCompleteTurns(conv, 1)
	wantRoles := []conversation.MessageRole{
		conversation.RoleUser,
		conversation.RoleAssistant,
	}
	if !reflect.DeepEqual(messageRoles(one), wantRoles) || one[0].Content != "second" {
		t.Fatalf("unexpected last complete turn: %#v", one)
	}
	all := RecentCompleteTurns(conv, 99)
	if len(all) != 7 || all[0].Content != "first" || all[len(all)-2].Content != "second" {
		t.Fatalf("complete turns/tool chain not preserved: %#v", all)
	}
	for _, message := range all {
		if message.Content == "unfinished" || message.Role == conversation.RoleThinking || message.Role == conversation.RoleContextSummary || message.Role == conversation.RoleContextBoundary {
			t.Fatalf("unfinished/thinking message leaked: %#v", all)
		}
	}

	all[0].Content = "mutated"
	all[3].ToolResultData[0] = 'X'
	if conv.Messages[2].Content != "first" || conv.Messages[5].ToolResultData[0] == 'X' {
		t.Fatal("returned history aliases main conversation")
	}
}

func TestRecentCompleteTurnsBoundaries(t *testing.T) {
	if RecentCompleteTurns(nil, 1) != nil {
		t.Fatal("nil conversation should return nil")
	}
	conv := &conversation.Conversation{Messages: []conversation.Message{{Role: conversation.RoleUser, Content: "user only"}}}
	if RecentCompleteTurns(conv, 0) != nil || RecentCompleteTurns(conv, -1) != nil || RecentCompleteTurns(conv, 1) != nil {
		t.Fatal("zero/negative/incomplete history should return nil")
	}
	conv.Messages = append(conv.Messages, conversation.Message{Role: conversation.RoleAssistant, Content: "done"})
	result := RecentCompleteTurns(conv, 2)
	if len(result) != 2 || result[0].Role != conversation.RoleUser || result[1].Role != conversation.RoleAssistant {
		t.Fatalf("unexpected simple turn: %#v", result)
	}
}

func messageRoles(messages []conversation.Message) []conversation.MessageRole {
	roles := make([]conversation.MessageRole, len(messages))
	for index, message := range messages {
		roles[index] = message.Role
	}
	return roles
}
