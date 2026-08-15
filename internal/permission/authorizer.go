package permission

import (
	"path/filepath"
	"strings"

	"xagent/internal/redact"
)

type UserAction string

const (
	ActionDeny           UserAction = "deny"
	ActionAllowOnce      UserAction = "allow_once"
	ActionAllowSession   UserAction = "allow_session"
	ActionAllowPermanent UserAction = "allow_permanent"
	ActionCancel         UserAction = "cancel"
)

type Authorizer struct {
	Session    *Session
	User       RuleLayer
	Project    RuleLayer
	Local      RuleLayer
	LoadErrors []LoadError
	Health     *Health
	Writer     Writer
	Redact     func(string) string
	Issuer     TicketIssuer

	fixedMode         Mode
	permanentDisabled bool
}

func (a *Authorizer) Decide(call Call, context Context) Decision {
	normalized, err := NormalizeCallWithReadRoots(call, context.ProjectRoot, context.ReadRoots)
	if err != nil {
		return deny(call, ReasonConfigError, Source{Kind: SourceHardConstraint, Description: "invalid tool arguments"}, "工具参数无法用于权限判断", "Tool arguments are invalid for permission checking.")
	}
	context.Mode = a.effectiveMode(context.Mode)
	if context.PlanMode && isWriteOrBash(call.Name) {
		return deny(call, ReasonPlanMode, Source{Kind: SourceHardConstraint, Description: "plan mode"}, "Plan Mode 下不允许执行写工具或 Bash", "Plan Mode allows only read-only tools.")
	}
	if hard := a.CheckHard(normalized, context); hard != nil {
		return *hard
	}
	return a.DecideOrdinary(normalized, context)
}

// CheckHard evaluates the permission constraints which ordinary rules and
// user confirmation may never override. Plan-mode policy remains an explicit
// orchestrator/legacy-wrapper stage and is intentionally not handled here.
func (a *Authorizer) CheckHard(normalized NormalizedCall, context Context) *Decision {
	call := normalized.Call
	if a != nil && a.permissionHealthDegraded() {
		if isConservativeReadOnlyTool(call.Name) {
			// Degraded read-only calls remain eligible, but CheckHard must not
			// issue an execution capability. The ordinary stage runs only after
			// BeforeTool and performs the actual issuance.
			return nil
		}
		decision := deny(call, ReasonConfigError, Source{Kind: SourceHardConstraint, Description: "permission config error"}, "权限配置文件损坏，无法安全执行该工具", "Permission configuration is invalid, so this action cannot be performed safely.")
		return &decision
	}
	if call.Name == "Bash" {
		if bashReferencesPermissionConfig(normalized.RawCommand) {
			decision := deny(call, ReasonSandbox, Source{Kind: SourceHardConstraint, Description: "permission config protection"}, "Bash 不能直接操作权限配置文件", "Permission configuration files cannot be modified by ordinary tool calls.")
			return &decision
		}
		if reason, ok := CheckBashBlacklist(normalized.Command); ok {
			decision := deny(call, ReasonBlacklist, Source{Kind: SourceHardConstraint, Description: reason}, "命令被不可覆盖的安全规则拦截", "This command is blocked by a non-overridable safety rule.")
			return &decision
		}
	}
	if isPermissionConfigPath(normalized) {
		decision := deny(call, ReasonSandbox, Source{Kind: SourceHardConstraint, Description: "permission config protection"}, "权限配置文件不能通过普通工具修改", "Permission configuration files cannot be modified by ordinary tool calls.")
		return &decision
	}
	return nil
}

