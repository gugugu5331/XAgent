package command

import (
	"sort"
	"strings"
	"unicode"
)

func (r *Registry) Complete(input string) []Suggestion {
	if r == nil {
		return nil
	}
	value := strings.TrimLeftFunc(input, unicode.IsSpace)
	if !strings.HasPrefix(value, "/") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return nil
	}
	prefix := normalizeName(value)
	if index, ok := r.lookup[prefix]; ok && !r.definitions[index].Hidden {
		return []Suggestion{suggestionFrom(r.definitions[index])}
	}

	matches := make(map[string]Suggestion)
	for _, definition := range r.definitions {
		if definition.Hidden {
			continue
		}
		if strings.HasPrefix(definition.Name, prefix) {
			matches[definition.Name] = suggestionFrom(definition)
			continue
		}
		for _, alias := range definition.Aliases {
			if strings.HasPrefix(alias, prefix) {
				matches[definition.Name] = suggestionFrom(definition)
				break
			}
		}
	}
	names := make([]string, 0, len(matches))
	for name := range matches {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]Suggestion, 0, len(names))
	for _, name := range names {
		result = append(result, matches[name])
	}
	return result
}

func suggestionFrom(definition Definition) Suggestion {
	return Suggestion{
		Name:        definition.Name,
		Aliases:     append([]string(nil), definition.Aliases...),
		Description: definition.Description,
		ArgHint:     definition.ArgHint,
		Badge:       definition.Badge,
	}
}
