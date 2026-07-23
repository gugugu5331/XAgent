package hook

import (
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"xagent/internal/diagnostics"
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

func hookDiagnostic(code string, rule Rule, event Event, stage string, duration time.Duration, summary string, limits Limits) diagnostics.Diagnostic {
	summary = safeSummary(summary, limits.DiagnosticBytes)
	attributes := map[string]string{
		"event":                  string(event),
		"rule_ordinal":           strconv.Itoa(rule.Source.Ordinal),
		"effective_rule_ordinal": strconv.Itoa(rule.Source.EffectiveOrdinal),
		"action":                 string(rule.action.typeName),
		"stage":                  stage,
		"duration_ms":            strconv.FormatInt(duration.Milliseconds(), 10),
	}
	return diagnostics.New(code, diagnostics.SeverityWarning, summary).
		WithSource(rule.Source.Path).
		WithPath(fmt.Sprintf("hooks[%d]", rule.Source.Ordinal-1)).
		WithAttributes(attributes)
}

func safeSummary(value string, maxBytes int) string {
	if value == "" {
		value = "hook action failed"
	}
	result := make([]rune, 0, len(value))
	for _, r := range value {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			result = append(result, r)
		}
	}
	value = string(result)
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}
