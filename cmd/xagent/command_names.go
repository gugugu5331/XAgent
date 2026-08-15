package main

import "xagent/internal/command"

// reservedCommandNames returns every canonical command name and alias that a
// discovered Skill must not shadow. It lives in production because both the
// real assembly path and startup fixtures build the same Skill manager.
func reservedCommandNames(definitions []command.Definition) []string {
	names := make([]string, 0, len(definitions)*2)
	for _, definition := range definitions {
		names = append(names, definition.Name)
		names = append(names, definition.Aliases...)
	}
	return names
}
