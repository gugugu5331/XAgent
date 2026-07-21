package command

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

type Registry struct {
	definitions []Definition
	lookup      map[string]int
}

func New(definitions ...Definition) (*Registry, error) {
	items := make([]Definition, len(definitions))
	for index, definition := range definitions {
		item, err := normalizeDefinition(definition)
		if err != nil {
			return nil, fmt.Errorf("命令定义 %d 无效: %w", index+1, err)
		}
		items[index] = item
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	lookup := make(map[string]int)
	type owner struct {
		name  string
		index int
	}
	owners := make(map[string]owner)
	for index, item := range items {
		keys := append([]string{item.Name}, item.Aliases...)
		for _, key := range keys {
			if previous, exists := owners[key]; exists {
				return nil, fmt.Errorf("命令名称或别名冲突 %q: /%s（定义 %d）与 /%s（定义 %d）", key, previous.name, previous.index+1, item.Name, index+1)
			}
			owners[key] = owner{name: item.Name, index: index}
			lookup[key] = index
		}
	}
	return &Registry{definitions: items, lookup: lookup}, nil
}

func MustNew(definitions ...Definition) *Registry {
	registry, err := New(definitions...)
	if err != nil {
		panic(err)
	}
	return registry
}

func (r *Registry) Visible() []Definition {
	if r == nil {
		return nil
	}
	visible := make([]Definition, 0, len(r.definitions))
	for _, definition := range r.definitions {
		if !definition.Hidden {
			visible = append(visible, cloneDefinition(definition))
		}
	}
	return visible
}

// Definitions returns an immutable snapshot of all registered command
// definitions, including hidden compatibility commands.
func (r *Registry) Definitions() []Definition {
	if r == nil {
		return nil
	}
	definitions := make([]Definition, len(r.definitions))
	for index, definition := range r.definitions {
		definitions[index] = cloneDefinition(definition)
	}
	return definitions
}

func normalizeDefinition(definition Definition) (Definition, error) {
	definition.Name = normalizeName(definition.Name)
	if definition.Name == "" {
		return Definition{}, fmt.Errorf("名称不能为空")
	}
	if containsWhitespace(definition.Name) {
		return Definition{}, fmt.Errorf("名称不能包含空白: %q", definition.Name)
	}
	if !validType(definition.Type) {
		return Definition{}, fmt.Errorf("命令类型无效: %q", definition.Type)
	}
	if definition.Handler == nil {
		return Definition{}, fmt.Errorf("/%s 缺少处理函数", definition.Name)
	}
	aliases := make([]string, len(definition.Aliases))
	seen := map[string]struct{}{definition.Name: {}}
	for index, alias := range definition.Aliases {
		alias = normalizeName(alias)
		if alias == "" {
			return Definition{}, fmt.Errorf("/%s 的别名不能为空", definition.Name)
		}
		if containsWhitespace(alias) {
			return Definition{}, fmt.Errorf("/%s 的别名不能包含空白: %q", definition.Name, alias)
		}
		if _, exists := seen[alias]; exists {
			return Definition{}, fmt.Errorf("/%s 内部名称或别名重复: %q", definition.Name, alias)
		}
		seen[alias] = struct{}{}
		aliases[index] = alias
	}
	definition.Aliases = aliases
	definition.Description = strings.TrimSpace(definition.Description)
	definition.Usage = strings.TrimSpace(definition.Usage)
	definition.ArgHint = strings.TrimSpace(definition.ArgHint)
	definition.Badge = strings.TrimSpace(definition.Badge)
	return definition, nil
}

func normalizeName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "/")
	return strings.ToLower(value)
}

func containsWhitespace(value string) bool {
	return strings.IndexFunc(value, unicode.IsSpace) >= 0
}

func validType(value Type) bool {
	return value == TypeLocal || value == TypeUI || value == TypePrompt
}

func cloneDefinition(definition Definition) Definition {
	definition.Aliases = append([]string(nil), definition.Aliases...)
	return definition
}