// DecideOrdinary applies configured layers and the mode default to a call that
// has already passed normalization, profile policy, and hard constraints.
func (a *Authorizer) DecideOrdinary(normalized NormalizedCall, context Context) Decision {
	context.Mode = a.effectiveMode(context.Mode)
	if a != nil && a.permissionHealthDegraded() {
		if isConservativeReadOnlyTool(normalized.Call.Name) {
			return a.allow(normalized, context, GrantMode, Source{Kind: SourceHardConstraint, Description: "permission config degraded read-only allowlist"})
		}
		return deny(normalized.Call, ReasonConfigError, Source{Kind: SourceHardConstraint, Description: "permission config error"}, "权限配置文件损坏，无法安全执行该工具", "Permission configuration is invalid, so this action cannot be performed safely.")
	}
	if a == nil {
		return a.decideByMode(context.Mode, normalized, context)
	}
	for _, layer := range a.layers() {
		if match, ok, err := FindRuleMatch(layer.Rules, normalized); err != nil {
			return deny(normalized.Call, ReasonConfigError, layer.Source, "权限规则匹配失败", "Permission rules could not be evaluated safely.")
		} else if ok {
			rule := match.Rule()
			if rule.Effect == string(EffectDeny) {
				return deny(normalized.Call, ReasonRuleDeny, layer.Source, "工具调用被权限规则拒绝", "This tool call is denied by a permission rule.")
			}
			if !match.AllowsWithoutPrompt() {
				return ask(normalized, context.Mode, "legacy permission rule requires confirmation")
			}
			ticket, err := match.IssueTicket(a.Issuer, context.Identity)
			if err != nil {
				return ticketFailure(normalized.Call)
			}
			decision := allowWithTicket(ticket, GrantRule, layer.Source)
			decision.Rule = &rule
			return decision
		}
	}
	decision := a.decideByMode(context.Mode, normalized, context)
	if decision.Prompt != nil {
		decision.Prompt = newConfirmationPrompt(normalized, decision.Prompt.Mode, decision.Prompt.Reason, a.redactText, a.permanentPermissionEnabled())
	}
	return decision
}

func (a *Authorizer) ResolveUserDecision(call Call, context Context, action UserAction) Decision {
	normalized, err := NormalizeCallWithReadRoots(call, context.ProjectRoot, context.ReadRoots)
	if err != nil {
		return deny(call, ReasonConfigError, Source{Kind: SourceUserDecision, Description: "invalid tool arguments"}, "工具参数无法用于权限判断", "Tool arguments are invalid for permission checking.")
	}
	return a.ResolveNormalizedUserDecision(normalized, context, action)
}

// ResolveNormalizedUserDecision applies a user's response to an existing
// normalized call, preserving its argument types and execution identity.
func (a *Authorizer) ResolveNormalizedUserDecision(normalized NormalizedCall, context Context, action UserAction) Decision {
	return a.resolveNormalizedUserDecision(normalized, context, action)
}

func (a *Authorizer) resolveNormalizedUserDecision(normalized NormalizedCall, context Context, action UserAction) Decision {
	context.Mode = a.effectiveMode(context.Mode)
	call := normalized.Call
	if context.PlanMode && isWriteOrBash(call.Name) {
		return deny(call, ReasonPlanMode, Source{Kind: SourceHardConstraint, Description: "plan mode"}, "Plan Mode 下不允许执行写工具或 Bash", "Plan Mode allows only read-only tools.")
	}
	if hard := a.CheckHard(normalized, context); hard != nil {
		return *hard
	}
	switch action {
	case ActionDeny:
		return deny(call, ReasonUserDenied, Source{Kind: SourceUserDecision, Description: "user denied"}, "用户拒绝执行工具", "The user denied this tool call. Choose a safer alternative.")
	case ActionCancel:
		return deny(call, ReasonUserCancelled, Source{Kind: SourceUserDecision, Description: "user cancelled"}, "用户取消工具确认", "The user cancelled this tool call. Choose a safer alternative.")
	case ActionAllowSession:
		rule := Rule{Tool: call.Name, Pattern: normalized.RuleValue, MatchType: string(MatchExact), Effect: string(EffectAllow), Description: "Session allow from confirmation"}
		decision := a.allow(normalized, context, GrantSession, Source{Kind: SourceUserDecision, Description: "session allow"})
		if decision.Kind != DecisionAllow {
			return decision
		}
		if a.Session != nil {
			a.Session.Add(rule)
		}
		decision.Rule = &rule
		return decision
	case ActionAllowPermanent:
		if !a.canAllowPermanent(normalized) {
			return deny(call, ReasonUserDenied, Source{Kind: SourceUserDecision, Description: "permanent allow disabled"}, "该工具调用不允许永久授权", "Permanent permission is not available for this tool call.")
		}
		rule := a.Writer.PreviewRule(normalized)
		decision := a.allow(normalized, context, GrantPermanent, Source{Kind: SourceUserDecision, Description: "permanent allow"})
		if decision.Kind != DecisionAllow {
			return decision
		}
		if err := a.Writer.WriteLocal(rule); err != nil {
			return deny(call, ReasonConfigError, Source{Kind: SourceUserDecision, Description: "permanent allow failed"}, "永久权限规则写入失败", "Permission configuration could not be updated safely.")
		}
		a.Local.Rules = appendUniqueRule(a.Local.Rules, rule)
		decision.Rule = &rule
		return decision
	case ActionAllowOnce:
		fallthrough
	default:
		return a.allow(normalized, context, GrantOnce, Source{Kind: SourceUserDecision, Description: "one-time allow"})
	}
}

