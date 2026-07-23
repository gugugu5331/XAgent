package tui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type finalModel struct{ value string }

func (m finalModel) Init() tea.Cmd                       { return nil }
func (m finalModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (m finalModel) View() string                        { return m.value }

type recordingProgram struct {
	model tea.Model
	err   error
}

func (p recordingProgram) Run() (tea.Model, error) { return p.model, p.err }

func TestRunReturnsFinalModel(t *testing.T) {
	want := finalModel{value: "final"}

	t.Run("normal", func(t *testing.T) {
		got, err := runProgram(recordingProgram{model: want})
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("final model = %#v, want %#v", got, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		wantErr := errors.New("renderer failed")
		got, err := runProgram(recordingProgram{model: want, err: wantErr})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
		if got != want {
			t.Fatalf("final model = %#v, want %#v", got, want)
		}
	})
}

func TestNoHookVisibleCompatibility(t *testing.T) {
	want := finalModel{value: "conversation\n[DEFAULT] provider=fake model=fake\ninput"}
	var baseline string
	for _, name := range []string{"nil", "noop", "empty-engine"} {
		got, err := runProgram(recordingProgram{model: want})
		if err != nil {
			t.Fatalf("%s run: %v", name, err)
		}
		visible := got.View()
		if visible != want.View() {
			t.Fatalf("%s final view = %q, want %q", name, visible, want.View())
		}
		if baseline == "" {
			baseline = visible
		} else if visible != baseline {
			t.Fatalf("%s changed final TUI view: legacy=%q actual=%q", name, baseline, visible)
		}
	}
}
