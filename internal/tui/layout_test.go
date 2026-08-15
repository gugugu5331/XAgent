package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"xagent/internal/conversation"
)

func TestComputeLayoutForBaselineWideAndCompact(t *testing.T) {
	t.Run("80x24 full chat", func(t *testing.T) {
		layout := ComputeLayout(LayoutInput{
			Terminal:   Size{Width: 80, Height: 24},
			InputLines: 1,
			Screen:     string(ScreenChat),
		})

		if layout.Mode != LayoutFull {
			t.Fatalf("mode = %q, want %q", layout.Mode, LayoutFull)
		}
		if layout.Terminal != (Size{Width: 80, Height: 24}) {
			t.Fatalf("terminal = %#v", layout.Terminal)
		}
		if layout.Main.Width != 80 || layout.Main.Height <= 0 {
			t.Fatalf("main = %#v, want full-width visible content", layout.Main)
		}
		if layout.Input.Height != 1 || layout.Status.Height != 1 {
			t.Fatalf("input/status = %#v/%#v, want one row each", layout.Input, layout.Status)
		}
		if layout.Confirmation.Height != 0 || layout.CommandMenu.Height != 0 {
			t.Fatalf("hidden panels consumed space: confirmation=%#v menu=%#v", layout.Confirmation, layout.CommandMenu)
		}
		assertLayoutPartition(t, layout)
	})

	t.Run("80x24 full chat keeps requested panels visible", func(t *testing.T) {
		layout := ComputeLayout(LayoutInput{
			Terminal:         Size{Width: 80, Height: 24},
			InputLines:       int(^uint(0) >> 1),
			ShowConfirmation: true,
			ShowCommandMenu:  true,
			Screen:           string(ScreenChat),
		})

		if layout.Mode != LayoutFull || layout.Main.Height <= 0 || layout.Input.Height <= 0 ||
			layout.Input.Height > FullInputMaxHeight || layout.Confirmation.Height <= 0 ||
			layout.CommandMenu.Height <= 0 || layout.Status.Height <= 0 {
			t.Fatalf("80x24 full layout lost a requested region: %#v", layout)
		}
		assertLayoutPartition(t, layout)
	})

	t.Run("wide content is centered and bounded", func(t *testing.T) {
		layout := ComputeLayout(LayoutInput{
			Terminal:         Size{Width: 240, Height: 40},
			InputLines:       4,
			ShowConfirmation: true,
			ShowCommandMenu:  true,
			Screen:           string(ScreenChat),
		})

		if layout.Mode != LayoutFull {
			t.Fatalf("mode = %q, want %q", layout.Mode, LayoutFull)
		}
		for _, named := range layoutRegions(layout) {
			if named.region.Width != MaxReadableWidth || named.region.X != 60 {
				t.Fatalf("%s = %#v, want centered width %d", named.name, named.region, MaxReadableWidth)
			}
		}
		if layout.Main.Height <= 0 || layout.Input.Height != 4 || layout.Confirmation.Height <= 0 ||
			layout.CommandMenu.Height <= 0 || layout.Status.Height != 1 {
			t.Fatalf("wide layout lost a required region: %#v", layout)
		}
		assertLayoutPartition(t, layout)
	})

	t.Run("either dimension below baseline is compact", func(t *testing.T) {
		for _, terminal := range []Size{{Width: 79, Height: 24}, {Width: 80, Height: 23}} {
			layout := ComputeLayout(LayoutInput{Terminal: terminal, InputLines: 1, Screen: string(ScreenChat)})
			if layout.Mode != LayoutCompact {
				t.Fatalf("terminal %#v mode = %q, want %q", terminal, layout.Mode, LayoutCompact)
			}
			assertLayoutPartition(t, layout)
		}
	})

	t.Run("panel flags are independent", func(t *testing.T) {
		for _, test := range []struct {
			name             string
			showConfirmation bool
			showCommandMenu  bool
		}{
			{name: "confirmation", showConfirmation: true},
			{name: "command menu", showCommandMenu: true},
		} {
			layout := ComputeLayout(LayoutInput{
				Terminal: Size{Width: 80, Height: 24}, InputLines: 1,
				ShowConfirmation: test.showConfirmation, ShowCommandMenu: test.showCommandMenu,
				Screen: string(ScreenChat),
			})
			if (layout.Confirmation.Height > 0) != test.showConfirmation ||
				(layout.CommandMenu.Height > 0) != test.showCommandMenu {
				t.Fatalf("%s flags projected incorrectly: %#v", test.name, layout)
			}
			assertLayoutPartition(t, layout)
		}
	})

	t.Run("chat input has a one-row minimum", func(t *testing.T) {
		for _, inputLines := range []int{0, -1} {
			layout := ComputeLayout(LayoutInput{
				Terminal: Size{Width: 80, Height: 24}, InputLines: inputLines, Screen: string(ScreenChat),
			})
			if layout.Input.Height != 1 {
				t.Fatalf("input lines %d produced height %d, want 1", inputLines, layout.Input.Height)
			}
			assertLayoutPartition(t, layout)
		}
	})

	t.Run("below baseline is compact", func(t *testing.T) {
		layout := ComputeLayout(LayoutInput{
			Terminal:         Size{Width: 79, Height: 23},
			InputLines:       20,
			ShowConfirmation: true,
			ShowCommandMenu:  true,
			Screen:           string(ScreenChat),
		})

		if layout.Mode != LayoutCompact {
			t.Fatalf("mode = %q, want %q", layout.Mode, LayoutCompact)
		}
		if layout.Input.Height > CompactInputMaxHeight || layout.Confirmation.Height <= 0 ||
			layout.CommandMenu.Height <= 0 || layout.Status.Height <= 0 || layout.Main.Height <= 0 {
			t.Fatalf("compact layout allocation = %#v", layout)
		}
		assertLayoutPartition(t, layout)
	})

	t.Run("list uses the main and status regions", func(t *testing.T) {
		layout := ComputeLayout(LayoutInput{
			Terminal:         Size{Width: 80, Height: 24},
			InputLines:       int(^uint(0) >> 1),
			ShowConfirmation: true,
			ShowCommandMenu:  true,
			Screen:           string(ScreenList),
		})

		if layout.Input.Height != 0 || layout.Confirmation.Height != 0 || layout.CommandMenu.Height != 0 ||
			layout.Main.Height != 23 || layout.Status.Height != 1 {
			t.Fatalf("list layout = %#v", layout)
		}
		assertLayoutPartition(t, layout)
	})

	t.Run("pathological sizes remain bounded", func(t *testing.T) {
		maxInt := int(^uint(0) >> 1)
		inputs := []LayoutInput{
			{Terminal: Size{Width: -1, Height: -1}, InputLines: -1, Screen: string(ScreenChat)},
			{Terminal: Size{Width: -1, Height: 24}, InputLines: 1, Screen: string(ScreenChat)},
			{Terminal: Size{Width: 80, Height: -1}, InputLines: 1, Screen: string(ScreenChat)},
			{Terminal: Size{}, InputLines: 1, ShowConfirmation: true, ShowCommandMenu: true, Screen: string(ScreenChat)},
			{Terminal: Size{Height: 24}, InputLines: 1, Screen: string(ScreenChat)},
			{Terminal: Size{Width: 80}, InputLines: 1, Screen: string(ScreenChat)},
			{Terminal: Size{Width: 1, Height: 1}, InputLines: 1, ShowConfirmation: true, ShowCommandMenu: true, Screen: string(ScreenChat)},
			{Terminal: Size{Width: 8, Height: 3}, InputLines: maxInt, ShowConfirmation: true, ShowCommandMenu: true, Screen: string(ScreenChat)},
			{Terminal: Size{Width: 200, Height: 2}, InputLines: 50, ShowConfirmation: true, Screen: string(ScreenChat)},
			{Terminal: Size{Width: maxInt, Height: maxInt}, InputLines: maxInt, ShowConfirmation: true, ShowCommandMenu: true, Screen: string(ScreenChat)},
		}
		for _, input := range inputs {
			layout := ComputeLayout(input)
			if repeated := ComputeLayout(input); repeated != layout {
				t.Fatalf("layout is not deterministic: first=%#v repeated=%#v", layout, repeated)
			}
			assertLayoutPartition(t, layout)
		}
	})
}

