package hook

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"xagent/internal/redact"
)

type templatePart struct {
	literal string
	field   *compiledField
}

type compiledTemplate struct {
	parts []templatePart
}

func compileTemplate(event Event, source string, maxBytes int) (*compiledTemplate, error) {
	if !utf8.ValidString(source) || len(source) > maxBytes {
		return nil, fmt.Errorf("template exceeds limit or is invalid UTF-8")
	}
	parts := []templatePart{}
	for len(source) > 0 {
		start := strings.Index(source, "{{")
		if start < 0 {
			if strings.Contains(source, "}}") {
				return nil, fmt.Errorf("malformed template")
			}
			parts = append(parts, templatePart{literal: source})
			break
		}
		if start > 0 {
			parts = append(parts, templatePart{literal: source[:start]})
		}
		source = source[start+2:]
		end := strings.Index(source, "}}")
		if end < 0 {
			return nil, fmt.Errorf("unterminated template token")
		}
		token := source[:end]
		if token == "" || token != strings.TrimSpace(token) || strings.ContainsAny(token, "|(){}") {
			return nil, fmt.Errorf("invalid template token")
		}
		field, err := compileField(event, token)
		if err != nil {
			return nil, fmt.Errorf("invalid template field")
		}
		parts = append(parts, templatePart{field: &field})
		source = source[end+2:]
	}
	return &compiledTemplate{parts: parts}, nil
}

func (t *compiledTemplate) render(event *frozenEvent, maxBytes int, redactor *redact.RuntimeRedactor) (string, error) {
	if t == nil {
		return "", fmt.Errorf("missing template")
	}
	var output bytes.Buffer
	for _, part := range t.parts {
		if part.field == nil {
			if err := appendTemplateFragment(&output, part.literal, maxBytes); err != nil {
				return "", err
			}
		} else {
			value := event.lookup(*part.field)
			if !value.exists || value.kind == scalarNone {
				return "", fmt.Errorf("template field unavailable")
			}
			var fragment string
			switch value.kind {
			case scalarString:
				fragment = value.value.(string)
			case scalarBool:
				if value.value.(bool) {
					fragment = "true"
				} else {
					fragment = "false"
				}
			case scalarNumber:
				var ok bool
				fragment, ok = canonicalDecimalBounded(value.value.(numberScalar).text, maxBytes-output.Len())
				if !ok {
					return "", fmt.Errorf("rendered template exceeds limit")
				}
			}
			if err := appendTemplateFragment(&output, fragment, maxBytes); err != nil {
				return "", err
			}
		}
	}
	value := output.String()
	if redactor != nil {
		value = redactor.Text(value)
	}
	if len(value) > maxBytes {
		return "", fmt.Errorf("rendered template exceeds limit")
	}
	return value, nil
}

func appendTemplateFragment(output *bytes.Buffer, fragment string, maxBytes int) error {
	if output == nil || maxBytes < 0 || output.Len() > maxBytes || len(fragment) > maxBytes-output.Len() {
		return fmt.Errorf("rendered template exceeds limit")
	}
	_, _ = output.WriteString(fragment)
	return nil
}
