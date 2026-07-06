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
