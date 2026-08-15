package tool

import (
	"encoding/json"
	"fmt"
)

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

// ToolDescriptor is a detached snapshot of registration-time metadata.
// Schema and TargetDigest are copied on every registry boundary.
type ToolDescriptor struct {
	Name         string
	Description  string
	Schema       Schema
	Risk         Risk
	Policy       ExecutionPolicy
	TargetDigest *[32]byte
}

// RegistrationOptions contains only locally trusted policy and target data.
// Remote annotations may remove local capabilities but can never add them.
type RegistrationOptions struct {
	Policy            ExecutionPolicy
	TargetDigest      *[32]byte
	RemoteAnnotations json.RawMessage
}

// SafeResultProducer marks a tool whose every publishable outcome is built by
// the injected ResultFactory boundary. Candidate registries reject tools that
// do not implement this closed assembly-time assertion.
type SafeResultProducer interface {
	Tool
	UsesSafeResultBoundary() bool
}

type Registry struct {
	tools         map[string]Definition
	executors     map[string]Tool
	descriptors   map[string]ToolDescriptor
	order         []string
	immutable     bool
	safeCandidate bool
}

// NewSafeCandidateRegistry returns an empty registry that accepts only tools
// carrying the injected safe-result boundary. Final Assembly remains the sole
// owner that populates it with one shared factory and fresh-Capture closures.
func NewSafeCandidateRegistry() *Registry {
	registry := newEmptyRegistry()
	registry.safeCandidate = true
	return registry
}

func NewRegistry(projectRoot string) (*Registry, error) {
	registry := newEmptyRegistry()
	defaults := []struct {
		tool   Tool
		policy ExecutionPolicy
	}{
		{tool: NewReadTool(projectRoot), policy: ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}},
		{tool: NewWriteTool(projectRoot)},
		{tool: NewEditTool(projectRoot)},
		{tool: NewBashTool(projectRoot)},
		{tool: NewGlobTool(projectRoot), policy: ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}},
		{tool: NewGrepTool(projectRoot), policy: ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}},
	}
	for _, item := range defaults {
		if err := registry.RegisterWithOptions(item.tool, RegistrationOptions{Policy: item.policy}); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func NewReadOnlyRegistry(projectRoot string) (*Registry, error) {
	registry := newEmptyRegistry()
	defaults := []Tool{
		NewReadTool(projectRoot),
		NewGlobTool(projectRoot),
		NewGrepTool(projectRoot),
	}
	for _, tool := range defaults {
		if err := registry.RegisterWithOptions(tool, RegistrationOptions{Policy: ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}}); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *Registry) Register(tool Tool) error {
	return r.RegisterWithOptions(tool, RegistrationOptions{})
}

func (r *Registry) RegisterWithOptions(tool Tool, options RegistrationOptions) error {
	if r == nil {
		return fmt.Errorf("工具注册中心不能为空")
	}
	if r.immutable {
		return fmt.Errorf("工具过滤视图不可修改")
	}
	if tool == nil {
		return fmt.Errorf("工具不能为空")
	}
	if r.safeCandidate {
		producer, ok := tool.(SafeResultProducer)
		if !ok || !producer.UsesSafeResultBoundary() {
			return fmt.Errorf("工具未使用安全结果边界")
		}
	}
	name := tool.Name()
	if name == "" {
		return fmt.Errorf("工具名称不能为空")
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("工具 %q 已注册", name)
	}
	schema := cloneSchema(tool.Schema())
	if err := validateSchemaDefinition(schema); err != nil {
		return fmt.Errorf("工具 %q schema 无效: %w", name, err)
	}
	policy, err := restrictPolicyWithRemoteAnnotations(options.Policy, options.RemoteAnnotations)
	if err != nil {
		return fmt.Errorf("工具 %q annotations 无效: %w", name, err)
	}
	if r.tools == nil {
		r.tools = make(map[string]Definition)
	}
	if r.descriptors == nil {
		r.descriptors = make(map[string]ToolDescriptor)
	}
	descriptor := ToolDescriptor{
		Name:         name,
		Description:  tool.Description(),
		Schema:       schema,
		Risk:         tool.Risk(),
		Policy:       policy,
		TargetDigest: cloneDigest(options.TargetDigest),
	}
	if r.executors == nil {
		r.executors = make(map[string]Tool)
	}
	r.tools[name] = &registeredTool{descriptor: cloneDescriptor(descriptor)}
	r.executors[name] = tool
	r.descriptors[name] = descriptor
	r.order = append(r.order, name)
	return nil
}

func newEmptyRegistry() *Registry {
	return &Registry{tools: make(map[string]Definition), executors: make(map[string]Tool), descriptors: make(map[string]ToolDescriptor)}
}

func (r *Registry) Get(name string) (Definition, bool) {
	if r == nil {
		return nil, false
	}
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *Registry) executionTool(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	tool, ok := r.executors[name]
	return tool, ok
}

// Names returns registered tool names in stable registration order.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.order...)
}

