package tool

import (
	"fmt"
	"strings"
)

func stringArg(args map[string]any, key string) (string, bool) {
	value, ok := args[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

func optionalStringArg(args map[string]any, key string) string {
	value, ok := args[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

func boolArg(args map[string]any, key string) bool {
	value, ok := args[key]
	if !ok {
		return false
	}
	result, _ := value.(bool)
	return result
}

func errorCode(err error) string {
	message := err.Error()
	for _, code := range []string{ErrPathOutsideProject, ErrNotFound, ErrMultipleMatches, ErrTimeout, ErrInvalidArguments} {
		if strings.Contains(message, code) {
			return code
		}
	}
	return fmt.Sprintf("%s", ErrNotFound)
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}
