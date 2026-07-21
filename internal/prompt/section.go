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
	StableBlocks  []Block
	DynamicBlocks []Block
}
