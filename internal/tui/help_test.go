package tui

import (
	"strings"
	"testing"
)

func TestHelpMatchesC9PublicMetadata(t *testing.T) {
	view := NewHelpView(HelpViewSpec{
		Commands: []HelpCommandSpec{
			{Name: "new", Description: "创建并切换到新会话", Usage: "/new", Type: "ui"},
			{Name: "sessions", Aliases: []string{"list"}, Description: "打开会话列表", Usage: "/sessions", Type: "ui"},
			{Name: "status", Aliases: []string{"st"}, Description: "显示统一运行状态", Usage: "/status", Type: "local"},
		},
		Shortcuts: []HelpShortcutSpec{
			{Context: "chat_idle", Key: "esc", Command: "sessions", Description: "打开会话列表"},
			{Context: "chat_streaming", Key: "esc", Command: "sessions", Description: "取消当前请求"},
			{Context: "chat_confirmation", Key: "esc", Command: "sessions", Description: "取消当前请求"},
			{Context: "sessions", Key: "enter", Command: "sessions", Description: "恢复所选会话"},
			{Context: "sessions", Key: "n", Command: "new", Description: "新建会话"},
			{Context: "sessions", Key: "q", Command: "sessions", Description: "退出应用"},
		},
		Entries: []HelpEntrySpec{
			{Kind: "permission_mode", Name: "strict", Command: "permission", Description: "写入与命令执行需要确认"},
			{Kind: "status", Name: "status", Command: "status", Description: "显示统一运行状态"},
			{Kind: "diagnostics", Name: "status", Command: "status", Description: "显示有界、脱敏的诊断摘要"},
		},
	})

	output := view.View()
	for _, want := range []string{
		"/new", "/sessions", "/list", "/status", "/st",
		"chat_idle", "chat_streaming", "chat_confirmation", "sessions",
		"esc", "enter", "n", "q", "strict", "显示有界、脱敏的诊断摘要",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("unified help missing %q: %q", want, output)
		}
	}
	for _, hidden := range []string{"/diagnostics", "/permissions", "/mcp", "/rv"} {
		if strings.Contains(output, hidden) {
			t.Fatalf("unified help exposed hidden compatibility command %q: %q", hidden, output)
		}
	}

	commands := []HelpCommandSpec{{Name: "safe\x1b]52;c;clipboard\a", Description: "line\x1b[2J\x00", Usage: "/safe", Type: "local"}}
	copyView := NewHelpView(HelpViewSpec{Commands: commands})
	commands[0].Name = "mutated"
	controlled := copyView.View()
	if strings.Contains(controlled, "mutated") || strings.ContainsAny(controlled, "\x1b\a\x00") || strings.Contains(controlled, "52;c;clipboard") {
		t.Fatalf("help retained mutable input or terminal controls: %q", controlled)
	}
}
