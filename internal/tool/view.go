package tool

import "fmt"

type ViewOptions struct {
	// AllowedNames is nil when no allowlist restriction applies. A non-nil,
	// empty map intentionally hides every ordinary tool.
	AllowedNames map[string]struct{}
	// AlwaysInclude names are added after filtering, in the order supplied.
	AlwaysInclude []string
	ReadOnly      bool
}

// View returns an immutable filtered registry that reuses the underlying Tool
// instances. It never mutates the source registry.
func (r *Registry) View(options ViewOptions) (*Registry, error) {
	if r == nil {
		return nil, fmt.Errorf("工具注册中心不能为空")
	}
	for name := range options.AllowedNames {
		if _, ok := r.tools[name]; !ok {
			return nil, fmt.Errorf("允许的工具 %q 未注册", name)
		}
	}
	for _, name := range options.AlwaysInclude {
		if _, ok := r.tools[name]; !ok {
			return nil, fmt.Errorf("始终保留的工具 %q 未注册", name)
		}
	}

	view := &Registry{tools: make(map[string]Definition), executors: make(map[string]Tool), descriptors: make(map[string]ToolDescriptor), immutable: true}
	always := make(map[string]struct{}, len(options.AlwaysInclude))
	for _, name := range options.AlwaysInclude {
		always[name] = struct{}{}
	}
	add := func(name string) {
		if _, exists := view.tools[name]; exists {
			return
		}
		view.tools[name] = r.tools[name]
		view.executors[name] = r.executors[name]
		if descriptor, ok := r.descriptors[name]; ok {
			view.descriptors[name] = cloneDescriptor(descriptor)
		}
		view.order = append(view.order, name)
	}
	for _, name := range r.order {
		if _, includeLast := always[name]; includeLast {
			continue
		}
		registered := r.tools[name]
		if options.AllowedNames != nil {
			if _, ok := options.AllowedNames[name]; !ok {
				continue
			}
		}
		if options.ReadOnly {
			descriptor, ok := r.descriptors[name]
			if !ok || !descriptor.Policy.ReadOnly {
				continue
			}
		}
		if registered != nil {
			add(name)
		}
	}
	for _, name := range options.AlwaysInclude {
		add(name)
	}
	return view, nil
}
