package tool

import (
	"encoding/json"
	"fmt"
	"reflect"
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
	Route        ExecutionRoute
	Policy       ExecutionPolicy
	Workspace    WorkspacePolicy
	TargetDigest *[32]byte
}

// RegistrationOptions contains only locally trusted policy and target data.
// Remote annotations may remove local capabilities but can never add them.
type RegistrationOptions struct {
	Route             ExecutionRoute
	Policy            ExecutionPolicy
	Workspace         WorkspacePolicy
	WorkspaceBinder   WorkspaceBinder
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
	tools            map[string]Definition
	executors        map[string]Tool
	descriptors      map[string]ToolDescriptor
	workspaceBinders map[string]WorkspaceBinder
	order            []string
	lineage          *registryLineage
	immutable        bool
	sealed           bool
	safeCandidate    bool
}

// registryLineage is an opaque identity shared only by monotonic views made
// from the same registration snapshot. It prevents an atomic placement switch
// from swapping in same-named tools backed by different execution targets.
type registryLineage struct {
	identity byte
	names    map[string]struct{}
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
		{tool: NewReadTool(projectRoot), policy: ExecutionPolicy{ReadOnly: true, SideEffectFree: true, ConcurrentSafe: true}},
		{tool: NewWriteTool(projectRoot)},
		{tool: NewEditTool(projectRoot)},
		{tool: NewBashTool(projectRoot)},
		{tool: NewGlobTool(projectRoot), policy: ExecutionPolicy{ReadOnly: true, SideEffectFree: true, ConcurrentSafe: true}},
		{tool: NewGrepTool(projectRoot), policy: ExecutionPolicy{ReadOnly: true, SideEffectFree: true, ConcurrentSafe: true}},
	}
	for _, item := range defaults {
		if err := registry.RegisterWithOptions(item.tool, builtinRegistrationOptions(item.tool, item.policy)); err != nil {
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
		if err := registry.RegisterWithOptions(tool, builtinRegistrationOptions(tool, ExecutionPolicy{ReadOnly: true, SideEffectFree: true, ConcurrentSafe: true})); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func builtinRegistrationOptions(target Tool, policy ExecutionPolicy) RegistrationOptions {
	options, _ := applyBuiltinWorkspaceDefaults(target, RegistrationOptions{Policy: policy})
	return options
}

func applyBuiltinWorkspaceDefaults(target Tool, options RegistrationOptions) (RegistrationOptions, error) {
	switch target.(type) {
	case loadSkillTool, agentTool:
		if options.Workspace.Mode != WorkspaceUnknown && options.Workspace.Mode != WorkspaceFixed {
			return RegistrationOptions{}, fmt.Errorf("system tool workspace policy must be fixed")
		}
		if options.WorkspaceBinder != nil {
			return RegistrationOptions{}, fmt.Errorf("system tool workspace binder is invalid")
		}
		options.Workspace = WorkspacePolicy{Mode: WorkspaceFixed}
	case *ReadTool, *GlobTool, *GrepTool:
		if options.Workspace.Mode == WorkspaceUnknown && options.WorkspaceBinder == nil {
			options.Workspace = WorkspacePolicy{Mode: WorkspaceContextual}
			options.WorkspaceBinder = target.(WorkspaceBinder)
		}
	case *WriteTool, *EditTool:
		if options.Workspace.Mode == WorkspaceUnknown && options.WorkspaceBinder == nil {
			options.Workspace = WorkspacePolicy{Mode: WorkspaceContextual, WriteContainment: true}
			options.WorkspaceBinder = target.(WorkspaceBinder)
		}
	case *BashTool:
		if options.Workspace.Mode == WorkspaceUnknown && options.WorkspaceBinder == nil {
			// A protected task Bash requires a task-owned protection plan and root.
			// The assembly-root instance cannot manufacture either, so it remains
			// fixed until Workspace Factory supplies an explicit trusted binder.
			options.Workspace = WorkspacePolicy{Mode: WorkspaceFixed}
		}
	}
	return options, nil
}

func (r *Registry) Register(tool Tool) error {
	return r.RegisterWithOptions(tool, RegistrationOptions{})
}

func (r *Registry) RegisterWithOptions(tool Tool, options RegistrationOptions) error {
	if r == nil {
		return fmt.Errorf("工具注册中心不能为空")
	}
	if r.immutable {
		return fmt.Errorf("工具注册中心已封存")
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
	var err error
	options, err = applyBuiltinWorkspaceDefaults(tool, options)
	if err != nil {
		return fmt.Errorf("工具 %q workspace metadata 无效: %w", name, err)
	}
	schema := cloneSchema(tool.Schema())
	if err := validateSchemaDefinition(schema); err != nil {
		return fmt.Errorf("工具 %q schema 无效: %w", name, err)
	}
	policy, err := restrictPolicyWithRemoteAnnotations(options.Policy, options.RemoteAnnotations)
	if err != nil {
		return fmt.Errorf("工具 %q annotations 无效: %w", name, err)
	}
	if err := options.Workspace.validate(); err != nil {
		return fmt.Errorf("工具 %q workspace policy 无效: %w", name, err)
	}
	if options.Workspace.Mode == WorkspaceContextual && options.WorkspaceBinder == nil {
		return fmt.Errorf("工具 %q contextual workspace binder 缺失", name)
	}
	if options.Workspace.Mode != WorkspaceContextual && options.WorkspaceBinder != nil {
		return fmt.Errorf("工具 %q 非 contextual workspace binder 无效", name)
	}
	route := options.Route
	if routed, ok := tool.(interface{ executionRoute() ExecutionRoute }); ok {
		route = routed.executionRoute()
	}
	if !route.valid() {
		return fmt.Errorf("工具 %q route 无效", name)
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
		Route:        route,
		Policy:       policy,
		Workspace:    options.Workspace,
		TargetDigest: cloneDigest(options.TargetDigest),
	}
	if r.executors == nil {
		r.executors = make(map[string]Tool)
	}
	r.tools[name] = &registeredTool{descriptor: cloneDescriptor(descriptor)}
	r.executors[name] = tool
	r.descriptors[name] = descriptor
	if options.WorkspaceBinder != nil {
		if r.workspaceBinders == nil {
			r.workspaceBinders = make(map[string]WorkspaceBinder)
		}
		r.workspaceBinders[name] = options.WorkspaceBinder
	}
	r.order = append(r.order, name)
	return nil
}

// Seal performs the one registration-to-execution transition. It rejects a
// tool whose public definition changed after Register, then makes metadata,
// ordering and execution-target associations immutable.
func (r *Registry) Seal() error {
	if r == nil {
		return fmt.Errorf("工具注册中心不能为空")
	}
	if r.sealed || r.immutable {
		return fmt.Errorf("工具注册中心已经封存")
	}
	for _, name := range r.order {
		executor, ok := r.executors[name]
		if !ok || executor == nil {
			return fmt.Errorf("工具 %q execution target drift", name)
		}
		descriptor, ok := r.descriptors[name]
		if !ok || executor.Name() != descriptor.Name || executor.Description() != descriptor.Description || executor.Risk() != descriptor.Risk || !reflect.DeepEqual(cloneSchema(executor.Schema()), descriptor.Schema) {
			return fmt.Errorf("工具 %q definition drift", name)
		}
	}
	r.sealed = true
	r.immutable = true
	if r.lineage == nil {
		r.lineage = &registryLineage{}
	}
	r.lineage.names = make(map[string]struct{}, len(r.order))
	for _, name := range r.order {
		r.lineage.names[name] = struct{}{}
	}
	return nil
}

// IsSealed reports whether registration has permanently ended. Filtered
// views are sealed at construction.
func (r *Registry) IsSealed() bool {
	return r != nil && r.sealed
}

func (r *Registry) knowsRegisteredName(name string) bool {
	if r == nil {
		return false
	}
	if _, ok := r.tools[name]; ok {
		return true
	}
	if r.lineage == nil {
		return false
	}
	_, ok := r.lineage.names[name]
	return ok
}

func newEmptyRegistry() *Registry {
	return &Registry{tools: make(map[string]Definition), executors: make(map[string]Tool), descriptors: make(map[string]ToolDescriptor), workspaceBinders: make(map[string]WorkspaceBinder), lineage: &registryLineage{}}
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
	names := make([]string, len(r.order))
	copy(names, r.order)
	return names
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
		policy.SideEffectFree = false
		policy.ConcurrentSafe = false
	}
	if annotations.DestructiveHint != nil && *annotations.DestructiveHint {
		policy.ReadOnly = false
		policy.SideEffectFree = false
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
