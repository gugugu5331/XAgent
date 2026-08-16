package tool

import (
	"context"
	"errors"
)

const LoadSkillToolName = "load_skill"

type loadSkillTool struct {
	resultFactory *ResultFactory
}

// NewLoadSkillTool returns the system-level Skill loader definition. The
// orchestrator must intercept calls to this sentinel before normal execution.
func NewLoadSkillTool() Tool {
	return loadSkillTool{}
}

func NewLoadSkillToolWithResultFactory(factory *ResultFactory) (Tool, error) {
	if factory == nil {
		return nil, errors.New("safe load-skill result factory is unavailable")
	}
	return loadSkillTool{resultFactory: factory}, nil
}

func (loadSkillTool) Name() string { return LoadSkillToolName }

func (loadSkillTool) Description() string {
	return "Load a reusable Skill by name and activate its full instructions. Use args to pass the user's complete argument text."
}

func (loadSkillTool) Schema() Schema {
	return ObjectSchema([]string{"name"}, map[string]SchemaProperty{
		"name": StringProperty("Canonical name of the Skill to load."),
		"args": StringProperty("Optional complete argument text to substitute into the Skill instructions."),
	})
}

func (loadSkillTool) Risk() Risk { return RiskSafe }

func (loadSkillTool) executionRoute() ExecutionRoute { return RouteSystem }

func (t loadSkillTool) UsesSafeResultBoundary() bool { return t.resultFactory != nil }

func (t loadSkillTool) Execute(_ context.Context, input Input) Result {
	if t.resultFactory != nil {
		return buildSyntheticResult(t.resultFactory, ResultFactoryInput{
			CallID: input.CallID, Name: input.Name, State: Rejected, Status: StatusError,
			Summary: "load_skill 需要内部路由",
			Error:   &Error{Code: ErrInternalRoutingRequired, Message: "load_skill 必须由 Orchestrator 内部路由", Recoverable: true},
		})
	}
	return Failure(input, ErrInternalRoutingRequired, "load_skill 必须由 Orchestrator 内部路由", true)
}
