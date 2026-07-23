package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"xagent/internal/redact"
)

type LoadOptions struct {
	HomeDir     string
	ProjectRoot string
	LookupEnv   func(string) (string, bool)
	Redactor    *redact.RuntimeRedactor
	Limits      Limits
}

type LoadError struct {
	Source  string
	Line    int
	Column  int
	Path    string
	Rule    int
	Message string
}

func (e *LoadError) Error() string {
	location := e.Source
	if e.Line > 0 {
		location += fmt.Sprintf(":%d:%d", e.Line, e.Column)
	}
	parts := []string{location}
	if e.Rule > 0 {
		parts = append(parts, fmt.Sprintf("rule %d", e.Rule))
	}
	if e.Path != "" {
		parts = append(parts, e.Path)
	}
	parts = append(parts, e.Message)
	return strings.Join(parts, ": ")
}

type candidateRule struct {
	config RuleConfig
	source Source
}

// Load discovers, strictly validates, and compiles the two schema-v1 files.
// It performs no process, network, Provider, or worker side effects.
func Load(options LoadOptions) (Snapshot, error) {
	limits := normalizeLimits(options.Limits)
	project, err := filepath.Abs(options.ProjectRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("hook project root: %w", err)
	}
	home := options.HomeDir
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return Snapshot{}, fmt.Errorf("hook home: %w", err)
		}
	}
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	paths := []string{filepath.Join(home, ".config", "xagent", "hooks.yaml"), filepath.Join(project, ".xagent", "hooks.yaml")}
	candidates := []candidateRule{}
	for _, path := range paths {
		loaded, err := loadFile(path, limits)
		if err != nil {
			return Snapshot{}, safeLoadError(err, options.Redactor)
		}
		for index := range loaded {
			loaded[index].source.EffectiveOrdinal = len(candidates) + 1
			candidates = append(candidates, loaded[index])
		}
	}
	rules := make([]Rule, 0, len(candidates))
	for _, candidate := range candidates {
		rule, err := compileRule(candidate, limits, lookup, options.Redactor)
		if err != nil {
			path := ""
			message := "invalid hook rule"
			var validationErr *validationError
			if errors.As(err, &validationErr) {
				path = validationErr.path
				message = validationErr.message
			}
			position := candidate.config.position(path)
			loadErr := &LoadError{
				Source:  candidate.source.Path,
				Line:    position.line,
				Column:  position.column,
				Rule:    candidate.source.Ordinal,
				Path:    joinPath(fmt.Sprintf("hooks[%d]", candidate.source.Ordinal-1), path),
				Message: message,
			}
			return Snapshot{}, safeLoadError(loadErr, options.Redactor)
		}
		rules = append(rules, rule)
	}
	return newSnapshot(rules), nil
}

func safeLoadError(err error, runtimeRedactor *redact.RuntimeRedactor) error {
	if runtimeRedactor == nil {
		return err
	}
	return errors.New(runtimeRedactor.Text(err.Error()))
}

