package mcpclient

import (
	"regexp"
	"strings"
)

func RedactArguments(arguments map[string]any) map[string]any {
	if arguments == nil {
		return nil
	}
	redacted := map[string]any{}
	for key, value := range arguments {
		redacted[key] = redactValue(key, value)
	}
	return redacted
}

func redactValue(key string, value any) any {
	if IsSensitiveKey(key) {
		return "[redacted]"
	}
	switch typed := value.(type) {
	case string:
		return RedactText(typed)
	case map[string]any:
		return RedactArguments(typed)
	case []any:
		items := make([]any, len(typed))
		for index, item := range typed {
			items[index] = redactValue(key, item)
		}
		return items
	default:
		return value
	}
}

func RedactAny(value any) any {
	return redactValue("", value)
}

func RedactText(value string) string {
	redacted := value
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)(api[_-]?key\s*[=:]\s*)[^\s,;]+`),
		regexp.MustCompile(`(?i)(token\s*[=:]\s*)[^\s,;]+`),
		regexp.MustCompile(`(?i)(secret\s*[=:]\s*)[^\s,;]+`),
		regexp.MustCompile(`(?i)(password\s*[=:]\s*)[^\s,;]+`),
		regexp.MustCompile(`(?i)(authorization\s*[=:]\s*)[^\s,;]+`),
		regexp.MustCompile(`(?i)(credential\s*[=:]\s*)[^\s,;]+`),
	} {
		redacted = pattern.ReplaceAllString(redacted, `${1}[redacted]`)
	}
	return redacted
}

func IsSensitiveKey(key string) bool {
	key = strings.ToLower(key)
	for _, marker := range []string{"token", "key", "secret", "password", "authorization", "credential"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
