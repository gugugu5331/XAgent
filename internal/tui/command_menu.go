package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type CommandMenuItem struct {
	Name        string
	Description string
	ArgHint     string
	Badge       string
}

type CommandMenu struct {
	Items     []CommandMenuItem
	Selected  int
	Visible   bool
	region    Region
	regionSet bool
}

func (m *CommandMenu) Open(items []CommandMenuItem) {
	m.Items = append([]CommandMenuItem(nil), items...)
	m.Selected = 0
	m.Visible = len(m.Items) > 0
}

func (m *CommandMenu) Close() {
	m.Items = nil
	m.Selected = 0
	m.Visible = false
}

func (m *CommandMenu) Move(delta int) {
	if !m.Visible || len(m.Items) == 0 || delta == 0 {
		return
	}
	m.Selected = (m.Selected + delta) % len(m.Items)
	if m.Selected < 0 {
		m.Selected += len(m.Items)
	}
}

func (m CommandMenu) SelectedItem() (CommandMenuItem, bool) {
	if !m.Visible || m.Selected < 0 || m.Selected >= len(m.Items) {
		return CommandMenuItem{}, false
	}
	return m.Items[m.Selected], true
}

func (m *CommandMenu) SetRegion(region Region) {
	m.region = Region{
		X: nonNegative(region.X), Y: nonNegative(region.Y),
		Width: nonNegative(region.Width), Height: nonNegative(region.Height),
	}
	m.regionSet = true
}

func (m CommandMenu) View() string {
	if !m.Visible || len(m.Items) == 0 {
		return ""
	}
	start, end := 0, len(m.Items)
	if m.regionSet {
		if m.region.Width == 0 || m.region.Height == 0 {
			return ""
		}
		maxRows := m.region.Height
		if end > maxRows {
			start = m.Selected - maxRows + 1
			if start < 0 {
				start = 0
			}
			end = start + maxRows
			if end > len(m.Items) {
				end = len(m.Items)
				start = end - maxRows
			}
		}
	}
	var builder strings.Builder
	for index := start; index < end; index++ {
		item := m.Items[index]
		prefix := "  "
		if index == m.Selected {
			prefix = "› "
		}
		line := prefix + "/" + item.Name
		if strings.TrimSpace(item.Badge) != "" {
			line += " [" + strings.TrimSpace(item.Badge) + "]"
		}
		if strings.TrimSpace(item.ArgHint) != "" {
			line += " " + item.ArgHint
		}
		if strings.TrimSpace(item.Description) != "" {
			line += " — " + item.Description
		}
		if m.regionSet {
			line = ansi.Truncate(line, m.region.Width, "")
		}
		if index == m.Selected {
			line = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render(line)
		}
		builder.WriteString(line)
		if index < end-1 {
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}