func loadFile(path string, limits Limits) ([]candidateRule, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &LoadError{Source: abs, Message: "cannot read hook configuration"}
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limits.YAMLBytes)+1))
	if err != nil {
		return nil, &LoadError{Source: abs, Message: "cannot read hook configuration"}
	}
	if len(data) > limits.YAMLBytes {
		return nil, &LoadError{Source: abs, Path: "$", Message: "configuration exceeds size limit"}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		line, column := yamlErrorPosition(err)
		return nil, &LoadError{Source: abs, Line: line, Column: column, Path: "$", Message: "invalid YAML structure"}
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			line, column := yamlErrorPosition(err)
			return nil, &LoadError{Source: abs, Line: line, Column: column, Path: "$", Message: "invalid YAML structure"}
		}
		node := &extra
		if len(extra.Content) == 1 {
			node = extra.Content[0]
		}
		return nil, located(abs, 0, "$", node, "multiple YAML documents are not allowed")
	}
	if len(document.Content) != 1 {
		return nil, &LoadError{Source: abs, Line: 1, Column: 1, Path: "$", Message: "empty configuration"}
	}
	root := document.Content[0]
	if err := validateRawNode(root, "$"); err != nil {
		rule, path := rawErrorLocation(err.path)
		return nil, located(abs, rule, path, err.node, err.message)
	}
	rootMap, err := mapping(root, []string{"version", "hooks"}, []string{"version", "hooks"}, "$")
	if err != nil {
		var parseErr *nodeError
		if errors.As(err, &parseErr) {
			return nil, located(abs, 0, parseErr.path, parseErr.node, parseErr.message)
		}
		return nil, located(abs, 0, "$", root, "invalid YAML structure")
	}
	version, err := intScalar(rootMap["version"])
	if err != nil || version != 1 {
		return nil, located(abs, 0, "version", rootMap["version"], "unsupported schema version")
	}
	hooks := rootMap["hooks"]
	if hooks.Kind != yaml.SequenceNode {
		return nil, located(abs, 0, "hooks", hooks, "must be a sequence")
	}
	if len(hooks.Content) > limits.RulesPerFile {
		return nil, located(abs, 0, "hooks", hooks, "rule count exceeds limit")
	}
	result := make([]candidateRule, 0, len(hooks.Content))
	for index, node := range hooks.Content {
		config, err := parseRuleNode(node, limits)
		if err != nil {
			var parseErr *nodeError
			if errors.As(err, &parseErr) {
				return nil, located(abs, index+1, joinPath(fmt.Sprintf("hooks[%d]", index), parseErr.path), parseErr.node, parseErr.message)
			}
			return nil, located(abs, index+1, fmt.Sprintf("hooks[%d]", index), node, err.Error())
		}
		config.line, config.column = node.Line, node.Column
		result = append(result, candidateRule{config: config, source: Source{Path: filepath.Clean(abs), Ordinal: index + 1}})
	}
	return result, nil
}

type nodeError struct {
	path, message string
	node          *yaml.Node
}

func (e *nodeError) Error() string { return e.message }
func nerr(path string, node *yaml.Node, message string) error {
	return &nodeError{path: path, node: node, message: message}
}

func joinPath(base, child string) string {
	if base == "" {
		return child
	}
	if child == "" {
		return base
	}
	if strings.HasPrefix(child, "[") {
		return base + child
	}
	return base + "." + child
}

func located(source string, rule int, path string, node *yaml.Node, message string) error {
	line, column := 0, 0
	if node != nil {
		line, column = node.Line, node.Column
	}
	return &LoadError{Source: source, Line: line, Column: column, Path: path, Rule: rule, Message: message}
}

