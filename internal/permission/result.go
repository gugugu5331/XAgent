package permission

func DeniedResultData(decision Decision) map[string]any {
	return map[string]any{
		"permission_denied": true,
		"reason":            string(decision.Reason),
		"recoverable":       decision.Recoverable,
	}
}

func DeniedModelMessage(decision Decision) string {
	if decision.ModelMessage != "" {
		return decision.ModelMessage
	}
	return "Permission denied before executing tool."
}

// ConfirmationResultData exposes only the already-sanitized confirmation
// model. Raw arguments and executable rule values are intentionally absent.
func ConfirmationResultData(decision Decision) map[string]any {
	if decision.Prompt == nil {
		return nil
	}
	prompt := decision.Prompt
	scopes := make([]ConfirmationScope, len(prompt.Scopes))
	copy(scopes, prompt.Scopes)
	data := map[string]any{
		"tool":            prompt.Tool,
		"risk":            string(prompt.Risk),
		"summary":         prompt.Summary,
		"target":          prompt.Target,
		"reason":          prompt.Reason,
		"mode":            string(prompt.Mode),
		"scopes":          scopes,
		"allow_permanent": prompt.AllowPermanent,
		"rule_location":   prompt.RuleLocation,
		"revoke_hint":     prompt.RevokeHint,
	}
	if prompt.RulePreview != nil {
		data["rule_preview"] = prompt.RulePreview.Display()
	}
	return data
}
