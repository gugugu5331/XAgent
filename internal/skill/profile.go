package skill

import (
	"fmt"
	"sort"
	"strings"
)

type ExecutionProfile struct {
	Catalog          []CatalogItem
	Activity         ActivitySnapshot
	Model            string
	AllowedTools     map[string]struct{}
	ReadRoots        []string
	IndependentDepth int
	Persist          bool
	UpdateMemory     bool
}

type ProfileRequest struct {
	Snapshot         Snapshot
	Activity         ActivitySnapshot
	DefaultModel     string
	BaseTools        []string
	ReadOnly         bool
	IndependentDepth int
}

func BuildProfile(request ProfileRequest) (ExecutionProfile, error) {
	if request.IndependentDepth < 0 {
		return ExecutionProfile{}, fmt.Errorf("independent depth cannot be negative")
	}
	base := make(map[string]struct{}, len(request.BaseTools))
	for _, name := range request.BaseTools {
		if strings.TrimSpace(name) == "" {
			return ExecutionProfile{}, fmt.Errorf("base tool name cannot be empty")
		}
		base[name] = struct{}{}
	}

	restricted := request.Activity.AllowedTools != nil || request.ReadOnly
	var allowed map[string]struct{}
	if restricted {
		allowed = map[string]struct{}{}
		for name := range base {
			if request.Activity.AllowedTools != nil && !containsString(request.Activity.AllowedTools, name) {
				continue
			}
			if request.ReadOnly && !isReadOnlyTool(name) {
				continue
			}
			allowed[name] = struct{}{}
		}
		allowed[LoadSkillToolName] = struct{}{}
	}

	model := strings.TrimSpace(request.Activity.Model)
	if model == "" {
		model = strings.TrimSpace(request.DefaultModel)
	}
	isolated := request.IndependentDepth > 0
	profile := ExecutionProfile{
		Catalog:          append([]CatalogItem(nil), request.Snapshot.Catalog...),
		Activity:         cloneActivitySnapshot(request.Activity),
		Model:            model,
		AllowedTools:     cloneStringSet(allowed),
		ReadRoots:        sortedUniqueStrings(request.Activity.ReadRoots),
		IndependentDepth: request.IndependentDepth,
		Persist:          !isolated,
		UpdateMemory:     !isolated,
	}
	return profile, nil
}

func (p ExecutionProfile) Clone() ExecutionProfile {
	return ExecutionProfile{
		Catalog:          append([]CatalogItem(nil), p.Catalog...),
		Activity:         cloneActivitySnapshot(p.Activity),
		Model:            p.Model,
		AllowedTools:     cloneStringSet(p.AllowedTools),
		ReadRoots:        append([]string(nil), p.ReadRoots...),
		IndependentDepth: p.IndependentDepth,
		Persist:          p.Persist,
		UpdateMemory:     p.UpdateMemory,
	}
}

func containsString(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	if index < len(values) && values[index] == target {
		return true
	}
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func isReadOnlyTool(name string) bool {
	switch name {
	case "Read", "Glob", "Grep":
		return true
	default:
		return false
	}
}

func cloneStringSet(source map[string]struct{}) map[string]struct{} {
	if source == nil {
		return nil
	}
	clone := make(map[string]struct{}, len(source))
	for value := range source {
		clone[value] = struct{}{}
	}
	return clone
}

func sortedUniqueStrings(values []string) []string {
	if values == nil {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