func yamlErrorPosition(err error) (int, int) {
	if err == nil {
		return 0, 0
	}
	message := err.Error()
	marker := "line "
	index := strings.Index(message, marker)
	if index < 0 {
		return 1, 1
	}
	rest := message[index+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	line, parseErr := strconv.Atoi(rest[:end])
	if parseErr != nil || line <= 0 {
		return 1, 1
	}
	column := 1
	if columnIndex := strings.Index(rest[end:], "column "); columnIndex >= 0 {
		columnText := rest[end+columnIndex+len("column "):]
		columnEnd := 0
		for columnEnd < len(columnText) && columnText[columnEnd] >= '0' && columnText[columnEnd] <= '9' {
			columnEnd++
		}
		if parsed, columnErr := strconv.Atoi(columnText[:columnEnd]); columnErr == nil && parsed > 0 {
			column = parsed
		}
	}
	return line, column
}

func rawErrorLocation(path string) (int, string) {
	const prefix = "$.hooks["
	if !strings.HasPrefix(path, prefix) {
		return 0, path
	}
	rest := path[len(prefix):]
	end := strings.IndexByte(rest, ']')
	if end < 1 {
		return 0, path
	}
	index, err := strconv.Atoi(rest[:end])
	if err != nil || index < 0 {
		return 0, path
	}
	return index + 1, strings.TrimPrefix(path, "$.")
}

func validateRawNode(node *yaml.Node, path string) *nodeError {
	if node == nil {
		return &nodeError{path: path, message: "missing node"}
	}
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return &nodeError{path: path, node: node, message: "anchors and aliases are not allowed"}
	}
	if node.Tag != "" && !strings.HasPrefix(node.Tag, "!!") {
		return &nodeError{path: path, node: node, message: "custom YAML tags are not allowed"}
	}
	switch node.Kind {
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Anchor != "" || key.Kind == yaml.AliasNode {
				return &nodeError{path: joinPath(path, "<key>"), node: key, message: "anchors and aliases are not allowed"}
			}
			if key.Value == "<<" {
				return &nodeError{path: joinPath(path, "<merge>"), node: key, message: "merge keys are not allowed"}
			}
			if key.Tag != "" && !strings.HasPrefix(key.Tag, "!!") {
				return &nodeError{path: joinPath(path, "<key>"), node: key, message: "custom YAML tags are not allowed"}
			}
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return &nodeError{path: joinPath(path, "<key>"), node: key, message: "mapping keys must be strings"}
			}
			childPath := joinPath(path, safePathSegment(key.Value))
			if seen[key.Value] {
				return &nodeError{path: childPath, node: key, message: "duplicate mapping key"}
			}
			seen[key.Value] = true
			if err := validateRawNode(node.Content[i+1], childPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if err := validateRawNode(child, joinPath(path, fmt.Sprintf("[%d]", index))); err != nil {
				return err
			}
		}
	}
	return nil
}

func safePathSegment(value string) string {
	if value == "" {
		return "<field>"
	}
	for index, r := range value {
		letter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if index == 0 && !letter && r != '_' {
			return "<field>"
		}
		if index > 0 && !letter && (r < '0' || r > '9') && r != '_' && r != '-' {
			return "<field>"
		}
	}
	return value
}

func mapping(node *yaml.Node, allowed, required []string, path string) (map[string]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, &nodeError{path: path, node: node, message: "must be a mapping"}
	}
	allow := map[string]bool{}
	for _, key := range allowed {
		allow[key] = true
	}
	values := map[string]*yaml.Node{}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if !allow[key.Value] {
			return nil, &nodeError{path: joinPath(path, safePathSegment(key.Value)), node: key, message: "unknown field"}
		}
		values[key.Value] = value
	}
	for _, key := range required {
		if values[key] == nil {
			return nil, &nodeError{path: joinPath(path, key), node: node, message: "missing required field"}
		}
	}
	return values, nil
}

func parseRuleNode(node *yaml.Node, limits Limits) (RuleConfig, error) {
	fields, err := mapping(node, []string{"event", "if", "once", "async", "timeout", "action"}, []string{"event", "action"}, "")
	if err != nil {
		return RuleConfig{}, err
	}
	config := RuleConfig{present: presence(fields), locations: map[string]sourcePosition{}}
	recordLocations(config.locations, "", fields)
	config.locations[""] = positionOf(node)
	if config.Event, err = eventScalar(fields["event"]); err != nil {
		return config, nerr("event", fields["event"], "event must be a string")
	}
	if value := fields["once"]; value != nil {
		config.Once, err = boolScalar(value)
		if err != nil {
			return config, nerr("once", value, "once must be a boolean")
		}
	}
	if value := fields["async"]; value != nil {
		config.Async, err = boolScalar(value)
		if err != nil {
			return config, nerr("async", value, "async must be a boolean")
		}
	}
	if value := fields["timeout"]; value != nil {
		if err := config.Timeout.UnmarshalYAML(value); err != nil {
			return config, nerr("timeout", value, err.Error())
		}
	}
	if value := fields["if"]; value != nil {
		group, e := parseConditionNode(value, limits, config.locations)
		if e != nil {
			return config, e
		}
		config.If = &group
	}
	action, err := parseActionNode(fields["action"], config.locations)
	if err != nil {
		return config, err
	}
	config.Action = action
	return config, nil
}

