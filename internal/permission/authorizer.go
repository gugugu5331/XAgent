package permission

import (
	"path/filepath"
	"strings"
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
	Writer     Writer
}

func (a *Authorizer) Decide(call Call, context Context) Decision {
	normalized, err := NormalizeCallWithReadRoots(call, context.ProjectRoot, context.ReadRoots)
	if err != nil {
		return deny(call, ReasonConfigError, Source{Kind: SourceHardConstraint, Description: "invalid tool arguments"}, "工具参数无法用于权限判断", "Tool arguments are invalid for permission checking.")
	}
	if context.Mode == "" {
		context.Mode = ModeDefault
	}
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
func (a *Authorizer) CheckHard(normalized NormalizedCall, _ Context) *Decision {
	call := normalized.Call
	if a != nil && len(a.LoadErrors) > 0 && isDangerousForConfigError(call.Name) {
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
	if context.Mode == "" {
		context.Mode = ModeDefault
	}
	if a == nil {
		return DecideByMode(context.Mode, normalized)
	}
	for _, layer := range a.layers() {
		if rule, ok, err := FindRule(layer.Rules, normalized); err != nil {
			return deny(normalized.Call, ReasonConfigError, layer.Source, "权限规则匹配失败", "Permission rules could not be evaluated safely.")
		} else if ok {
			if rule.Effect == string(EffectDeny) {
				return deny(normalized.Call, ReasonRuleDeny, layer.Source, "工具调用被权限规则拒绝", "This tool call is denied by a permission rule.")
			}
			decision := allow(normalized, GrantRule, layer.Source)
			decision.Rule = rule
			return decision
		}
	}
	return DecideByMode(context.Mode, normalized)
}

func (a *Authorizer) ResolveUserDecision(call Call, context Context, action UserAction) Decision {
	normalized, err := NormalizeCallWithReadRoots(call, context.ProjectRoot, context.ReadRoots)
	if err != nil {
		return deny(call, ReasonConfigError, Source{Kind: SourceUserDecision, Description: "invalid tool arguments"}, "工具参数无法用于权限判断", "Tool arguments are invalid for permission checking.")
	}
	return a.ResolveNormalizedUserDecision(normalized, context, action)
}

// ResolveNormalizedUserDecision applies a user's response to an existing
// normalized call, preserving its argument types and grant fingerprint.
func (a *Authorizer) ResolveNormalizedUserDecision(normalized NormalizedCall, _ Context, action UserAction) Decision {
	call := normalized.Call
	switch action {
	case ActionDeny:
		return deny(call, ReasonUserDenied, Source{Kind: SourceUserDecision, Description: "user denied"}, "用户拒绝执行工具", "The user denied this tool call. Choose a safer alternative.")
	case ActionCancel:
		return deny(call, ReasonUserCancelled, Source{Kind: SourceUserDecision, Description: "user cancelled"}, "用户取消工具确认", "The user cancelled this tool call. Choose a safer alternative.")
	case ActionAllowSession:
		rule := Rule{Tool: call.Name, Pattern: normalized.RuleValue, MatchType: string(MatchExact), Effect: string(EffectAllow), Description: "Session allow from confirmation"}
		if a.Session != nil {
			a.Session.Add(rule)
		}
		decision := allow(normalized, GrantSession, Source{Kind: SourceUserDecision, Description: "session allow"})
		decision.Rule = &rule
		return decision
	case ActionAllowPermanent:
		if !canAllowPermanent(normalized) {
			return deny(call, ReasonUserDenied, Source{Kind: SourceUserDecision, Description: "permanent allow disabled"}, "该工具调用不允许永久授权", "Permanent permission is not available for this tool call.")
		}
		rule := a.Writer.PreviewRule(normalized)
		if err := a.Writer.WriteLocal(rule); err != nil {
			return deny(call, ReasonConfigError, Source{Kind: SourceUserDecision, Description: "permanent allow failed"}, "永久权限规则写入失败", "Permission configuration could not be updated safely.")
		}
		a.Local.Rules = appendUniqueRule(a.Local.Rules, rule)
		decision := allow(normalized, GrantPermanent, Source{Kind: SourceUserDecision, Description: "permanent allow"})
		decision.Rule = &rule
		return decision
	case ActionAllowOnce:
		fallthrough
	default:
		return allow(normalized, GrantOnce, Source{Kind: SourceUserDecision, Description: "one-time allow"})
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

func allow(normalized NormalizedCall, scope GrantScope, source Source) Decision {
	return Decision{
		Kind:   DecisionAllow,
		Source: source,
		Grant: &Grant{
			CallID:      normalized.Call.ID,
			Tool:        normalized.Call.Name,
			Scope:       scope,
			Source:      source,
			Fingerprint: Fingerprint(normalized),
		},
		Recoverable: true,
	}
}

func ask(normalized NormalizedCall, mode Mode, reason string) Decision {
	risk := RiskMedium
	allowPermanent := canAllowPermanent(normalized)
	if normalized.Call.Name == "Bash" && normalized.ComplexShell {
		risk = RiskHigh
	}
	rule := Rule{Tool: normalized.Call.Name, Pattern: normalized.RuleValue, MatchType: string(MatchExact), Effect: string(EffectAllow)}
	return Decision{
		Kind:        DecisionAsk,
		Source:      Source{Kind: SourceNone, Description: reason},
		Recoverable: true,
		Prompt: &ConfirmationPrompt{
			Tool:           normalized.Call.Name,
			Risk:           risk,
			Summary:        normalized.RuleValue,
			Target:         normalized.OriginalPath,
			Reason:         reason,
			Mode:           mode,
			RulePreview:    &rule,
			AllowPermanent: allowPermanent,
		},
	}
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
	if strings.HasPrefix(normalized.Call.Name, "mcp__") {
		return false
	}
	if ruleContainsSecret(Rule{Tool: normalized.Call.Name, Pattern: normalized.RuleValue, MatchType: string(MatchExact), Effect: string(EffectAllow)}) {
		return false
	}
	return !(normalized.Call.Name == "Bash" && normalized.ComplexShell)
}

func isDangerousForConfigError(toolName string) bool {
	return toolName == "Bash" || toolName == "Write" || toolName == "Edit"
}

func isPermissionConfigPath(normalized NormalizedCall) bool {
	if normalized.Call.Name != "Write" && normalized.Call.Name != "Edit" {
		return false
	}
	path := normalized.RuleValue
	return path == ".xagent/permissions.yaml" || path == ".xagent/permissions.local.yaml"
}
