package command

import (
	"fmt"
	"strings"
	"unicode"
)

func (r *Registry) Dispatch(input string, controller Controller) DispatchResult {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return DispatchResult{Kind: DispatchEmpty}
	}
	if !strings.HasPrefix(trimmed, "/") {
		return DispatchResult{Kind: DispatchPlainText, Text: trimmed}
	}

	name, args := splitCommand(trimmed)
	matched := normalizeName(name)
	index, ok := r.lookup[matched]
	if !ok {
		err := fmt.Errorf("未知命令 %s；请使用 /help 查看可用命令", name)
		if controller != nil {
			controller.DisplayError(err)
		}
		return DispatchResult{Kind: DispatchUnknown, Text: trimmed, Err: err}
	}
	definition := r.definitions[index]
	invocation := Invocation{CanonicalName: definition.Name, MatchedName: matched, Args: args, Raw: trimmed}
	if controller == nil {
		err := fmt.Errorf("命令控制器不可用")
		return DispatchResult{Kind: DispatchExecuted, Invocation: invocation, Err: err}
	}
	err := definition.Handler(ExecutionContext{Registry: r, Controller: controller}, invocation)
	if err != nil {
		controller.DisplayError(err)
	}
	return DispatchResult{Kind: DispatchExecuted, Invocation: invocation, Err: err}
}

func splitCommand(input string) (string, string) {
	index := strings.IndexFunc(input, unicode.IsSpace)
	if index < 0 {
		return input, ""
	}
	name := input[:index]
	args := strings.TrimLeftFunc(input[index:], unicode.IsSpace)
	return name, args
}
