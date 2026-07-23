package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"xagent/internal/redact"
)

var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)

func parseDecision(data []byte, limits Limits, runtimeRedactor *redact.RuntimeRedactor) (ToolDecision, error) {
	if !utf8.Valid(data) {
		return Continue(), fmt.Errorf("decision is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return Continue(), fmt.Errorf("decision must be a JSON object")
	}
	values := map[string]string{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return Continue(), fmt.Errorf("invalid decision object")
		}
		key, ok := keyToken.(string)
		if !ok {
			return Continue(), fmt.Errorf("invalid decision key")
		}
		if key != "decision" && key != "reason" {
			return Continue(), fmt.Errorf("unknown decision key")
		}
		if _, exists := values[key]; exists {
			return Continue(), fmt.Errorf("duplicate decision key")
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return Continue(), fmt.Errorf("invalid decision value")
		}
		value, ok := valueToken.(string)
		if !ok {
			return Continue(), fmt.Errorf("decision values must be strings")
		}
		values[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return Continue(), fmt.Errorf("invalid decision object")
	}
	if token, err = decoder.Token(); err != io.EOF {
		return Continue(), fmt.Errorf("extra decision content")
	}
	switch values["decision"] {
	case "allow":
		if len(values) != 1 {
			return Continue(), fmt.Errorf("allow must not include reason")
		}
		return Continue(), nil
	case "deny":
		if len(values) != 2 {
			return Continue(), fmt.Errorf("deny requires reason")
		}
		reason := values["reason"]
		if len(reason) > limits.DenyReasonBytes {
			return Continue(), fmt.Errorf("deny reason exceeds limit")
		}
		reason = sanitizeReason(reason, runtimeRedactor)
		if len(reason) > limits.DenyReasonBytes {
			return Continue(), fmt.Errorf("deny reason exceeds limit after redaction")
		}
		if strings.TrimSpace(reason) == "" {
			return Continue(), fmt.Errorf("deny reason is empty")
		}
		return Deny(reason), nil
	default:
		return Continue(), fmt.Errorf("unknown decision")
	}
}

func sanitizeReason(value string, runtimeRedactor *redact.RuntimeRedactor) string {
	// Redact the decoded value before removing terminal controls. A registered
	// secret may itself contain an ANSI sequence or another control character;
	// sanitizing first would change that value and make the registered secret
	// impossible to match. The second pass below remains authoritative for the
	// final, sanitized text and catches values that only become recognizable
	// after cleanup.
	value = redactDecisionReason(value, runtimeRedactor)
	value = ansiEscape.ReplaceAllString(value, "")
	var result strings.Builder
	for _, r := range value {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			result.WriteRune(r)
		}
	}
	return redactDecisionReason(result.String(), runtimeRedactor)
}

func redactDecisionReason(value string, runtimeRedactor *redact.RuntimeRedactor) string {
	if runtimeRedactor != nil {
		return runtimeRedactor.Text(value)
	}
	return redact.Text(value)
}
