package agentrole

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

type ParseOptions struct {
	Origin   string
	Limits   Limits
	Redactor *redact.RuntimeRedactor
}

type frontmatterDocument struct {
	Name           *string   `yaml:"name"`
	Description    *string   `yaml:"description"`
	AllowedTools   *[]string `yaml:"allowed_tools"`
	DeniedTools    *[]string `yaml:"denied_tools"`
	Model          *string   `yaml:"model"`
	MaxIterations  *int      `yaml:"max_iterations"`
	PermissionMode *string   `yaml:"permission_mode"`
}

var roleNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var frontmatterFields = map[string]yaml.Kind{
	"name":            yaml.ScalarNode,
	"description":     yaml.ScalarNode,
	"allowed_tools":   yaml.SequenceNode,
	"denied_tools":    yaml.SequenceNode,
	"model":           yaml.ScalarNode,
	"max_iterations":  yaml.ScalarNode,
	"permission_mode": yaml.ScalarNode,
}

func ParseMarkdown(raw []byte, options ParseOptions) (Candidate, error) {
	if options.Redactor == nil || options.Limits.Validate() != nil {
		return Candidate{}, errors.New("invalid role parser options")
	}
	origin, originOK := safeOrigin(options)
	invalid := func(code, message string) (Candidate, error) {
		return invalidCandidate(origin, options.Redactor, code, message), nil
	}
	if !originOK {
		return invalid("role_origin_invalid", "role origin is invalid")
	}
	if !utf8.Valid(raw) {
		return invalid("role_utf8_invalid", "role document is not valid UTF-8")
	}
	if int64(len(raw)) > options.Limits.MaxEntryBytes {
		return invalid("role_entry_limit", "role document exceeds its size limit")
	}
	frontmatter, body, ok := splitRoleDocument(raw)
	if !ok {
		return invalid("role_frontmatter_invalid", "role frontmatter boundary is invalid")
	}
	if int64(len(frontmatter)) > options.Limits.MaxFrontmatterBytes {
		return invalid("role_frontmatter_limit", "role frontmatter exceeds its size limit")
	}
	if int64(len(body)) > options.Limits.MaxBodyBytes || int64(len(body)) > options.Limits.MaxInstructionBytes {
		return invalid("role_body_limit", "role instructions exceed their size limit")
	}
	if hasTrailingYAMLDocument(body) {
		return invalid("role_yaml_document_invalid", "role document contains trailing YAML")
	}

	node, err := decodeStrictYAMLNode(frontmatter)
	if err != nil {
		return invalid("role_yaml_invalid", "role frontmatter is invalid")
	}
	if err := validateFrontmatterNode(node); err != nil {
		return invalid("role_yaml_unsafe", "role frontmatter uses an unsupported YAML construct")
	}

	var document frontmatterDocument
	decoder := yaml.NewDecoder(bytes.NewReader(frontmatter))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return invalid("role_yaml_invalid", "role frontmatter fields are invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return invalid("role_yaml_document_invalid", "role frontmatter must contain one YAML document")
	}

	metadata, err := normalizeMetadata(document, options)
	if err != nil {
		return invalid("role_metadata_invalid", "role metadata is invalid")
	}
	instructions := strings.TrimSpace(strings.ReplaceAll(string(body), "\r\n", "\n"))
	if instructions == "" || hasUnsafeControls(instructions, true) {
		return invalid("role_body_invalid", "role instructions are invalid")
	}
	safeInstructions := options.Redactor.Redact(instructions)
	if safeInstructions.Text() == "" || !utf8.ValidString(safeInstructions.Text()) ||
		int64(len(safeInstructions.Text())) > options.Limits.MaxInstructionBytes ||
		int64(len(safeInstructions.Text())) > options.Limits.MaxBodyBytes ||
		hasUnsafeControls(safeInstructions.Text(), true) {
		return invalid("role_body_invalid", "role instructions are invalid after redaction")
	}

	return Candidate{
		Metadata:     metadata,
		Instructions: safeInstructions,
		Origin:       origin,
		Valid:        true,
	}, nil
}