func parseConditionNode(node *yaml.Node, limits Limits, locations map[string]sourcePosition) (ConditionGroup, error) {
	fields, err := mapping(node, []string{"all", "any"}, nil, "if")
	if err != nil {
		return ConditionGroup{}, err
	}
	recordLocations(locations, "if", fields)
	if (fields["all"] == nil) == (fields["any"] == nil) {
		return ConditionGroup{}, nerr("if", node, "if must contain exactly one of all or any")
	}
	group := ConditionGroup{present: presence(fields)}
	key := "all"
	if fields[key] == nil {
		key = "any"
	}
	sequence := fields[key]
	if sequence.Kind != yaml.SequenceNode || len(sequence.Content) == 0 {
		return group, nerr("if."+key, sequence, "condition group must be a non-empty sequence")
	}
	if len(sequence.Content) > limits.PredicatesPerRule {
		return group, nerr("if."+key, sequence, "predicate count exceeds limit")
	}
	items := make([]Predicate, 0, len(sequence.Content))
	for index, child := range sequence.Content {
		base := fmt.Sprintf("if.%s[%d]", key, index)
		locations[base] = positionOf(child)
		p, err := parsePredicateNode(child, base, locations)
		if err != nil {
			return group, err
		}
		p.line, p.column = child.Line, child.Column
		items = append(items, p)
	}
	if key == "all" {
		group.All = items
	} else {
		group.Any = items
	}
	return group, nil
}

func parsePredicateNode(node *yaml.Node, path string, locations map[string]sourcePosition) (Predicate, error) {
	fields, err := mapping(node, []string{"field", "match", "value", "negate"}, []string{"field", "match", "value"}, path)
	if err != nil {
		return Predicate{}, err
	}
	recordLocations(locations, path, fields)
	p := Predicate{present: presence(fields)}
	p.Field, err = stringScalar(fields["field"])
	if err != nil || p.Field == "" {
		return p, nerr(joinPath(path, "field"), fields["field"], "field must be a non-empty string")
	}
	match, err := stringScalar(fields["match"])
	if err != nil {
		return p, nerr(joinPath(path, "match"), fields["match"], "match must be a string")
	}
	p.Match = MatchType(match)
	p.Value, err = predicateScalar(fields["value"])
	if err != nil {
		return p, nerr(joinPath(path, "value"), fields["value"], err.Error())
	}
	if value := fields["negate"]; value != nil {
		p.Negate, err = boolScalar(value)
		if err != nil {
			return p, nerr(joinPath(path, "negate"), value, "negate must be a boolean")
		}
	}
	return p, nil
}

