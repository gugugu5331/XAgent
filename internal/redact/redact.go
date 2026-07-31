package redact

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const marker = "[redacted]"

const shortSecretRuneLimit = 4

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key\s*[=:]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(apiKey\s*:\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(access[_-]?token\s*[=:]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(refresh[_-]?token\s*[=:]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(token\s*[=:]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(secret\s*[=:]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(password\s*[=:]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(cookie\s*[=:]\s*)[^\n\r,;]+`),
	regexp.MustCompile(`(?i)(set-cookie\s*[=:]\s*)[^\n\r,;]+`),
	regexp.MustCompile(`(?i)(x-api-key\s*:\s*)[^\n\r]+`),
	regexp.MustCompile(`(?i)(authorization\s*[=:]\s*)[^\n\r,;]+`),
	regexp.MustCompile(`(?i)(authorization:\s*)[^\n\r,;]+`),
	regexp.MustCompile(`(?i)(credential\s*[=:]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(AWS_SECRET_ACCESS_KEY\s*=\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(GITHUB_TOKEN\s*=\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(ANTHROPIC_API_KEY\s*=\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(OPENAI_API_KEY\s*=\s*)[^\s,;]+`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`ghp_[A-Za-z0-9_]+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

func Text(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	redacted := redactURLSecrets(value)
	for _, pattern := range secretPatterns {
		redacted = pattern.ReplaceAllString(redacted, `${1}`+marker)
	}
	return redacted
}

func Any(value any) any {
	return redactValue("", value)
}

func Map(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	redacted := map[string]any{}
	for key, value := range values {
		redacted[key] = redactValue(key, value)
	}
	return redacted
}

func IsSensitiveKey(key string) bool {
	key = strings.ToLower(key)
	for _, marker := range []string{"token", "key", "secret", "password", "authorization", "credential", "cookie"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

type RuntimeRedactor struct {
	mu      sync.RWMutex
	secrets []string
}

func NewRuntimeRedactor() *RuntimeRedactor {
	return &RuntimeRedactor{}
}

func (r *RuntimeRedactor) RegisterSecret(value string) {
	if r == nil {
		return
	}
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.secrets {
		if existing == value {
			return
		}
	}
	r.secrets = append(r.secrets, value)
	sort.SliceStable(r.secrets, func(i, j int) bool {
		return len(r.secrets[i]) > len(r.secrets[j])
	})
}

func (r *RuntimeRedactor) MaxSecretBytes() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	maximum := 0
	for _, secret := range r.secrets {
		if len(secret) > maximum {
			maximum = len(secret)
		}
	}
	return maximum
}

func (r *RuntimeRedactor) Text(value string) string {
	redacted := Text(value)
	if r == nil {
		return redacted
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, secret := range r.secrets {
		if utf8.RuneCountInString(secret) < shortSecretRuneLimit {
			redacted = replaceWholeToken(redacted, secret)
			continue
		}
		redacted = strings.ReplaceAll(redacted, secret, marker)
	}
	return redacted
}

func replaceWholeToken(value string, secret string) string {
	if secret == "" {
		return value
	}
	var result strings.Builder
	searchFrom := 0
	for searchFrom < len(value) {
		relative := strings.Index(value[searchFrom:], secret)
		if relative < 0 {
			break
		}
		start := searchFrom + relative
		end := start + len(secret)
		if tokenBoundaryBefore(value, start) && tokenBoundaryAfter(value, end) {
			result.WriteString(value[searchFrom:start])
			result.WriteString(marker)
			searchFrom = end
			continue
		}
		result.WriteString(value[searchFrom:end])
		searchFrom = end
	}
	if searchFrom == 0 {
		return value
	}
	result.WriteString(value[searchFrom:])
	return result.String()
}

func tokenBoundaryBefore(value string, index int) bool {
	if index == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(value[:index])
	return !isTokenRune(r)
}

func tokenBoundaryAfter(value string, index int) bool {
	if index == len(value) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(value[index:])
	return !isTokenRune(r)
}

func isTokenRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func (r *RuntimeRedactor) Any(value any) any {
	if r == nil {
		return Any(value)
	}
	return r.redactValue("", value)
}

func (r *RuntimeRedactor) Map(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	redacted := map[string]any{}
	for key, value := range values {
		redacted[key] = r.redactValue(key, value)
	}
	return redacted
}

func (r *RuntimeRedactor) redactValue(key string, value any) any {
	if IsSensitiveKey(key) {
		return marker
	}
	switch typed := value.(type) {
	case string:
		return r.Text(typed)
	case map[string]any:
		return r.Map(typed)
	case []any:
		items := make([]any, len(typed))
		for index, item := range typed {
			items[index] = r.redactValue(key, item)
		}
		return items
	default:
		return value
	}
}

func redactValue(key string, value any) any {
	if IsSensitiveKey(key) {
		return marker
	}
	switch typed := value.(type) {
	case string:
		return Text(typed)
	case map[string]any:
		return Map(typed)
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

func redactURLSecrets(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return value
	}
	redacted := value
	for _, field := range fields {
		trimmed := strings.Trim(field, `"'()[],;`)
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		changed := false
		if parsed.User != nil {
			parsed.User = url.UserPassword(marker, marker)
			changed = true
		}
		query := parsed.Query()
		for key := range query {
			if IsSensitiveKey(key) {
				query.Set(key, marker)
				changed = true
			}
		}
		if !changed {
			continue
		}
		parsed.RawQuery = query.Encode()
		redacted = strings.ReplaceAll(redacted, trimmed, parsed.String())
	}
	return redacted
}
