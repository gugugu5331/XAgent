package skill

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func ParseWithLimits(data []byte, limits Limits) (Metadata, string, error) {
	limits = normalizeLimits(limits)
	if int64(len(data)) > limits.MaxEntryBytes {
		return Metadata{}, "", fmt.Errorf("skill entry exceeds %d bytes", limits.MaxEntryBytes)
	}
	frontmatter, body, err := splitFrontmatter(data)
	if err != nil {
		return Metadata{}, "", err
	}
	if hasStandaloneYAMLDocumentEnd(frontmatter) {
		return Metadata{}, "", fmt.Errorf("skill frontmatter must contain one YAML document")
	}
	if len(body) > limits.MaxBodyBytes {
		return Metadata{}, "", fmt.Errorf("skill body exceeds %d bytes", limits.MaxBodyBytes)
	}
	if strings.TrimSpace(body) == "" {
		return Metadata{}, "", fmt.Errorf("skill body is empty")
	}
	var document yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(frontmatter)).Decode(&document); err != nil {
		return Metadata{}, "", fmt.Errorf("skill frontmatter is invalid or contains unknown fields")
	}
	historySet, err := validateFrontmatterHistory(document)
	if err != nil {
		return Metadata{}, "", err
	}

	var metadata Metadata
	decoder := yaml.NewDecoder(bytes.NewReader(frontmatter))
	decoder.KnownFields(true)
	if err := decoder.Decode(&metadata); err != nil {
		return Metadata{}, "", fmt.Errorf("skill frontmatter is invalid or contains unknown fields")
	}
	metadata.historySet = historySet
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Metadata{}, "", fmt.Errorf("skill frontmatter must contain one YAML document")
	}
	metadata, err = ValidateMetadata(metadata)
	if err != nil {
		return Metadata{}, "", err
	}
	return metadata, body, nil
}

func hasStandaloneYAMLDocumentEnd(frontmatter []byte) bool {
	for _, line := range bytes.Split(frontmatter, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.Equal(line, []byte("...")) {
			return true
		}
	}
	return false
}

func ValidateMetadata(metadata Metadata) (Metadata, error) {
	metadata.Name = strings.ToLower(strings.TrimSpace(metadata.Name))
	if !skillNamePattern.MatchString(metadata.Name) {
		return Metadata{}, fmt.Errorf("skill name must match [a-z0-9][a-z0-9_-]{0,63}")
	}
	metadata.Description = strings.TrimSpace(metadata.Description)
	if metadata.Description == "" {
		return Metadata{}, fmt.Errorf("skill description is required")
	}
	metadata.Mode = normalizeSkillMode(metadata.Mode)
	switch metadata.Mode {
	case ModeShared, ModeIsolated:
	default:
		return Metadata{}, fmt.Errorf("skill mode must be shared or isolated")
	}
	if err := validateSkillHistory(metadata.History, metadata.Mode); err != nil {
		return Metadata{}, err
	}
	metadata.Model = strings.TrimSpace(metadata.Model)

	toolSet := make(map[string]struct{}, len(metadata.AllowedTools))
	tools := make([]string, 0, len(metadata.AllowedTools))
	for _, toolName := range metadata.AllowedTools {
		toolName = strings.TrimSpace(toolName)
		if toolName == "" {
			return Metadata{}, fmt.Errorf("allowed tool name cannot be empty")
		}
		if _, exists := toolSet[toolName]; exists {
			continue
		}
		toolSet[toolName] = struct{}{}
		tools = append(tools, toolName)
	}
	sort.Strings(tools)
	if len(tools) == 0 {
		metadata.AllowedTools = nil
	} else {
		metadata.AllowedTools = tools
	}
	return metadata, nil
}

func splitFrontmatter(data []byte) ([]byte, string, error) {
	if len(data) < 4 || !bytes.HasPrefix(data, []byte("---\n")) && !bytes.HasPrefix(data, []byte("---\r\n")) {
		return nil, "", fmt.Errorf("skill entry must start with a YAML frontmatter delimiter")
	}
	firstEnd := bytes.IndexByte(data, '\n')
	if firstEnd < 0 {
		return nil, "", fmt.Errorf("skill frontmatter is not terminated")
	}
	frontmatterStart := firstEnd + 1
	lineStart := frontmatterStart
	for lineStart <= len(data) {
		lineEnd := bytes.IndexByte(data[lineStart:], '\n')
		var next int
		if lineEnd < 0 {
			lineEnd = len(data)
			next = len(data)
		} else {
			lineEnd += lineStart
			next = lineEnd + 1
		}
		line := data[lineStart:lineEnd]
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.Equal(line, []byte("---")) {
			return data[frontmatterStart:lineStart], string(data[next:]), nil
		}
		if next == len(data) {
			break
		}
		lineStart = next
	}
	return nil, "", fmt.Errorf("skill frontmatter is not terminated")
}
