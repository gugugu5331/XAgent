package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type Status struct {
	Mode                     string
	Provider                 string
	Model                    string
	ShowResponseTimer        bool
	ActiveSkills             string
	RequestModel             string
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
	TaskCount                int
	RunningTasks             int
	WaitingTaskConfirmations int
	Duration                 time.Duration
	MCP                      string
	Notice                   string
	Error                    error
	region                   Region
	regionSet                bool
}

func (s Status) View() string {
	if s.regionSet {
		return s.responsiveView()
	}
	parts := []string{modeLabel(s.Mode), fmt.Sprintf("Provider: %s", s.Provider), fmt.Sprintf("Model: %s", s.Model)}
	if strings.TrimSpace(s.ActiveSkills) != "" {
		parts = append(parts, "Skills: "+strings.TrimSpace(s.ActiveSkills))
	}
	if strings.TrimSpace(s.RequestModel) != "" {
		parts = append(parts, "Request model: "+strings.TrimSpace(s.RequestModel))
	}
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
	if s.TaskCount > 0 {
		parts = append(parts, fmt.Sprintf("任务: %d", s.TaskCount))
	}
	if s.RunningTasks > 0 {
		parts = append(parts, fmt.Sprintf("运行中: %d", s.RunningTasks))
	}
	if s.WaitingTaskConfirmations > 0 {
		parts = append(parts, fmt.Sprintf("等待任务确认: %d", s.WaitingTaskConfirmations))
	}
	if strings.TrimSpace(s.MCP) != "" {
		parts = append(parts, "MCP: "+s.MCP)
	}
	if s.ShowResponseTimer && s.Duration > 0 {
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

func (s *Status) SetRegion(region Region) {
	s.region = Region{
		X: nonNegative(region.X), Y: nonNegative(region.Y),
		Width: nonNegative(region.Width), Height: nonNegative(region.Height),
	}
	s.regionSet = true
}

// ResetRequest clears values whose lifetime is one request while preserving
// resolved UI configuration and conversation/runtime identity.
func (s *Status) ResetRequest() {
	if s == nil {
		return
	}
	s.RequestModel = ""
	s.Streaming = false
	s.WaitingConfirmation = false
	s.AgentIteration = 0
	s.AgentMaxIterations = 0
	s.StopReason = ""
	s.StopMessage = ""
	s.InputTokens = 0
	s.OutputTokens = 0
	s.CacheCreationInputTokens = 0
	s.CacheReadInputTokens = 0
	s.Duration = 0
	s.Error = nil
}

func (s Status) responsiveView() string {
	if s.region.Width == 0 || s.region.Height == 0 {
		return ""
	}
	critical := make([]string, 0, 4)
	if s.Error != nil {
		critical = append(critical, "错误: "+s.Error.Error())
	}
	if s.WaitingConfirmation {
		critical = append(critical, "等待工具确认")
	} else if s.Streaming {
		if s.AgentIteration > 0 && s.AgentMaxIterations > 0 {
			critical = append(critical, fmt.Sprintf("正在响应... 第 %d/%d 轮", s.AgentIteration, s.AgentMaxIterations))
		} else {
			critical = append(critical, "正在响应...")
		}
	}
	if s.StopReason != "" {
		message := stopReasonText(s.StopReason)
		if strings.TrimSpace(s.StopMessage) != "" {
			message = s.StopMessage
		}
		critical = append(critical, message)
	}
	if strings.TrimSpace(s.Notice) != "" {
		critical = append(critical, s.Notice)
	}

	identity := []string{modeLabel(s.Mode), fmt.Sprintf("Provider: %s", s.Provider), fmt.Sprintf("Model: %s", s.Model)}
	if strings.TrimSpace(s.ActiveSkills) != "" {
		identity = append(identity, "Skills: "+strings.TrimSpace(s.ActiveSkills))
	}
	if strings.TrimSpace(s.RequestModel) != "" {
		identity = append(identity, "Request model: "+strings.TrimSpace(s.RequestModel))
	}

	metrics := make([]string, 0, 4)
	if s.InputTokens > 0 || s.OutputTokens > 0 {
		metrics = append(metrics, fmt.Sprintf("Tokens: %d in / %d out", s.InputTokens, s.OutputTokens))
	}
	if s.CacheCreationInputTokens > 0 || s.CacheReadInputTokens > 0 {
		metrics = append(metrics, fmt.Sprintf("Cache: %d create / %d read", s.CacheCreationInputTokens, s.CacheReadInputTokens))
	}
	if s.TaskCount > 0 {
		metrics = append(metrics, fmt.Sprintf("任务: %d", s.TaskCount))
	}
	if s.RunningTasks > 0 {
		metrics = append(metrics, fmt.Sprintf("运行中: %d", s.RunningTasks))
	}
	if s.WaitingTaskConfirmations > 0 {
		critical = append(critical, fmt.Sprintf("等待任务确认: %d", s.WaitingTaskConfirmations))
	}
	if strings.TrimSpace(s.MCP) != "" {
		metrics = append(metrics, "MCP: "+s.MCP)
	}
	if s.ShowResponseTimer && s.Duration > 0 {
		metrics = append(metrics, fmt.Sprintf("耗时: %s", s.Duration.Round(time.Millisecond)))
	}

	parts := append(append(critical, identity...), metrics...)
	line := ansi.Truncate(strings.Join(parts, " | "), s.region.Width, "")
	return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(line)
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