func splitRoleDocument(raw []byte) (frontmatter []byte, body []byte, ok bool) {
	if len(raw) < 4 {
		return nil, nil, false
	}
	openingEnd := bytes.IndexByte(raw, '\n')
	if openingEnd < 0 || string(bytes.TrimSuffix(raw[:openingEnd], []byte{'\r'})) != "---" {
		return nil, nil, false
	}
	frontStart := openingEnd + 1
	for offset := frontStart; offset <= len(raw); {
		relativeEnd := bytes.IndexByte(raw[offset:], '\n')
		lineEnd := len(raw)
		next := len(raw)
		if relativeEnd >= 0 {
			lineEnd = offset + relativeEnd
			next = lineEnd + 1
		}
		line := bytes.TrimSuffix(raw[offset:lineEnd], []byte{'\r'})
		if bytes.Equal(line, []byte("---")) {
			return raw[frontStart:offset], raw[next:], true
		}
		if relativeEnd < 0 {
			break
		}
		offset = next
	}
	return nil, nil, false
}

func decodeStrictYAMLNode(frontmatter []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(frontmatter))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("multiple YAML documents")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("frontmatter is not a mapping")
	}
	return document.Content[0], nil
}

func validateFrontmatterNode(root *yaml.Node) error {
	if root == nil || root.Kind != yaml.MappingNode || len(root.Content)%2 != 0 {
		return errors.New("invalid mapping")
	}
	seen := make(map[string]struct{}, len(root.Content)/2)
	for index := 0; index < len(root.Content); index += 2 {
		key := root.Content[index]
		value := root.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Style&yaml.TaggedStyle != 0 || key.Anchor != "" {
			return errors.New("invalid mapping key")
		}
		if key.Value == "<<" {
			return errors.New("merge key is forbidden")
		}
		wantKind, known := frontmatterFields[key.Value]
		if !known {
			return errors.New("unknown field")
		}
		if _, duplicate := seen[key.Value]; duplicate {
			return errors.New("duplicate field")
		}
		seen[key.Value] = struct{}{}
		if value.Kind != wantKind || value.Tag == "!!null" || value.Style&yaml.TaggedStyle != 0 {
			return errors.New("invalid field type")
		}
		if err := rejectUnsafeYAMLNode(value); err != nil {
			return err
		}
		switch key.Value {
		case "name", "description", "model", "permission_mode":
			if value.Tag != "!!str" {
				return errors.New("field must be a string")
			}
		case "max_iterations":
			if value.Tag != "!!int" {
				return errors.New("max_iterations must be an integer")
			}
		case "allowed_tools", "denied_tools":
			for _, item := range value.Content {
				if item.Kind != yaml.ScalarNode || item.Tag != "!!str" || item.Style&yaml.TaggedStyle != 0 {
					return errors.New("tool name must be a string")
				}
			}
		}
	}
	return nil
}

func rejectUnsafeYAMLNode(node *yaml.Node) error {
	if node == nil || node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" || node.Style&yaml.TaggedStyle != 0 {
		return errors.New("unsafe YAML node")
	}
	for index, child := range node.Content {
		if node.Kind == yaml.MappingNode && index%2 == 0 && child.Value == "<<" {
			return errors.New("merge key is forbidden")
		}
		if err := rejectUnsafeYAMLNode(child); err != nil {
			return err
		}
	}
	return nil
}

