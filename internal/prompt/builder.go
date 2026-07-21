package prompt

import (
	"sort"
	"strings"
)

func Build(req BuildRequest) Bundle {
	stable := stableBlocks(req.OptionalStableSections)
	if catalog := strings.TrimSpace(req.SkillCatalog); catalog != "" {
		stable = append(stable, Block{Name: SkillCatalogBlockName, Content: catalog, Stable: true})
	}
	return Bundle{StableBlocks: stable, DynamicBlocks: DynamicBlocks(req)}
}

func StableSections(optional []Section) []Section {
	sections := append([]Section{}, fixedStableSections()...)
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
	sections = append(sections, filtered...)
	return sections
}

func stableBlocks(optional []Section) []Block {
	sections := StableSections(optional)
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
