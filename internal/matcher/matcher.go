// Package matcher contains the small, shared string matching core used by
// permission rules and lifecycle hooks. Matching is deliberately literal:
// values are case-sensitive, are never trimmed, and must match in full.
package matcher

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MatchExact reports whether value and pattern are byte-for-byte equal.
func MatchExact(value string, pattern string) bool {
	return value == pattern
}

// MatchGlob reports whether value matches pattern in full. A single '*' and
// '?' do not cross slash boundaries, matching filepath.Match semantics. A
// double star may cross slash boundaries; "**/" also matches zero path
// components. Invalid glob syntax is returned as an error.
func MatchGlob(value string, pattern string) (bool, error) {
	if !strings.Contains(pattern, "**") {
		return filepath.Match(pattern, value)
	}
	if err := validatePattern(pattern); err != nil {
		return false, err
	}
	expression := globExpression(pattern)
	return regexp.MatchString(`\A`+expression+`\z`, value)
}

func validatePattern(pattern string) error {
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '\\':
			index++
			if index == len(pattern) {
				return filepath.ErrBadPattern
			}
		case '[':
			end, ok := classEnd(pattern, index)
			if !ok {
				return filepath.ErrBadPattern
			}
			index = end
		}
	}
	return nil
}

func globExpression(pattern string) string {
	var expression strings.Builder
	for index := 0; index < len(pattern); {
		switch pattern[index] {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index += 2
				if index < len(pattern) && pattern[index] == '/' {
					expression.WriteString(`(?s:(?:.*/)?)`)
					index++
				} else {
					expression.WriteString(`(?s:.*)`)
				}
				continue
			}
			expression.WriteString(`[^/]*`)
			index++
		case '?':
			expression.WriteString(`[^/]`)
			index++
		case '[':
			end, _ := classEnd(pattern, index)
			expression.WriteString(pattern[index : end+1])
			index = end + 1
		case '\\':
			index++
			_, size := utf8.DecodeRuneInString(pattern[index:])
			expression.WriteString(regexp.QuoteMeta(pattern[index : index+size]))
			index += size
		default:
			start := index
			for index < len(pattern) && !strings.ContainsRune(`*?[\`, rune(pattern[index])) {
				index++
			}
			expression.WriteString(regexp.QuoteMeta(pattern[start:index]))
		}
	}
	return expression.String()
}

func classEnd(pattern string, start int) (int, bool) {
	index := start + 1
	if index < len(pattern) && pattern[index] == '^' {
		index++
	}
	if index < len(pattern) && pattern[index] == ']' {
		index++
	}
	for ; index < len(pattern); index++ {
		if pattern[index] == '\\' {
			index++
			if index == len(pattern) {
				return 0, false
			}
			continue
		}
		if pattern[index] == ']' {
			return index, true
		}
	}
	return 0, false
}
