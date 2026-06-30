package tui

import "github.com/charmbracelet/bubbles/textinput"

type Input struct {
	Text textinput.Model
}

func NewInput(prompt string) Input {
	input := textinput.New()
	input.Placeholder = prompt
	input.Focus()
	input.CharLimit = 4000
	input.Width = 80
	return Input{Text: input}
}

func (i *Input) Value() string {
	return i.Text.Value()
}

func (i *Input) Clear() {
	i.Text.SetValue("")
}

func (i *Input) SetEnabled(enabled bool) {
	if enabled {
		i.Text.Focus()
	} else {
		i.Text.Blur()
	}
}