func normalizeMetadata(document frontmatterDocument, options ParseOptions) (Metadata, error) {
	if document.Name == nil || document.Description == nil {
		return Metadata{}, errors.New("required metadata is missing")
	}
	name := strings.ToLower(strings.TrimSpace(*document.Name))
	if !roleNamePattern.MatchString(name) || int64(len(name)) > options.Limits.MaxNameBytes {
		return Metadata{}, errors.New("invalid role name")
	}
	description := strings.TrimSpace(*document.Description)
	if description == "" || hasUnsafeControls(description, false) || int64(len(description)) > options.Limits.MaxDescriptionBytes {
		return Metadata{}, errors.New("invalid description")
	}
	safeDescription := options.Redactor.Redact(description)
	if safeDescription.Text() == "" || !utf8.ValidString(safeDescription.Text()) ||
		int64(len(safeDescription.Text())) > options.Limits.MaxDescriptionBytes || hasUnsafeControls(safeDescription.Text(), false) {
		return Metadata{}, errors.New("invalid redacted description")
	}

	allowed, err := normalizeToolNames(document.AllowedTools, options.Limits)
	if err != nil {
		return Metadata{}, err
	}
	denied, err := normalizeToolNames(document.DeniedTools, options.Limits)
	if err != nil {
		return Metadata{}, err
	}
	if len(allowed) > options.Limits.MaxToolNames || len(denied) > options.Limits.MaxToolNames-len(allowed) {
		return Metadata{}, errors.New("too many tool names")
	}
	if toolListBytes(allowed)+toolListBytes(denied) > options.Limits.MaxToolListBytes {
		return Metadata{}, errors.New("tool list exceeds its size limit")
	}

	model := ModelInherit
	if document.Model != nil {
		model = ModelAlias(*document.Model)
	}
	switch model {
	case ModelInherit, ModelHaiku, ModelSonnet, ModelOpus:
	default:
		return Metadata{}, errors.New("invalid model alias")
	}
	permissionMode := PermissionInherit
	if document.PermissionMode != nil {
		permissionMode = PermissionMode(*document.PermissionMode)
	}
	switch permissionMode {
	case PermissionInherit, PermissionStrict, PermissionDefault, PermissionPermissive:
	default:
		return Metadata{}, errors.New("invalid permission mode")
	}
	if document.MaxIterations != nil && *document.MaxIterations < 0 {
		return Metadata{}, errors.New("negative iteration limit")
	}
	var maxIterations *int
	if document.MaxIterations != nil {
		value := *document.MaxIterations
		maxIterations = &value
	}
	return Metadata{
		Name:           name,
		Description:    safeDescription,
		ToolAllow:      allowed,
		ToolDeny:       denied,
		Model:          model,
		MaxIterations:  maxIterations,
		PermissionMode: permissionMode,
	}, nil
}

func normalizeToolNames(input *[]string, limits Limits) ([]string, error) {
	if input == nil {
		return nil, nil
	}
	if len(*input) > limits.MaxToolNames {
		return nil, errors.New("too many tool names")
	}
	seen := make(map[string]struct{}, len(*input))
	result := make([]string, 0, len(*input))
	for _, raw := range *input {
		name := strings.TrimSpace(raw)
		if name == "" || !utf8.ValidString(name) || int64(len(name)) > limits.MaxToolNameBytes || hasUnsafeControls(name, false) {
			return nil, errors.New("invalid tool name")
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func toolListBytes(names []string) int64 {
	var total int64
	for _, name := range names {
		if int64(len(name)) > int64(^uint64(0)>>1)-total {
			return int64(^uint64(0) >> 1)
		}
		total += int64(len(name))
	}
	return total
}

func safeOrigin(options ParseOptions) (redact.SafeText, bool) {
	origin := strings.TrimSpace(options.Origin)
	if origin == "" || !utf8.ValidString(origin) || int64(len(origin)) > options.Limits.MaxOriginBytes || hasUnsafeControls(origin, false) {
		return options.Redactor.Redact("invalid role origin"), false
	}
	safe := options.Redactor.Redact(origin)
	if safe.Text() == "" || !utf8.ValidString(safe.Text()) || int64(len(safe.Text())) > options.Limits.MaxOriginBytes || hasUnsafeControls(safe.Text(), false) {
		return options.Redactor.Redact("invalid role origin"), false
	}
	return safe, true
}

func invalidCandidate(origin redact.SafeText, redactor *redact.RuntimeRedactor, code, message string) Candidate {
	return Candidate{
		Origin: origin,
		Valid:  false,
		Diagnostics: []diagnostics.SafeDiagnostic{{
			Code:     code,
			Source:   "agentrole",
			Hint:     "fix_role_definition",
			Severity: diagnostics.SeverityError,
			Message:  redactor.Redact(message),
		}},
	}
}

func hasUnsafeControls(value string, multiline bool) bool {
	for _, character := range value {
		if multiline && (character == '\n' || character == '\t') {
			continue
		}
		if character < 0x20 || (character >= 0x7f && character <= 0x9f) {
			return true
		}
	}
	return false
}

func hasTrailingYAMLDocument(body []byte) bool {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if !strings.HasPrefix(trimmed, "---\n") && !strings.HasPrefix(trimmed, "---\r\n") {
		return false
	}
	lines := strings.Split(strings.ReplaceAll(trimmed, "\r\n", "\n"), "\n")
	for _, line := range lines[1:] {
		if line == "---" || line == "..." {
			return true
		}
	}
	return false
}
