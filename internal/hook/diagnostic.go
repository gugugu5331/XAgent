package hook

import (
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

const (
	DiagnosticConditionFailed        = "hook_condition_failed"
	DiagnosticTemplateFailed         = "hook_template_failed"
	DiagnosticCommandFailed          = "hook_command_failed"
	DiagnosticHTTPFailed             = "hook_http_failed"
	DiagnosticDecisionInvalid        = "hook_decision_invalid"
	DiagnosticActionTimeout          = "hook_action_timeout"
	DiagnosticAsyncQueueFull         = "hook_async_queue_full"
	DiagnosticShutdownCancelled      = "hook_shutdown_cancelled"
	DiagnosticSubAgentNotImplemented = "hook_subagent_not_implemented"
	DiagnosticPromptScopeUnavailable = "hook_prompt_scope_unavailable"
	DiagnosticLimitExceeded          = "hook_limit_exceeded"
	DiagnosticActionPanic            = "hook_action_panic"
)

type diagnosticRule struct {
	source           redact.SafeText
	ordinal          int
	effectiveOrdinal int
	action           ActionType
}

func hookDiagnostic(code string, rule diagnosticRule, event Event, stage string, duration time.Duration, summary redact.SafeText, limits Limits) diagnostics.Diagnostic {
	summary = safeSummary(summary, limits.DiagnosticBytes)
	attributes := map[string]string{
		"event":                  string(event),
		"rule_ordinal":           strconv.Itoa(rule.ordinal),
		"effective_rule_ordinal": strconv.Itoa(rule.effectiveOrdinal),
		"action":                 string(rule.action),
		"stage":                  stage,
		"duration_ms":            strconv.FormatInt(duration.Milliseconds(), 10),
	}
	return diagnostics.New(code, diagnostics.SeverityWarning, summary.Text()).
		WithSource(rule.source.Text()).
		WithPath(fmt.Sprintf("hooks[%d]", rule.ordinal-1)).
		WithAttributes(attributes)
}

func safeSummary(value redact.SafeText, maxBytes int) redact.SafeText {
	text := value.Text()
	if text == "" {
		text = "hook action failed"
	}
	result := make([]rune, 0, len(text))
	for _, r := range text {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			result = append(result, r)
		}
	}
	text = string(result)
	if len(text) <= maxBytes {
		return redact.NewRuntimeRedactor().Redact(text)
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return redact.NewRuntimeRedactor().Redact(text[:end])
}