func (a *Authorizer) layers() []RuleLayer {
	sessionRules := []Rule(nil)
	if a.Session != nil {
		sessionRules = a.Session.Rules()
	}
	local := a.Local
	if local.Source.Kind == "" {
		local.Source = Source{Kind: SourceLocalRule, Description: "local permissions"}
	}
	project := a.Project
	if project.Source.Kind == "" {
		project.Source = Source{Kind: SourceProjectRule, Description: "project permissions"}
	}
	user := a.User
	if user.Source.Kind == "" {
		user.Source = Source{Kind: SourceUserRule, Description: "user permissions"}
	}
	return []RuleLayer{
		{Source: Source{Kind: SourceSessionRule, Description: "session permissions"}, Rules: sessionRules},
		local,
		project,
		user,
	}
}

func (a *Authorizer) allow(normalized NormalizedCall, context Context, scope GrantScope, source Source) Decision {
	if a == nil || a.Issuer == nil {
		return ticketFailure(normalized.Call)
	}
	ticket, err := a.Issuer.Issue(normalized.Call.ID, context.Identity)
	if err != nil {
		return ticketFailure(normalized.Call)
	}
	return allowWithTicket(ticket, scope, source)
}

func allowWithTicket(ticket ExecutionTicket, scope GrantScope, source Source) Decision {
	return Decision{Kind: DecisionAllow, Source: source, Ticket: ticket, Scope: scope, Recoverable: true}
}

func ticketFailure(call Call) Decision {
	return deny(call, ReasonConfigError, Source{Kind: SourceHardConstraint, Description: "execution ticket unavailable"}, "无法安全签发执行票据", "A valid execution ticket could not be issued for this tool call.")
}

func ask(normalized NormalizedCall, mode Mode, reason string) Decision {
	prompt := newConfirmationPrompt(normalized, mode, reason, redact.Text, true)
	return Decision{
		Kind:        DecisionAsk,
		Source:      Source{Kind: SourceNone, Description: reason},
		Recoverable: true,
		Prompt:      prompt,
	}
}

func newConfirmationPrompt(normalized NormalizedCall, mode Mode, reason string, redactText func(string) string, permanentEnabled bool) *ConfirmationPrompt {
	risk := RiskMedium
	if normalized.Call.Name == "Bash" && normalized.ComplexShell {
		risk = RiskHigh
	}
	allowPermanent := permanentEnabled && canAllowPermanentWith(normalized, redactText)
	prompt := &ConfirmationPrompt{
		Tool:           normalized.Call.Name,
		Risk:           risk,
		Summary:        safeConfirmationText(redactText, normalized.RuleValue),
		Target:         safeConfirmationText(redactText, confirmationTarget(normalized)),
		Reason:         reason,
		Mode:           mode,
		AllowPermanent: allowPermanent,
		Scopes:         confirmationScopes(allowPermanent),
		RevokeHint:     "One-time permission expires after this call; session permission expires when the session ends.",
	}
	if allowPermanent {
		rule := Rule{Tool: normalized.Call.Name, Pattern: normalized.RuleValue, MatchType: string(MatchExact), Effect: string(EffectAllow)}
		prompt.RulePreview = &rule
		prompt.RuleLocation = localRuleSlot
		prompt.RevokeHint += " Remove the matching rule from " + localRuleSlot + " to revoke permanent permission."
	} else {
		prompt.RevokeHint += " Permanent permission is unavailable for this call."
	}
	return prompt
}

func deny(call Call, reason DenyReason, source Source, userMessage string, modelMessage string) Decision {
	return Decision{Kind: DecisionDeny, Reason: reason, Source: source, UserMessage: userMessage, ModelMessage: modelMessage, Recoverable: true}
}