func parseActionNode(node *yaml.Node, locations map[string]sourcePosition) (ActionConfig, error) {
	allowed := []string{"type", "command", "env", "decision", "url", "method", "headers", "send_event", "content", "scope", "agent", "input"}
	fields, err := mapping(node, allowed, []string{"type"}, "action")
	if err != nil {
		return ActionConfig{}, err
	}
	recordLocations(locations, "action", fields)
	a := ActionConfig{present: presence(fields), line: node.Line, column: node.Column}
	typeName, err := stringScalar(fields["type"])
	if err != nil {
		return a, nerr("action.type", fields["type"], "action type must be a string")
	}
	a.Type = ActionType(typeName)
	stringsToSet := []struct {
		key    string
		target *string
	}{
		{key: "command", target: &a.Command},
		{key: "url", target: &a.URL},
		{key: "method", target: &a.Method},
		{key: "content", target: &a.Content},
		{key: "agent", target: &a.Agent},
		{key: "input", target: &a.Input},
	}
	for _, item := range stringsToSet {
		if value := fields[item.key]; value != nil {
			*item.target, err = stringScalar(value)
			if err != nil {
				return a, nerr("action."+item.key, value, item.key+" must be a string")
			}
		}
	}
	if value := fields["scope"]; value != nil {
		raw, e := stringScalar(value)
		if e != nil {
			return a, nerr("action.scope", value, "scope must be a string")
		}
		a.Scope = PromptScope(raw)
	}
	if value := fields["decision"]; value != nil {
		a.Decision, err = boolScalar(value)
		if err != nil {
			return a, nerr("action.decision", value, "decision must be a boolean")
		}
	}
	if value := fields["send_event"]; value != nil {
		parsed, e := boolScalar(value)
		if e != nil {
			return a, nerr("action.send_event", value, "send_event must be a boolean")
		}
		a.SendEvent = &parsed
	}
	if value := fields["env"]; value != nil {
		a.Env, err = stringMap(value, "action.env")
		if err != nil {
			return a, err
		}
	}
	if value := fields["headers"]; value != nil {
		a.Headers, err = stringMap(value, "action.headers")
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

func presence(fields map[string]*yaml.Node) map[string]bool {
	result := map[string]bool{}
	for key := range fields {
		result[key] = true
	}
	return result
}

func positionOf(node *yaml.Node) sourcePosition {
	if node == nil {
		return sourcePosition{}
	}
	return sourcePosition{line: node.Line, column: node.Column}
}

func recordLocations(locations map[string]sourcePosition, base string, fields map[string]*yaml.Node) {
	for key, node := range fields {
		locations[joinPath(base, key)] = positionOf(node)
	}
}

func (c RuleConfig) position(path string) sourcePosition {
	current := path
	for {
		if position, ok := c.locations[current]; ok && position.line > 0 {
			return position
		}
		if current == "" {
			break
		}
		if index := strings.LastIndexByte(current, '.'); index >= 0 {
			current = current[:index]
			continue
		}
		current = ""
	}
	return sourcePosition{line: c.line, column: c.column}
}

func stringScalar(node *yaml.Node) (string, error) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || !utf8.ValidString(node.Value) {
		return "", fmt.Errorf("not string")
	}
	return node.Value, nil
}
func eventScalar(node *yaml.Node) (Event, error) {
	value, err := stringScalar(node)
	return Event(value), err
}
func boolScalar(node *yaml.Node) (bool, error) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
		return false, fmt.Errorf("not bool")
	}
	return strconv.ParseBool(node.Value)
}
func intScalar(node *yaml.Node) (int, error) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return 0, fmt.Errorf("not int")
	}
	return strconv.Atoi(node.Value)
}
func stringMap(node *yaml.Node, path string) (map[string]string, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, nerr(path, node, "must map string keys to string values")
	}
	result := map[string]string{}
	for i := 0; i < len(node.Content); i += 2 {
		key, err := stringScalar(node.Content[i])
		if err != nil {
			return nil, nerr(joinPath(path, "<key>"), node.Content[i], "map key must be a string")
		}
		value, err := stringScalar(node.Content[i+1])
		if err != nil {
			return nil, nerr(joinPath(path, safePathSegment(key)), node.Content[i+1], "map value must be a string")
		}
		result[key] = value
	}
	return result, nil
}
func predicateScalar(node *yaml.Node) (any, error) {
	if node == nil || node.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("predicate value must be a scalar")
	}
	switch node.Tag {
	case "!!str":
		return node.Value, nil
	case "!!bool":
		return strconv.ParseBool(node.Value)
	case "!!int", "!!float":
		lower := strings.ToLower(node.Value)
		if strings.Contains(lower, "nan") || strings.Contains(lower, "inf") {
			return nil, fmt.Errorf("predicate number must be finite decimal")
		}
		if _, ok := normalizeNumber(node.Value); !ok {
			return nil, fmt.Errorf("predicate number must be finite decimal")
		}
		return json.Number(node.Value), nil
	default:
		return nil, fmt.Errorf("predicate value must be string, number, or boolean")
	}
}
