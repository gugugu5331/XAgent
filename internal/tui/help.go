package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// HelpCommandSpec is a presentation-only public command projection.
type HelpCommandSpec struct {
	Name        string
	Aliases     []string
	Description string
	Usage       string
	Type        string
	ArgHint     string
	Badge       string
}

// HelpShortcutSpec describes one contextual public key binding.
type HelpShortcutSpec struct {
	Context     string
	Key         string
	Command     string
	Description string
}

// HelpEntrySpec describes permission modes and public status/diagnostics
// discovery without carrying a registry or command handler.
type HelpEntrySpec struct {
	Kind        string
	Name        string
	Command     string
	Description string
}

type HelpViewSpec struct {
	Commands  []HelpCommandSpec
	Shortcuts []HelpShortcutSpec
	Entries   []HelpEntrySpec
}

// HelpView owns only immutable strings copied from command metadata.
type HelpView struct {
	commands  []HelpCommandSpec
	shortcuts []HelpShortcutSpec
	entries   []HelpEntrySpec
}

func NewHelpView(spec HelpViewSpec) HelpView {
	commands := make([]HelpCommandSpec, len(spec.Commands))
	for index, item := range spec.Commands {
		item.Aliases = append([]string(nil), item.Aliases...)
		commands[index] = item
	}
	return HelpView{
		commands:  commands,
		shortcuts: append([]HelpShortcutSpec(nil), spec.Shortcuts...),
		entries:   append([]HelpEntrySpec(nil), spec.Entries...),
	}
}

func (view HelpView) View() string {
	sections := make([]string, 0, 3)
	if len(view.commands) > 0 {
		lines := []string{"正式命令："}
		for _, item := range view.commands {
			name := safeHelpText(item.Name)
			if name == "" {
				continue
			}
			line := "/" + name
			aliases := make([]string, 0, len(item.Aliases))
			for _, alias := range item.Aliases {
				if alias = safeHelpText(alias); alias != "" {
					aliases = append(aliases, "/"+alias)
				}
			}
			if len(aliases) > 0 {
				line += " (" + strings.Join(aliases, ", ") + ")"
			}
			if badge := safeHelpText(item.Badge); badge != "" {
				line += " [" + badge + "]"
			}
			if kind := safeHelpText(item.Type); kind != "" {
				line += " [" + kind + "]"
			}
			if hint := safeHelpText(item.ArgHint); hint != "" {
				line += " " + hint
			}
			if description := safeHelpText(item.Description); description != "" {
				line += " — " + description
			}
			if usage := safeHelpText(item.Usage); usage != "" {
				line += "；用法: " + usage
			}
			lines = append(lines, line)
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if len(view.shortcuts) > 0 {
		lines := []string{"上下文快捷键："}
		for _, item := range view.shortcuts {
			context := safeHelpText(item.Context)
			key := safeHelpText(item.Key)
			if context == "" || key == "" {
				continue
			}
			line := context + " " + key
			if command := safeHelpText(item.Command); command != "" {
				line += " → /" + command
			}
			if description := safeHelpText(item.Description); description != "" {
				line += " — " + description
			}
			lines = append(lines, line)
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if len(view.entries) > 0 {
		lines := []string{"权限、状态与诊断："}
		for _, item := range view.entries {
			kind := safeHelpText(item.Kind)
			name := safeHelpText(item.Name)
			if kind == "" || name == "" {
				continue
			}
			line := kind + " " + name
			if command := safeHelpText(item.Command); command != "" {
				line += " → /" + command
			}
			if description := safeHelpText(item.Description); description != "" {
				line += " — " + description
			}
			lines = append(lines, line)
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

func safeHelpText(value string) string {
	value = ansi.Strip(value)
	value = strings.Map(func(current rune) rune {
		if current < ' ' || current >= '\x7f' && current <= '\x9f' {
			return ' '
		}
		return current
	}, value)
	return strings.Join(strings.Fields(value), " ")
}
