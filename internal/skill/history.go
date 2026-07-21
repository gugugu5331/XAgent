package skill

import "xagent/internal/conversation"

func RecentCompleteTurns(conv *conversation.Conversation, count int) []conversation.Message {
	if conv == nil || count <= 0 {
		return nil
	}
	messages := conversation.ContextMessages(conv)
	if len(messages) == 0 {
		return nil
	}
	type turn struct {
		messages []conversation.Message
		complete bool
	}
	turns := make([]turn, 0)
	for index := 0; index < len(messages); {
		if messages[index].Role != conversation.RoleUser {
			index++
			continue
		}
		end := index + 1
		for end < len(messages) && messages[end].Role != conversation.RoleUser {
			end++
		}
		segment := messages[index:end]
		turns = append(turns, turn{messages: segment, complete: completeTurn(segment)})
		index = end
	}
	completed := make([]turn, 0, len(turns))
	for _, candidate := range turns {
		if candidate.complete {
			completed = append(completed, candidate)
		}
	}
	if len(completed) == 0 {
		return nil
	}
	start := len(completed) - count
	if start < 0 {
		start = 0
	}
	result := make([]conversation.Message, 0)
	for _, selected := range completed[start:] {
		result = appendClonedMessages(result, selected.messages)
	}
	return result
}

func completeTurn(messages []conversation.Message) bool {
	if len(messages) == 0 || messages[0].Role != conversation.RoleUser {
		return false
	}
	for index := len(messages) - 1; index >= 1; index-- {
		switch messages[index].Role {
		case conversation.RoleAssistant:
			return true
		case conversation.RoleToolCall, conversation.RoleToolResult:
			return false
		}
	}
	return false
}

func appendClonedMessages(destination []conversation.Message, source []conversation.Message) []conversation.Message {
	for _, message := range source {
		destination = append(destination, cloneConversationMessage(message))
	}
	return destination
}

func cloneConversationMessage(message conversation.Message) conversation.Message {
	message.ToolResultData = append([]byte(nil), message.ToolResultData...)
	message.ToolResultError = append([]byte(nil), message.ToolResultError...)
	return message
}
