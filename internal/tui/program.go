package tui

import tea "github.com/charmbracelet/bubbletea"

type programRunner interface {
	Run() (tea.Model, error)
}

// responsiveModel retains only the last size actually reported by Bubble Tea.
// It brackets every later message with that size so App can apply layout before
// dispatch and recompute it after state changes. No guessed size is synthesized.
type responsiveModel struct {
	model       tea.Model
	terminal    tea.WindowSizeMsg
	hasTerminal bool
}

func newResponsiveModel(model tea.Model) tea.Model {
	return responsiveModel{model: model}
}

func (model responsiveModel) Init() tea.Cmd {
	return model.model.Init()
}

func (model responsiveModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		model.terminal = size
		model.hasTerminal = true
		updated, cmd := model.model.Update(size)
		model.model = updated
		return model, cmd
	}
	if !model.hasTerminal {
		updated, cmd := model.model.Update(msg)
		model.model = updated
		return model, cmd
	}

	before, beforeCmd := model.model.Update(model.terminal)
	updated, updateCmd := before.Update(msg)
	after, afterCmd := updated.Update(model.terminal)
	model.model = after
	return model, tea.Batch(beforeCmd, updateCmd, afterCmd)
}

func (model responsiveModel) View() string {
	return model.model.View()
}

func unwrapResponsiveModel(model tea.Model) tea.Model {
	if responsive, ok := model.(responsiveModel); ok {
		return responsive.model
	}
	return model
}

// Run returns Bubble Tea's final Model so the process-level owner can close
// resources from the state that actually handled the last event.
func Run(model tea.Model) (tea.Model, error) {
	final, err := runProgram(tea.NewProgram(newResponsiveModel(model), tea.WithAltScreen()))
	return unwrapResponsiveModel(final), err
}

func runProgram(program programRunner) (tea.Model, error) {
	return program.Run()
}