func (r *Registry) List() []Definition {
	if r == nil {
		return nil
	}
	tools := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		tools = append(tools, r.tools[name])
	}
	return tools
}

// Descriptor returns an independent registration-time metadata snapshot.
func (r *Registry) Descriptor(name string) (ToolDescriptor, bool) {
	if r == nil {
		return ToolDescriptor{}, false
	}
	descriptor, ok := r.descriptors[name]
	if !ok {
		return ToolDescriptor{}, false
	}
	return cloneDescriptor(descriptor), true
}

func (r *Registry) AnthropicDefinitions() []AnthropicDefinition {
	definitions := make([]AnthropicDefinition, 0, len(r.order))
	for _, name := range r.order {
		descriptor, ok := r.Descriptor(name)
		if !ok {
			continue
		}
		definitions = append(definitions, AnthropicDefinition{Name: descriptor.Name, Description: descriptor.Description, InputSchema: descriptor.Schema})
	}
	return definitions
}

func (r *Registry) OpenAIDefinitions() []OpenAIDefinition {
	definitions := make([]OpenAIDefinition, 0, len(r.order))
	for _, name := range r.order {
		descriptor, ok := r.Descriptor(name)
		if !ok {
			continue
		}
		definitions = append(definitions, OpenAIDefinition{
			Type:     "function",
			Function: OpenAIFunction{Name: descriptor.Name, Description: descriptor.Description, Parameters: descriptor.Schema},
		})
	}
	return definitions
}

func cloneDescriptor(descriptor ToolDescriptor) ToolDescriptor {
	descriptor.Schema = cloneSchema(descriptor.Schema)
	descriptor.TargetDigest = cloneDigest(descriptor.TargetDigest)
	return descriptor
}

func cloneSchema(schema Schema) Schema {
	cloned := Schema{
		Type:     schema.Type,
		Required: append([]string(nil), schema.Required...),
		Raw:      append(json.RawMessage(nil), schema.Raw...),
	}
	if schema.Properties != nil {
		cloned.Properties = make(map[string]SchemaProperty, len(schema.Properties))
		for name, property := range schema.Properties {
			property.Enum = append([]string(nil), property.Enum...)
			cloned.Properties[name] = property
		}
	}
	return cloned
}

func cloneDigest(digest *[32]byte) *[32]byte {
	if digest == nil {
		return nil
	}
	cloned := *digest
	return &cloned
}

func restrictPolicyWithRemoteAnnotations(policy ExecutionPolicy, raw json.RawMessage) (ExecutionPolicy, error) {
	if len(raw) == 0 {
		return policy, nil
	}
	var annotations struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
		IdempotentHint  *bool `json:"idempotentHint"`
		OpenWorldHint   *bool `json:"openWorldHint"`
	}
	if err := json.Unmarshal(raw, &annotations); err != nil {
		return ExecutionPolicy{}, fmt.Errorf("remote annotations must be one JSON object")
	}
	if annotations.ReadOnlyHint != nil && !*annotations.ReadOnlyHint {
		policy.ReadOnly = false
		policy.ConcurrentSafe = false
	}
	if annotations.DestructiveHint != nil && *annotations.DestructiveHint {
		policy.ReadOnly = false
		policy.ConcurrentSafe = false
	}
	if annotations.IdempotentHint != nil && !*annotations.IdempotentHint {
		policy.ConcurrentSafe = false
	}
	if annotations.OpenWorldHint != nil && *annotations.OpenWorldHint {
		policy.ConcurrentSafe = false
	}
	return policy, nil
}

type registeredTool struct {
	descriptor ToolDescriptor
}

func (t *registeredTool) Name() string {
	return t.descriptor.Name
}

func (t *registeredTool) Description() string {
	return t.descriptor.Description
}

func (t *registeredTool) Schema() Schema {
	return cloneSchema(t.descriptor.Schema)
}

func (t *registeredTool) Risk() Risk {
	return t.descriptor.Risk
}
