package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"

	"xagent/internal/agentrole"
)

type FilterReason string

const (
	FilterRoleAllow      FilterReason = "role_allow"
	FilterRoleDeny       FilterReason = "role_deny"
	FilterGlobalDeny     FilterReason = "global_deny"
	FilterBackgroundDeny FilterReason = "background_deny"
	FilterPlanMode       FilterReason = "plan_mode"
	FilterRecursive      FilterReason = "recursive_delegate"
	FilterUnknown        FilterReason = "unknown_tool"
)

// CapabilitySet binds model-visible definitions and execution preflight to
// one sealed registry view. Rejections records the first monotonic filter that
// removed each parent capability.
type CapabilitySet struct {
	Registry    *Registry
	Names       []string
	Fingerprint string
	Rejections  map[string]FilterReason
}

func (c CapabilitySet) Allows(name string) bool {
	if c.Registry == nil {
		return false
	}
	_, ok := c.Registry.Get(name)
	return ok
}

// FilterReason returns an empty reason for an allowed tool, the recorded
// monotonic filter for a denied parent tool, or unknown_tool otherwise.
func (c CapabilitySet) FilterReason(name string) FilterReason {
	if c.Allows(name) {
		return ""
	}
	if reason, ok := c.Rejections[name]; ok {
		return reason
	}
	return FilterUnknown
}

func (c CapabilitySet) Clone() CapabilitySet {
	clone := c
	clone.Names = append([]string(nil), c.Names...)
	clone.Rejections = make(map[string]FilterReason, len(c.Rejections))
	for name, reason := range c.Rejections {
		clone.Rejections[name] = reason
	}
	return clone
}

// BuildCapabilitySet applies each policy exactly once, in security order. No
// later layer can add a name removed by an earlier layer.
func BuildCapabilitySet(
	parent *Registry,
	role *agentrole.Definition,
	background BackgroundPolicy,
	globalDenied map[string]struct{},
	planMode bool,
) (CapabilitySet, error) {
	if parent == nil {
		return CapabilitySet{}, fmt.Errorf("父工具视图不能为空")
	}
	if !parent.IsSealed() {
		return CapabilitySet{}, fmt.Errorf("父工具视图尚未封存")
	}
	background, err := normalizeBackgroundPolicy(parent, background)
	if err != nil {
		return CapabilitySet{}, err
	}

	var roleAllow map[string]struct{}
	roleDeny := make(map[string]struct{})
	if role != nil {
		if role.ToolAllow != nil {
			roleAllow = make(map[string]struct{}, len(role.ToolAllow))
			for _, name := range role.ToolAllow {
				roleAllow[name] = struct{}{}
			}
		}
		for _, name := range role.ToolDeny {
			roleDeny[name] = struct{}{}
		}
	}
	backgroundAllowed := make(map[string]struct{}, len(background.AllowedNames))
	for _, name := range background.AllowedNames {
		backgroundAllowed[name] = struct{}{}
	}

	allowed := make(map[string]struct{}, len(parent.Names()))
	rejections := make(map[string]FilterReason)
	for _, name := range parent.Names() {
		descriptor, ok := parent.Descriptor(name)
		if !ok {
			return CapabilitySet{}, fmt.Errorf("父工具 %q 元数据缺失", name)
		}
		if roleAllow != nil {
			if _, ok := roleAllow[name]; !ok {
				rejections[name] = FilterRoleAllow
				continue
			}
		}
		if _, denied := roleDeny[name]; denied {
			rejections[name] = FilterRoleDeny
			continue
		}
		if name == AgentToolName {
			rejections[name] = FilterRecursive
			continue
		}
		if _, globallyDenied := globalDenied[name]; globallyDenied {
			rejections[name] = FilterGlobalDeny
			continue
		}
		if background.Enabled {
			if _, ok := backgroundAllowed[name]; !ok {
				rejections[name] = FilterBackgroundDeny
				continue
			}
		}
		if planMode && descriptor.Route == RouteExecutor && !descriptor.Policy.ReadOnly {
			rejections[name] = FilterPlanMode
			continue
		}
		allowed[name] = struct{}{}
	}
	view, err := parent.View(ViewOptions{AllowedNames: allowed})
	if err != nil {
		return CapabilitySet{}, err
	}
	capabilities := CapabilitySet{
		Registry:   view,
		Names:      view.Names(),
		Rejections: rejections,
	}
	capabilities.Fingerprint = capabilityFingerprint(capabilities)
	return capabilities, nil
}

// BuildCapabilityViews prepares both placement snapshots from one sealed
// parent. Background filtering is deliberately disabled for the foreground
// snapshot and enabled only for the detached snapshot.
func BuildCapabilityViews(
	parent *Registry,
	role *agentrole.Definition,
	background BackgroundPolicy,
	globalDenied map[string]struct{},
	planMode bool,
) (CapabilitySet, CapabilitySet, error) {
	foregroundPolicy := background
	foregroundPolicy.Enabled = false
	foregroundPolicy.Fingerprint = ""
	foreground, err := BuildCapabilitySet(parent, role, foregroundPolicy, globalDenied, planMode)
	if err != nil {
		return CapabilitySet{}, CapabilitySet{}, err
	}
	background.Enabled = true
	background.Fingerprint = ""
	detached, err := BuildCapabilitySet(parent, role, background, globalDenied, planMode)
	if err != nil {
		return CapabilitySet{}, CapabilitySet{}, err
	}
	return foreground, detached, nil
}

