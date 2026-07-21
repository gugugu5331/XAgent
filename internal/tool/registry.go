package tool

import "fmt"

type AnthropicDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema Schema `json:"input_schema"`
}

type OpenAIDefinition struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  Schema `json:"parameters"`
}

type Registry struct {
	tools     map[string]Tool
	order     []string
	immutable bool
}

func NewRegistry(projectRoot string) (*Registry, error) {
	registry := &Registry{tools: map[string]Tool{}}
	defaults := []Tool{
		NewReadTool(projectRoot),
		NewWriteTool(projectRoot),
		NewEditTool(projectRoot),
		NewBashTool(projectRoot),
		NewGlobTool(projectRoot),
		NewGrepTool(projectRoot),
	}
	for _, tool := range defaults {
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func NewReadOnlyRegistry(projectRoot string) (*Registry, error) {
	registry := &Registry{tools: map[string]Tool{}}
	defaults := []Tool{
		NewReadTool(projectRoot),
		NewGlobTool(projectRoot),
		NewGrepTool(projectRoot),
	}
	for _, tool := range defaults {
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *Registry) Register(tool Tool) error {
	if r == nil {
		return fmt.Errorf("工具注册中心不能为空")
	}
	if r.immutable {
		return fmt.Errorf("工具过滤视图不可修改")
	}
	if tool == nil {
		return fmt.Errorf("工具不能为空")
	}
	name := tool.Name()
	if name == "" {
		return fmt.Errorf("工具名称不能为空")
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("工具 %q 已注册", name)
	}
	r.tools[name] = tool
	r.order = append(r.order, name)
	return nil
}

func (r *Registry) Get(name string) (Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

// Names returns registered tool names in stable registration order.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.order...)
}

func (r *Registry) List() []Tool {
	if r == nil {
		return nil
	}
	tools := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		tools = append(tools, r.tools[name])
	}
	return tools
}

func (r *Registry) AnthropicDefinitions() []AnthropicDefinition {
	definitions := make([]AnthropicDefinition, 0, len(r.order))
	for _, tool := range r.List() {
		definitions = append(definitions, AnthropicDefinition{Name: tool.Name(), Description: tool.Description(), InputSchema: tool.Schema()})
	}
	return definitions
}

func (r *Registry) OpenAIDefinitions() []OpenAIDefinition {
	definitions := make([]OpenAIDefinition, 0, len(r.order))
	for _, tool := range r.List() {
		definitions = append(definitions, OpenAIDefinition{
			Type:     "function",
			Function: OpenAIFunction{Name: tool.Name(), Description: tool.Description(), Parameters: tool.Schema()},
		})
	}
	return definitions
}
