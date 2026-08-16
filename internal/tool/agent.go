package tool

import (
	"context"
	"errors"
)

const (
	AgentToolName         = "Agent"
	AgentToolMaxTaskBytes = 32 << 10
)

const (
	AgentTypeDefined = "defined"
	AgentTypeFork    = "fork"

	AgentPlacementDefault    = "default"
	AgentPlacementForeground = "foreground"
	AgentPlacementBackground = "background"
)

// AgentToolInput is the stable model-facing delegation contract. Runtime
// identity and parent snapshots are trusted routing data and intentionally do
// not appear in this payload.
type AgentToolInput struct {
	Task      string `json:"task"`
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Placement string `json:"placement,omitempty"`
}

type agentTool struct {
	resultFactory *ResultFactory
}

// NewAgentTool returns the system-routed Agent delegation definition.
func NewAgentTool() Tool {
	return agentTool{}
}

func NewAgentToolWithResultFactory(factory *ResultFactory) (Tool, error) {
	if factory == nil {
		return nil, errors.New("safe Agent result factory is unavailable")
	}
	return agentTool{resultFactory: factory}, nil
}

func (agentTool) Name() string { return AgentToolName }

func (agentTool) Description() string {
	return "Delegate a bounded task to a defined or forked subagent. Fork tasks always run in the background."
}

func (agentTool) Schema() Schema {
	return Schema{Raw: []byte(`{"type":"object","properties":{"task":{"type":"string","minLength":1,"maxLength":32768,"description":"Complete task for the subagent."},"type":{"type":"string","enum":["defined","fork"],"description":"Delegation context type."},"role":{"type":"string","minLength":1,"maxLength":32768,"description":"Optional canonical role name."},"placement":{"type":"string","enum":["default","foreground","background"],"description":"Requested task placement."}},"required":["task","type"],"additionalProperties":false}`)}
}

func (agentTool) Risk() Risk { return RiskSafe }

func (agentTool) executionRoute() ExecutionRoute { return RouteSystem }

func (t agentTool) UsesSafeResultBoundary() bool { return t.resultFactory != nil }

func (t agentTool) Execute(_ context.Context, input Input) Result {
	if t.resultFactory != nil {
		return buildSyntheticResult(t.resultFactory, ResultFactoryInput{
			CallID: input.CallID, Name: input.Name, State: Rejected, Status: StatusError,
			Summary: "Agent 需要内部路由",
			Error:   &Error{Code: ErrInternalRoutingRequired, Message: "Agent 必须由 Orchestrator 内部路由", Recoverable: true},
		})
	}
	return Failure(input, ErrInternalRoutingRequired, "Agent 必须由 Orchestrator 内部路由", true)
}
