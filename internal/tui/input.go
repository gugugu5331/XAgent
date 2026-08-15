package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type Input struct {
	Text      textarea.Model
	region    Region
	regionSet bool
}

func NewInput(prompt string) Input {
	input := textarea.New()
	input.Placeholder = prompt
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.Focus()
	input.CharLimit = 4000
	input.SetWidth(BaselineWidth)
	input.SetHeight(1)
	return Input{Text: input}
}

func (i *Input) Value() string {
	return i.Text.Value()
}

func (i *Input) Clear() {
	i.Text.SetValue("")
}

func (i *Input) SetValue(value string) {
	i.Text.SetValue(value)
	i.Text.CursorEnd()
	i.Text.Focus()
}

func (i *Input) SetEnabled(enabled bool) {
	if enabled {
		i.Text.Focus()
	} else {
		i.Text.Blur()
	}
}

// VisualLineCount returns the number of rows needed after soft wrapping at
// width. The result is always at least one and handles wide Unicode cells.
func (i Input) VisualLineCount(width int) int {
	if width < 1 {
		width = 1
	}
	count := 0
	for _, line := range strings.Split(i.Value(), "\n") {
		lineWidth := ansi.StringWidth(line)
		rows := 1
		if lineWidth > 0 {
			rows = 1 + (lineWidth-1)/width
		}
		count += rows
	}
	if count < 1 {
		return 1
	}
	return count
}

func (i *Input) SetRegion(region Region) {
	i.region = Region{
		X: nonNegative(region.X), Y: nonNegative(region.Y),
		Width: nonNegative(region.Width), Height: nonNegative(region.Height),
	}
	i.regionSet = true
	i.Text.SetWidth(max(1, i.region.Width))
	i.Text.SetHeight(max(1, i.region.Height))
}

func (i Input) Height() int {
	if i.regionSet {
		return i.region.Height
	}
	return i.Text.Height()
}

func (i *Input) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	i.Text, cmd = i.Text.Update(msg)
	return cmd
}

func (i Input) View() string {
	if i.regionSet && (i.region.Width == 0 || i.region.Height == 0) {
		return ""
	}
	return i.Text.View()
}