func capabilityFingerprint(capabilities CapabilitySet) string {
	hash := sha256.New()
	for _, name := range capabilities.Names {
		_, _ = hash.Write([]byte("allow\x00" + name + "\x00"))
		if descriptor, ok := capabilities.Registry.Descriptor(name); ok {
			encoded, _ := json.Marshal(struct {
				Name        string
				Description string
				Schema      Schema
				Risk        Risk
				Route       ExecutionRoute
				Policy      ExecutionPolicy
				Workspace   WorkspacePolicy
			}{descriptor.Name, descriptor.Description, descriptor.Schema, descriptor.Risk, descriptor.Route, descriptor.Policy, descriptor.Workspace})
			_, _ = hash.Write(encoded)
			_, _ = hash.Write([]byte{0})
		}
	}
	denied := make([]string, 0, len(capabilities.Rejections))
	for name := range capabilities.Rejections {
		denied = append(denied, name)
	}
	sort.Strings(denied)
	for _, name := range denied {
		_, _ = hash.Write([]byte("deny\x00" + name + "\x00" + string(capabilities.Rejections[name]) + "\x00"))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// CapabilitySource is the placement-aware read boundary used by Provider
// request construction and final execution preflight.
type CapabilitySource interface {
	Current() CapabilitySet
}

type capabilitySnapshot struct {
	set CapabilitySet
}

// CapabilitySwitch atomically narrows a task from its prepared foreground
// view to its prepared background view. It has no reverse transition.
type CapabilitySwitch struct {
	foreground *capabilitySnapshot
	background *capabilitySnapshot
	active     atomic.Pointer[capabilitySnapshot]
}

func NewCapabilitySwitch(foreground, background CapabilitySet) (*CapabilitySwitch, error) {
	if foreground.Registry == nil || background.Registry == nil || !foreground.Registry.IsSealed() || !background.Registry.IsSealed() {
		return nil, fmt.Errorf("能力视图必须已封存")
	}
	if foreground.Registry.lineage == nil || foreground.Registry.lineage != background.Registry.lineage {
		return nil, fmt.Errorf("前后台能力视图不属于同一注册快照")
	}
	for _, name := range background.Names {
		if !foreground.Allows(name) {
			return nil, fmt.Errorf("后台能力 %q 扩大了前台能力", name)
		}
	}
	switcher := &CapabilitySwitch{
		foreground: &capabilitySnapshot{set: foreground.Clone()},
		background: &capabilitySnapshot{set: background.Clone()},
	}
	switcher.active.Store(switcher.foreground)
	return switcher, nil
}

func (s *CapabilitySwitch) Current() CapabilitySet {
	if s == nil {
		return CapabilitySet{}
	}
	current := s.active.Load()
	if current == nil {
		return CapabilitySet{}
	}
	return current.set.Clone()
}

func (s *CapabilitySwitch) MoveToBackground() (bool, CapabilitySet) {
	if s == nil {
		return false, CapabilitySet{}
	}
	changed := s.active.CompareAndSwap(s.foreground, s.background)
	return changed, s.Current()
}

type ViewOptions struct {
	// AllowedNames is nil when no allowlist restriction applies. A non-nil,
	// empty map intentionally hides every ordinary tool.
	AllowedNames map[string]struct{}
	// AlwaysInclude names are added after filtering, in the order supplied.
	AlwaysInclude []string
	ReadOnly      bool
}

// View returns a sealed, monotonically filtered registry that reuses the
// underlying Tool instances. It never mutates the source registry.
func (r *Registry) View(options ViewOptions) (*Registry, error) {
	if r == nil {
		return nil, fmt.Errorf("工具注册中心不能为空")
	}
	if !r.sealed {
		return nil, fmt.Errorf("工具注册中心尚未封存")
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

	view := &Registry{tools: make(map[string]Definition), executors: make(map[string]Tool), descriptors: make(map[string]ToolDescriptor), workspaceBinders: make(map[string]WorkspaceBinder), order: make([]string, 0), lineage: r.lineage, immutable: true, sealed: true}
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
		if binder, ok := r.workspaceBinders[name]; ok {
			view.workspaceBinders[name] = binder
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
			if !ok || (descriptor.Route != RouteSystem && !descriptor.Policy.ReadOnly) {
				continue
			}
		}
		if registered != nil {
			add(name)
		}
	}
	for _, name := range options.AlwaysInclude {
		// AlwaysInclude is retained for source compatibility, but it may only
		// affect the order of a tool that already survived every filter.
		// In particular, it cannot reopen a denied system tool.
		if options.AllowedNames != nil {
			if _, ok := options.AllowedNames[name]; !ok {
				continue
			}
		}
		descriptor, ok := r.descriptors[name]
		if options.ReadOnly && (!ok || (descriptor.Route != RouteSystem && !descriptor.Policy.ReadOnly)) {
			continue
		}
		add(name)
	}
	return view, nil
}
