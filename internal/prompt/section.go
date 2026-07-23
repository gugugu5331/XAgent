package prompt

type Section struct {
	Name     string
	Priority int
	Content  string
	Stable   bool
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