func bashReferencesPermissionConfig(command string) bool {
	return containsPath(command, ".xagent/permissions.yaml") || containsPath(command, ".xagent/permissions.local.yaml")
}

func containsPath(value string, path string) bool {
	return strings.Contains(filepath.ToSlash(value), path)
}

func canAllowPermanent(normalized NormalizedCall) bool {
	return canAllowPermanentWith(normalized, redact.Text)
}

func (a *Authorizer) canAllowPermanent(normalized NormalizedCall) bool {
	if a != nil && a.permanentDisabled {
		return false
	}
	if a == nil {
		return canAllowPermanent(normalized)
	}
	return canAllowPermanentWith(normalized, a.redactText)
}

func (a *Authorizer) permanentPermissionEnabled() bool {
	return a == nil || !a.permanentDisabled
}

func (a *Authorizer) effectiveMode(requested Mode) Mode {
	if a == nil || a.fixedMode == "" {
		return modeOrDefault(requested)
	}
	fixed, fixedRank, fixedOK := normalizedModeRank(a.fixedMode)
	if !fixedOK {
		return ModeStrict
	}
	if requested == "" {
		return fixed
	}
	requested, requestedRank, requestedOK := normalizedModeRank(requested)
	if !requestedOK {
		return ModeStrict
	}
	if requestedRank < fixedRank {
		return requested
	}
	return fixed
}

func canAllowPermanentWith(normalized NormalizedCall, redactText func(string) string) bool {
	rule := Rule{Tool: normalized.Call.Name, Pattern: normalized.RuleValue, MatchType: string(MatchExact), Effect: string(EffectAllow)}
	return rule.ValidatePermanent() == nil && !normalizedContainsSecret(normalized, redactText)
}

func (a *Authorizer) redactText(value string) string {
	if a == nil || a.Redact == nil {
		return redact.Text(value)
	}
	return a.Redact(value)
}

func safeConfirmationText(redactText func(string) string, value string) string {
	if redactText == nil {
		return redact.Text(value)
	}
	return redactText(value)
}

func confirmationTarget(normalized NormalizedCall) string {
	if normalized.OriginalPath != "" {
		return normalized.OriginalPath
	}
	if normalized.Call.Name == "Bash" {
		return normalized.RawCommand
	}
	if strings.HasPrefix(normalized.Call.Name, "mcp__") {
		return normalized.Call.Name + " " + normalized.Call.ArgumentsJSON
	}
	return normalized.RuleValue
}

func confirmationScopes(permanent bool) []ConfirmationScope {
	return []ConfirmationScope{
		{Scope: GrantOnce, Available: true, Description: "Allow only this call; the permission expires immediately after use."},
		{Scope: GrantSession, Available: true, Description: "Allow the same minimal rule until the current session ends."},
		{Scope: GrantPermanent, Available: permanent, Description: "Save the same minimal rule in the local permission file until explicitly removed."},
	}
}

func normalizedContainsSecret(normalized NormalizedCall, redactText func(string) string) bool {
	for _, value := range []string{normalized.Call.ArgumentsJSON, normalized.RawCommand, normalized.OriginalPath, normalized.RuleValue} {
		if value != "" && safeConfirmationText(redactText, value) != value {
			return true
		}
	}
	return permissionValueContainsSecret(normalized.Arguments, redactText)
}

func permissionValueContainsSecret(value any, redactText func(string) string) bool {
	switch typed := value.(type) {
	case string:
		return safeConfirmationText(redactText, typed) != typed
	case map[string]any:
		for key, item := range typed {
			if redact.IsSensitiveKey(key) {
				return true
			}
			if permissionValueContainsSecret(item, redactText) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if permissionValueContainsSecret(item, redactText) {
				return true
			}
		}
	}
	return false
}

func (a *Authorizer) permissionHealthDegraded() bool {
	return len(a.LoadErrors) > 0 || (a.Health != nil && a.Health.Degraded())
}

func isPermissionConfigPath(normalized NormalizedCall) bool {
	if normalized.Call.Name != "Write" && normalized.Call.Name != "Edit" {
		return false
	}
	path := normalized.RuleValue
	return path == ".xagent/permissions.yaml" || path == ".xagent/permissions.local.yaml"
}
