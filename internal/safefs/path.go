package safefs

import (
	"errors"
	"path"
	"strings"
	"unicode/utf8"
)

var errInvalidRelativePath = errors.New("safefs relative path is invalid")

func canonicalRelative(value string) (string, error) {
	if value == "" || value == "." || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return "", errInvalidRelativePath
	}
	if strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return "", errInvalidRelativePath
	}
	if len(value) >= 2 && value[1] == ':' && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) {
		return "", errInvalidRelativePath
	}
	canonical := path.Clean(value)
	if canonical != value || path.IsAbs(canonical) {
		return "", errInvalidRelativePath
	}
	for _, component := range strings.Split(canonical, "/") {
		if component == "" || component == "." || component == ".." {
			return "", errInvalidRelativePath
		}
	}
	return canonical, nil
}
