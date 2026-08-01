package permission

import "strings"

type Mode string

const (
	ModeStrict     Mode = "strict"
	ModeDefault    Mode = "default"
	ModePermissive Mode = "permissive"
)

func ParseMode(value string) (Mode, bool) {
	switch Mode(value) {
	case ModeStrict, ModeDefault, ModePermissive:
		return Mode(value), true
	case "":
		return ModeDefault, true
	default:
		return "", false
	}
}

func (a *Authorizer) decideByMode(mode Mode, normalized NormalizedCall, context Context) Decision {
	if strings.HasPrefix(normalized.Call.Name, "mcp__") {
		return ask(normalized, modeOrDefault(mode), "mcp tools require confirmation")
	}
	switch mode {
	case ModeStrict:
		if isReadOnlyFileTool(normalized.Call.Name) {
			return a.allow(normalized, context, GrantMode, Source{Kind: SourceMode, Description: "strict mode"})
		}
		return ask(normalized, mode, "strict mode requires confirmation")
	case ModePermissive:
		if normalized.Call.Name == "Bash" {
			if isBuiltinReadOnlyBash(normalized.Command) {
				return a.allow(normalized, context, GrantMode, Source{Kind: SourceMode, Description: "permissive mode"})
			}
			return ask(normalized, mode, "bash requires confirmation")
		}
		return a.allow(normalized, context, GrantMode, Source{Kind: SourceMode, Description: "permissive mode"})
	default:
		if isReadOnlyFileTool(normalized.Call.Name) {
			return a.allow(normalized, context, GrantMode, Source{Kind: SourceMode, Description: "default mode"})
		}
		return ask(normalized, ModeDefault, "default mode requires confirmation")
	}
}

func modeOrDefault(mode Mode) Mode {
	if mode == "" {
		return ModeDefault
	}
	return mode
}

func isBuiltinReadOnlyBash(command string) bool {
	switch command {
	case "pwd", "git status", "git diff", "git log", "git branch":
		return true
	default:
		return false
	}
}

func isReadOnlyFileTool(toolName string) bool {
	return toolName == "Read" || toolName == "Glob" || toolName == "Grep"
}

func isWriteOrBash(toolName string) bool {
	return toolName == "Write" || toolName == "Edit" || toolName == "Bash"
}
