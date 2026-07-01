package prompt

type Section struct {
	Name     string
	Priority int
	Content  string
	Stable   bool
}

type RunMode string

const (
	RunModeDefault RunMode = "default"
	RunModePlan    RunMode = "plan"
	RunModeDo      RunMode = "do"
)

type BuildRequest struct {
	Mode                   RunMode
	Iteration              int
	ProjectRoot            string
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
