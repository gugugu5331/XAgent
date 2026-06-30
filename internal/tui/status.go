package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

type Status struct {
	Provider  string
	Model     string
	Streaming bool
	Duration  time.Duration
	Error     error
}

func (s Status) View() string {
	parts := []string{fmt.Sprintf("Provider: %s", s.Provider), fmt.Sprintf("Model: %s", s.Model)}
	if s.Streaming {
		parts = append(parts, "正在响应...")
	}
	if s.Duration > 0 {
		parts = append(parts, fmt.Sprintf("耗时: %s", s.Duration.Round(time.Millisecond)))
	}
	if s.Error != nil {
		parts = append(parts, "错误: "+s.Error.Error())
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(strings.Join(parts, " | "))
}
