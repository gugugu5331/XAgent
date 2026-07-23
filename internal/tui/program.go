package tui

import tea "github.com/charmbracelet/bubbletea"

type programRunner interface {
	Run() (tea.Model, error)
}

// Run returns Bubble Tea's final Model so the process-level owner can close
// resources from the state that actually handled the last event.
func Run(model tea.Model) (tea.Model, error) {
	return runProgram(tea.NewProgram(model, tea.WithAltScreen()))
}

func runProgram(program programRunner) (tea.Model, error) {
	return program.Run()
}
