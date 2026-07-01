package orchestrator

import (
	"sort"

	"xagent/internal/provider"
	"xagent/internal/tool"
)

func toolDefinitionsFromRegistry(registry *tool.Registry) []provider.ToolDefinition {
	if registry == nil {
		return nil
	}
	tools := registry.List()
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })
	definitions := make([]provider.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		definitions = append(definitions, provider.ToolDefinition{Name: tool.Name(), Description: tool.Description(), Schema: tool.Schema()})
	}
	return definitions
}
