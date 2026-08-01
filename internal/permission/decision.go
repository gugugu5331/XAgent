package permission

type DecisionKind string

const (
	DecisionAllow DecisionKind = "allow"
	DecisionDeny  DecisionKind = "deny"
	DecisionAsk   DecisionKind = "ask"
)

type DenyReason string

const (
	ReasonBlacklist     DenyReason = "blacklist"
	ReasonSandbox       DenyReason = "sandbox"
	ReasonPlanMode      DenyReason = "plan_mode"
	ReasonRuleDeny      DenyReason = "rule_deny"
	ReasonUserDenied    DenyReason = "user_denied"
	ReasonUserCancelled DenyReason = "user_cancelled"
	ReasonConfigError   DenyReason = "config_error"
)

type SourceKind string

const (
	SourceHardConstraint SourceKind = "hard_constraint"
	SourceSessionRule    SourceKind = "session_rule"
	SourceLocalRule      SourceKind = "local_rule"
	SourceProjectRule    SourceKind = "project_rule"
	SourceUserRule       SourceKind = "user_rule"
	SourceMode           SourceKind = "mode"
	SourceUserDecision   SourceKind = "user_decision"
	SourceNone           SourceKind = "none"
)

type Source struct {
	Kind        SourceKind
	Description string
}

type Decision struct {
	Kind         DecisionKind
	Reason       DenyReason
	Source       Source
	Rule         *Rule
	Ticket       ExecutionTicket
	Scope        GrantScope
	Prompt       *ConfirmationPrompt
	UserMessage  string
	ModelMessage string
	Recoverable  bool
}

type GrantScope string

const (
	GrantOnce      GrantScope = "once"
	GrantSession   GrantScope = "session"
	GrantPermanent GrantScope = "permanent"
	GrantRule      GrantScope = "rule"
	GrantMode      GrantScope = "mode"
)

type ConfirmationPrompt struct {
	Tool           string
	Risk           RiskLevel
	Summary        string
	Target         string
	Reason         string
	Mode           Mode
	Source         Source
	RulePreview    *Rule
	AllowPermanent bool
	Scopes         []ConfirmationScope
	RuleLocation   string
	RevokeHint     string
}

type ConfirmationScope struct {
	Scope       GrantScope `json:"scope"`
	Available   bool       `json:"available"`
	Description string     `json:"description"`
}

type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

type Context struct {
	ProjectRoot    string
	ReadRoots      []string
	Mode           Mode
	PlanMode       bool
	ConversationID string
	Identity       CallIdentity
}
