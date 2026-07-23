package prompt

import (
	"sort"
	"strings"
)

func Build(req BuildRequest) Bundle {
	fixed := fixedStableBlocks()
	optional := optionalStableBlocks(req.OptionalStableSections)
	stable := make([]Block, 0, len(fixed)+len(optional)+1)
	stable = append(stable, fixed...)
	stable = append(stable, optional...)
	if catalog := strings.TrimSpace(req.SkillCatalog); catalog != "" {
		stable = append(stable, Block{Name: SkillCatalogBlockName, Content: catalog, Stable: true})
	}
	dynamic := DynamicBlocks(req)
	bundle := Bundle{StableBlocks: stable, DynamicBlocks: dynamic}

	hooks := hookBlocks(req.HookBlocks)
	if len(hooks) == 0 {
		return bundle
	}
	ordered := make([]Block, 0, len(fixed)+len(hooks)+len(optional)+1+len(dynamic))
	ordered = append(ordered, fixed...)
	ordered = append(ordered, hooks...)
	ordered = append(ordered, optional...)
	if catalog := strings.TrimSpace(req.SkillCatalog); catalog != "" {
		ordered = append(ordered, Block{Name: SkillCatalogBlockName, Content: catalog, Stable: true})
	}
	ordered = append(ordered, dynamic...)
	bundle.OrderedBlocks = ordered
	bundle.SystemBreakpointName = fixed[len(fixed)-1].Name
	return bundle
}

func StableSections(optional []Section) []Section {
	sections := append([]Section{}, fixedStableSections()...)
	sections = append(sections, normalizedOptionalStableSections(optional)...)
	return sections
}

func normalizedOptionalStableSections(optional []Section) []Section {
	seen := map[string]bool{}
	filtered := make([]Section, 0, len(optional))
	for _, section := range optional {
		section.Content = strings.TrimSpace(section.Content)
		section.Name = strings.TrimSpace(section.Name)
		if section.Name == "" || section.Content == "" || !section.Stable || seen[section.Name] {
			continue
		}
		seen[section.Name] = true
		filtered = append(filtered, section)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Priority == filtered[j].Priority {
			return filtered[i].Name < filtered[j].Name
		}
		return filtered[i].Priority < filtered[j].Priority
	})
	return filtered
}

func fixedStableBlocks() []Block {
	return sectionBlocks(fixedStableSections())
}

func optionalStableBlocks(optional []Section) []Block {
	return sectionBlocks(normalizedOptionalStableSections(optional))
}

func sectionBlocks(sections []Section) []Block {
	blocks := make([]Block, 0, len(sections))
	for _, section := range sections {
		content := strings.TrimSpace(section.Content)
		if content == "" {
			continue
		}
		blocks = append(blocks, Block{Name: section.Name, Content: content, Stable: true})
	}
	return blocks
}

func hookBlocks(blocks []Block) []Block {
	result := make([]Block, 0, len(blocks))
	for _, block := range blocks {
		block.Name = strings.TrimSpace(block.Name)
		block.Content = strings.TrimSpace(block.Content)
		if block.Content == "" {
			continue
		}
		block.Stable = false
		result = append(result, block)
	}
	return result
}

func JoinBlocks(blocks []Block) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		content := strings.TrimSpace(block.Content)
		if content != "" {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "\n\n")
}
