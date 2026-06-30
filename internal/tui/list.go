package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/list"

	"xagent/internal/conversation"
)

const NewConversationID = "__new__"

type ConversationItem struct {
	ID        string
	Name      string
	UpdatedAt time.Time
	New       bool
}

func (i ConversationItem) FilterValue() string { return i.Name }
func (i ConversationItem) Title() string       { return i.Name }
func (i ConversationItem) Description() string {
	if i.New {
		return "开始一个新会话"
	}
	return fmt.Sprintf("更新于 %s", i.UpdatedAt.Format("2006-01-02 15:04:05"))
}

func NewConversationList(conversations []conversation.Conversation) list.Model {
	items := []list.Item{ConversationItem{ID: NewConversationID, Name: "新建会话", New: true}}
	for _, conv := range conversations {
		items = append(items, ConversationItem{ID: conv.ID, Name: conv.Title, UpdatedAt: conv.UpdatedAt})
	}
	model := list.New(items, list.NewDefaultDelegate(), 80, 20)
	model.Title = "XAgent 历史会话"
	return model
}
