package prompt

// Scope 标识 Prompt 块与父会话或工作区的绑定范围。
// 该元数据必须显式提供，调用方不得根据块名称猜测。
type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
	ScopeRuntime Scope = "runtime"
)

func (scope Scope) Valid() bool {
	switch scope {
	case ScopeGlobal, ScopeUser, ScopeProject, ScopeRuntime:
		return true
	default:
		return false
	}
}

type Section struct {
	Name     string
	Priority int
	Content  string
	Stable   bool
	Scope    Scope
}

type RunMode string

const (
	SkillCatalogBlockName = "skill-catalog"
	ActiveSkillsBlockName = "active-skills"
)

const (
	RunModeDefault RunMode = "default"
	RunModePlan    RunMode = "plan"
	RunModeDo      RunMode = "do"
)

type BuildRequest struct {
	Mode                   RunMode
	Iteration              int
	ProjectRoot            string
	HookBlocks             []Block
	SkillCatalog           string
	ActiveSkills           string
	OptionalStableSections []Section
}

type Block struct {
	Name    string
	Content string
	Stable  bool
	Scope   Scope
}

type Bundle struct {
	StableBlocks         []Block
	DynamicBlocks        []Block
	OrderedBlocks        []Block
	SystemBreakpointName string
}

// UsesOrderedBlocks reports whether this bundle must preserve individual
// system block boundaries. Bundles without Hook blocks keep the legacy
// StableBlocks/DynamicBlocks path byte-for-byte compatible.
func (b Bundle) UsesOrderedBlocks() bool {
	return len(b.OrderedBlocks) > 0
}