func TestLongMessagesAndInputRemainOperable(t *testing.T) {
	t.Run("message viewport preserves explicit anchors", func(t *testing.T) {
		layout := ComputeLayout(LayoutInput{
			Terminal: Size{Width: 80, Height: 24}, InputLines: 1, Screen: string(ScreenChat),
		})
		view := NewMessagesView(false)
		view.SetRegion(layout.Main)

		messages := make([]conversation.Message, 60)
		for index := range messages {
			messages[index] = conversation.Message{
				Role: conversation.RoleUser, Content: safeDisplayText(fmt.Sprintf("history-%02d %s", index, strings.Repeat("content ", 4))),
			}
		}
		view.SetMessages(messages)
		if !view.AtBottom() || view.TotalLineCount() <= view.VisibleLineCount() || !strings.Contains(view.View(), "history-59") {
			t.Fatalf("initial history did not reset to bottom: offset=%d total=%d visible=%d view=%q", view.YOffset(), view.TotalLineCount(), view.VisibleLineCount(), view.View())
		}

		view.AppendAssistantDelta("followed-tail")
		view.CommitAssistant()
		if !view.AtBottom() || !strings.Contains(view.View(), "followed-tail") {
			t.Fatalf("bottom anchor did not follow append: offset=%d view=%q", view.YOffset(), view.View())
		}

		view.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		if view.AtBottom() {
			t.Fatal("page up did not leave the bottom anchor")
		}
		scrolledOffset := view.YOffset()
		view.AppendAssistantDelta("background-tail")
		view.CommitAssistant()
		if view.YOffset() != scrolledOffset || strings.Contains(view.View(), "background-tail") {
			t.Fatalf("append moved a user-controlled anchor: before=%d after=%d view=%q", scrolledOffset, view.YOffset(), view.View())
		}
		view.Update(tea.KeyMsg{Type: tea.KeyEnd})
		if !view.AtBottom() || !strings.Contains(view.View(), "background-tail") {
			t.Fatalf("end did not restore the bottom anchor: offset=%d view=%q", view.YOffset(), view.View())
		}
		view.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		if view.AtBottom() {
			t.Fatal("page up after returning to the tail did not restore a user-controlled anchor")
		}
		scrolledOffset = view.YOffset()

		compact := ComputeLayout(LayoutInput{
			Terminal: Size{Width: 60, Height: 16}, InputLines: 1, Screen: string(ScreenChat),
		})
		view.SetRegion(compact.Main)
		if view.YOffset() != scrolledOffset {
			t.Fatalf("resize moved a user-controlled anchor: before=%d after=%d", scrolledOffset, view.YOffset())
		}
		if width := widestRenderedLine(view.View()); width > compact.Main.Width {
			t.Fatalf("compact message viewport rendered width %d beyond region %d", width, compact.Main.Width)
		}

		replacement := make([]conversation.Message, 30)
		for index := range replacement {
			replacement[index] = conversation.Message{
				Role: conversation.RoleAssistant, Content: safeDisplayText(fmt.Sprintf("replacement-%02d", index)),
			}
		}
		view.SetMessages(replacement)
		if !view.AtBottom() || !strings.Contains(view.View(), "replacement-29") || strings.Contains(view.View(), "history-59") {
			t.Fatalf("conversation switch did not reset the anchor and content: offset=%d view=%q", view.YOffset(), view.View())
		}

		wide := ComputeLayout(LayoutInput{
			Terminal: Size{Width: 180, Height: 32}, InputLines: 1, Screen: string(ScreenChat),
		})
		view.SetRegion(wide.Main)
		if !view.AtBottom() || !strings.Contains(view.View(), "replacement-29") {
			t.Fatalf("bottom anchor was lost on resize: offset=%d view=%q", view.YOffset(), view.View())
		}
	})

	t.Run("long input grows within full and compact budgets", func(t *testing.T) {
		input := NewInput("输入消息")
		input.SetValue(strings.Repeat("长输入", 120))

		fullLines := input.VisualLineCount(80)
		full := ComputeLayout(LayoutInput{
			Terminal: Size{Width: 80, Height: 24}, InputLines: fullLines,
			ShowConfirmation: true, Screen: string(ScreenChat),
		})
		if full.Input.Height <= 1 || full.Input.Height > FullInputMaxHeight || full.Confirmation.Height <= 0 || full.Status.Height <= 0 || full.Main.Height <= 0 {
			t.Fatalf("full layout lost long-input or critical regions: lines=%d layout=%#v", fullLines, full)
		}

		compactLines := input.VisualLineCount(40)
		compact := ComputeLayout(LayoutInput{
			Terminal: Size{Width: 40, Height: 12}, InputLines: compactLines,
			ShowConfirmation: true, Screen: string(ScreenChat),
		})
		input.SetRegion(compact.Input)
		if input.Height() != compact.Input.Height || input.Height() > CompactInputMaxHeight ||
			compact.Confirmation.Height <= 0 || compact.Status.Height <= 0 || compact.Main.Height <= 0 {
			t.Fatalf("compact input allocation is inoperable: lines=%d height=%d layout=%#v", compactLines, input.Height(), compact)
		}

		before := input.Value()
		input.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("终")})
		if input.Value() != before+"终" || strings.TrimSpace(input.View()) == "" {
			t.Fatalf("compact input is not editable/submittable: before=%q after=%q view=%q", before, input.Value(), input.View())
		}

		multiline := NewInput("输入消息")
		multiline.SetValue("第一行\n第二行")
		multiline.SetRegion(Region{Width: 40, Height: 3})
		multiline.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("终")})
		if multiline.VisualLineCount(40) != 2 || multiline.Value() != "第一行\n第二行终" || strings.TrimSpace(multiline.View()) == "" {
			t.Fatalf("multiline draft is not retained and editable: lines=%d value=%q view=%q", multiline.VisualLineCount(40), multiline.Value(), multiline.View())
		}
	})

	t.Run("zero sized regions hide components", func(t *testing.T) {
		view := NewMessagesView(false)
		view.SetMessages([]conversation.Message{{Role: conversation.RoleUser, Content: safeDisplayText("hidden")}})
		view.SetRegion(Region{X: -1, Y: -1, Width: -1, Height: -1})
		if output := view.View(); output != "" {
			t.Fatalf("zero-sized message region remained visible: offset=%d view=%q", view.YOffset(), output)
		}

		input := NewInput("输入消息")
		input.SetValue("hidden")
		input.SetRegion(Region{X: -1, Y: -1, Width: -1, Height: -1})
		if input.Height() != 0 || input.View() != "" || input.VisualLineCount(0) < 1 {
			t.Fatalf("zero-sized input region is invalid: height=%d lines=%d view=%q", input.Height(), input.VisualLineCount(0), input.View())
		}
	})
}

