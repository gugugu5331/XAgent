package tool

import "context"

const LoadSkillToolName = "load_skill"

type loadSkillTool struct{}

// NewLoadSkillTool returns the system-level Skill loader definition. The
// orchestrator must intercept calls to this sentinel before normal execution.
func NewLoadSkillTool() Tool {
	return loadSkillTool{}
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

func (loadSkillTool) Execute(_ context.Context, input Input) Result {
	return Failure(input, ErrInternalRoutingRequired, "load_skill 必须由 Orchestrator 内部路由", true)
}
