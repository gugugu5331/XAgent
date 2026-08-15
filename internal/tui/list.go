package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"

	"xagent/internal/conversation"
)

const NewConversationID = "__new__"

type ConversationItem struct {
	ID                 string
	Name               string
	UpdatedAtUnixMilli int64
	New                bool
	Available          bool
	Selectable         bool
	RecoveryStatus     string
	RecoveryNotice     string
}

func (i ConversationItem) FilterValue() string { return i.Name }
func (i ConversationItem) Title() string       { return i.Name }
func (i ConversationItem) Description() string {
	if i.New {
		return "开始一个新会话"
	}
	prefix := "更新于"
	switch {
	case !i.Available || i.RecoveryStatus == string(conversation.RecoveryPlaceholder):
		prefix = "恢复占位 · 不可用 · 更新于"
	case i.RecoveryStatus == string(conversation.RecoveryPartial):
		prefix = "已部分恢复 · 更新于"
	}
	updatedAt := time.UnixMilli(i.UpdatedAtUnixMilli)
	description := fmt.Sprintf("%s %s", prefix, updatedAt.Format("2006-01-02 15:04:05"))
	if notice := strings.TrimSpace(i.RecoveryNotice); notice != "" {
		description += " · " + notice
	}
	return description
}

func NewConversationList(entries []conversation.ListEntry) list.Model {
	items := newConversationItems()
	for _, entry := range entries {
		selectable := entry.Available && entry.Recovery.Status != conversation.RecoveryPlaceholder
		items = append(items, ConversationItem{
			ID: entry.Summary.ID, Name: entry.Summary.Title.Text(), UpdatedAtUnixMilli: entry.Summary.UpdatedAt.UnixMilli(),
			Available: entry.Available, Selectable: selectable, RecoveryStatus: string(entry.Recovery.Status),
		})
	}
	return newConversationListModel(items, len(entries), false, "")
}

// NewSessionListModel constructs the responsive list directly from the pure
// TUI ViewModel projection. No Store or Conversation object is retained.
func NewSessionListModel(sessions SessionListView) list.Model {
	entries := sessions.Entries()
	items := newConversationItems()
	for _, entry := range entries {
		items = append(items, ConversationItem{
			ID: entry.ID(), Name: entry.Title().Text(), UpdatedAtUnixMilli: entry.UpdatedAtUnixMilli(),
			Available: entry.Available(), Selectable: entry.Selectable(),
			RecoveryStatus: entry.RecoveryStatus(), RecoveryNotice: entry.RecoveryNotice().Text(),
		})
	}
	return newConversationListModel(items, len(entries), sessions.Truncated(), sessions.Notice().Text())
}

func newConversationItems() []list.Item {
	return []list.Item{ConversationItem{
		ID:         NewConversationID,
		Name:       "新建会话",
		New:        true,
		Available:  true,
		Selectable: true,
	}}
}

func newConversationListModel(items []list.Item, historyCount int, truncated bool, notice string) list.Model {
	titleParts := []string{"XAgent 历史会话"}
	if historyCount == 0 {
		titleParts = append(titleParts, "暂无历史会话")
	}
	if truncated {
		titleParts = append(titleParts, "结果已截断")
	}
	if notice = strings.TrimSpace(notice); notice != "" {
		titleParts = append(titleParts, notice)
	}
	initial := ComputeLayout(LayoutInput{Terminal: Size{Width: BaselineWidth, Height: BaselineHeight}, Screen: string(ScreenList)})
	model := list.New(items, list.NewDefaultDelegate(), 0, 0)
	model.Title = strings.Join(titleParts, " · ")
	ApplyConversationListLayout(&model, initial)
	return model
}

// ApplyConversationListLayout resizes without rebuilding the model, so the
// current selection, filter and pagination state remain owned by Bubbles.
func ApplyConversationListLayout(model *list.Model, layout Layout) {
	if model == nil {
		return
	}
	model.SetShowHelp(layout.Mode == LayoutFull)
	frameWidth := max(
		model.Styles.TitleBar.GetHorizontalFrameSize(),
		model.Styles.StatusBar.GetHorizontalFrameSize(),
		model.Styles.PaginationStyle.GetHorizontalFrameSize(),
		model.Styles.HelpStyle.GetHorizontalFrameSize(),
	)
	model.SetSize(max(0, layout.Main.Width-frameWidth), layout.Main.Height)
}