func widestRenderedLine(value string) int {
	widest := 0
	for _, line := range strings.Split(value, "\n") {
		widest = max(widest, ansi.StringWidth(line))
	}
	return widest
}

func TestListStatusAndMenuResizeWithoutStateLoss(t *testing.T) {
	t.Run("list keeps selection and distinguishes recovery states", func(t *testing.T) {
		sessions := NewStateViewModel(ViewModelSpec{Sessions: SessionListViewSpec{
			Entries: []SessionListEntrySpec{
				{ID: "partial", Title: safeDisplayText("Partial session"), UpdatedAtUnixMilli: 2_000, MessageCount: 2, Available: true, Selectable: true, RecoveryStatus: "partial", RecoveryNotice: safeDisplayText("已恢复可信前缀")},
				{ID: "placeholder", Title: safeDisplayText("Unavailable session"), UpdatedAtUnixMilli: 1_000, Available: false, Selectable: false, RecoveryStatus: "placeholder", RecoveryNotice: safeDisplayText("文件不可恢复")},
			},
			Truncated: true,
			Notice:    safeDisplayText("扫描预算已用完"),
		}}).Sessions()
		model := NewSessionListModel(sessions)
		model.Select(1)

		for _, terminal := range []Size{{Width: 180, Height: 32}, {Width: 40, Height: 12}, {Width: 80, Height: 24}} {
			layout := ComputeLayout(LayoutInput{Terminal: terminal, Screen: string(ScreenList)})
			ApplyConversationListLayout(&model, layout)
			selected, ok := model.SelectedItem().(ConversationItem)
			if !ok || selected.ID != "partial" {
				t.Fatalf("terminal %#v lost selected session: %#v", terminal, model.SelectedItem())
			}
			if width := lipgloss.Width(model.View()); width > layout.Main.Width {
				t.Fatalf("terminal %#v list width %d exceeds region %d: %q", terminal, width, layout.Main.Width, model.View())
			}
		}

		partial := model.Items()[1].(ConversationItem)
		placeholder := model.Items()[2].(ConversationItem)
		if !strings.Contains(partial.Description(), "部分恢复") || !strings.Contains(partial.Description(), "已恢复可信前缀") {
			t.Fatalf("partial entry is ambiguous: %q", partial.Description())
		}
		if !strings.Contains(placeholder.Description(), "恢复占位") || !strings.Contains(placeholder.Description(), "不可用") || placeholder.Selectable {
			t.Fatalf("placeholder entry is ambiguous or selectable: %#v description=%q", placeholder, placeholder.Description())
		}
		if !strings.Contains(model.Title, "结果已截断") || !strings.Contains(model.Title, "扫描预算已用完") {
			t.Fatalf("truncated list notice is not visible: %q", model.Title)
		}

		empty := NewSessionListModel(NewStateViewModel(ViewModelSpec{}).Sessions())
		if len(empty.Items()) != 1 || !strings.Contains(empty.Title, "暂无历史会话") {
			t.Fatalf("empty history is not distinguishable: title=%q items=%#v", empty.Title, empty.Items())
		}
	})

	t.Run("status keeps critical state visible", func(t *testing.T) {
		status := Status{
			Mode: "plan", Provider: "fake", Model: "model", WaitingConfirmation: true,
			Notice: "可恢复操作", Error: errors.New("critical resize error"),
			InputTokens: 13, OutputTokens: 8,
		}
		for _, terminal := range []Size{{Width: 180, Height: 32}, {Width: 40, Height: 12}, {Width: 80, Height: 24}} {
			layout := ComputeLayout(LayoutInput{Terminal: terminal, InputLines: 1, Screen: string(ScreenChat)})
			status.SetRegion(layout.Status)
			output := status.View()
			if !strings.Contains(output, "错误: critical resize error") {
				t.Fatalf("terminal %#v hid critical error: %q", terminal, output)
			}
			if width := lipgloss.Width(output); width > layout.Status.Width {
				t.Fatalf("terminal %#v status width %d exceeds region %d: %q", terminal, width, layout.Status.Width, output)
			}
		}
	})

	t.Run("command menu keeps focused item visible", func(t *testing.T) {
		items := make([]CommandMenuItem, 12)
		for index := range items {
			items[index] = CommandMenuItem{Name: fmt.Sprintf("command-%02d", index), Description: strings.Repeat("detail ", 10)}
		}
		var menu CommandMenu
		menu.Open(items)
		menu.Move(9)

		for _, terminal := range []Size{{Width: 180, Height: 32}, {Width: 40, Height: 12}, {Width: 80, Height: 24}} {
			layout := ComputeLayout(LayoutInput{Terminal: terminal, InputLines: 1, ShowCommandMenu: true, Screen: string(ScreenChat)})
			menu.SetRegion(layout.CommandMenu)
			selected, ok := menu.SelectedItem()
			if !ok || selected.Name != "command-09" || !strings.Contains(menu.View(), "› /command-09") {
				t.Fatalf("terminal %#v lost command focus: selected=%#v view=%q", terminal, selected, menu.View())
			}
			if width := lipgloss.Width(menu.View()); width > layout.CommandMenu.Width {
				t.Fatalf("terminal %#v menu width %d exceeds region %d", terminal, width, layout.CommandMenu.Width)
			}
			if height := lipgloss.Height(menu.View()); height > layout.CommandMenu.Height {
				t.Fatalf("terminal %#v menu height %d exceeds region %d", terminal, height, layout.CommandMenu.Height)
			}
		}
	})
}

