package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

type Status struct {
	Mode                     string
	Provider                 string
	Model                    string
	Streaming                bool
	WaitingConfirmation      bool
	AgentIteration           int
	AgentMaxIterations       int
	StopReason               string
	StopMessage              string
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	Duration                 time.Duration
	MCP                      string
	Notice                   string
	Error                    error
}

func (s Status) View() string {
	parts := []string{modeLabel(s.Mode), fmt.Sprintf("Provider: %s", s.Provider), fmt.Sprintf("Model: %s", s.Model)}
	if s.WaitingConfirmation {
		parts = append(parts, "等待工具确认")
	} else if s.Streaming {
		if s.AgentIteration > 0 && s.AgentMaxIterations > 0 {
			parts = append(parts, fmt.Sprintf("正在响应... 第 %d/%d 轮", s.AgentIteration, s.AgentMaxIterations))
		} else {
			parts = append(parts, "正在响应...")
		}
	}
	if s.StopReason != "" {
		message := stopReasonText(s.StopReason)
		if strings.TrimSpace(s.StopMessage) != "" {
			message = s.StopMessage
		}
		parts = append(parts, message)
	}
	if s.InputTokens > 0 || s.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("Tokens: %d in / %d out", s.InputTokens, s.OutputTokens))
	}
	if s.CacheCreationInputTokens > 0 || s.CacheReadInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("Cache: %d create / %d read", s.CacheCreationInputTokens, s.CacheReadInputTokens))
	}
	if strings.TrimSpace(s.MCP) != "" {
		parts = append(parts, "MCP: "+s.MCP)
	}
	if s.Duration > 0 {
		parts = append(parts, fmt.Sprintf("耗时: %s", s.Duration.Round(time.Millisecond)))
	}
	if strings.TrimSpace(s.Notice) != "" {
		parts = append(parts, s.Notice)
	}
	if s.Error != nil {
		parts = append(parts, "错误: "+s.Error.Error())
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(strings.Join(parts, " | "))
}

func modeLabel(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "plan") {
		return "[PLAN]"
	}
	return "[DEFAULT]"
}

func stopReasonText(reason string) string {
	switch reason {
	case "completed":
		return "已完成"
	case "max_iterations":
		return "达到迭代上限"
	case "cancelled":
		return "已取消"
	case "unknown_tool":
		return "未知工具过多"
	case "provider_error":
		return "Provider 错误"
	default:
		return reason
	}
}