func assertLayoutPartition(t *testing.T, layout Layout) {
	t.Helper()
	if layout.Terminal.Width < 0 || layout.Terminal.Height < 0 {
		t.Fatalf("negative normalized terminal: %#v", layout.Terminal)
	}
	previousBottom := 0
	totalHeight := 0
	expectedWidth := min(layout.Terminal.Width, MaxReadableWidth)
	expectedX := (layout.Terminal.Width - expectedWidth) / 2
	for _, named := range layoutRegions(layout) {
		region := named.region
		if region.X < 0 || region.Y < 0 || region.Width < 0 || region.Height < 0 {
			t.Fatalf("%s has negative geometry: %#v", named.name, region)
		}
		if region.X > layout.Terminal.Width || region.Width > layout.Terminal.Width-region.X ||
			region.Y > layout.Terminal.Height || region.Height > layout.Terminal.Height-region.Y {
			t.Fatalf("%s exceeds terminal %#v: %#v", named.name, layout.Terminal, region)
		}
		if region.Width != expectedWidth || region.X != expectedX {
			t.Fatalf("%s does not use centered readable width %d at x=%d: %#v", named.name, expectedWidth, expectedX, region)
		}
		if region.Y != previousBottom {
			t.Fatalf("%s starts at %d after previous bottom %d", named.name, region.Y, previousBottom)
		}
		previousBottom = region.Bottom()
		totalHeight += region.Height
	}
	if totalHeight != layout.Terminal.Height || previousBottom != layout.Terminal.Height {
		t.Fatalf("regions consume height %d and end at %d, terminal height %d", totalHeight, previousBottom, layout.Terminal.Height)
	}
	leftGutter := expectedX
	rightGutter := layout.Terminal.Width - (expectedX + expectedWidth)
	if leftGutter-rightGutter > 1 || rightGutter-leftGutter > 1 {
		t.Fatalf("content is not centered: left=%d right=%d layout=%#v", leftGutter, rightGutter, layout)
	}
}

type namedRegion struct {
	name   string
	region Region
}

func layoutRegions(layout Layout) []namedRegion {
	return []namedRegion{
		{name: "main", region: layout.Main},
		{name: "command menu", region: layout.CommandMenu},
		{name: "confirmation", region: layout.Confirmation},
		{name: "input", region: layout.Input},
		{name: "status", region: layout.Status},
	}
}
