package hook

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"xagent/internal/matcher"
	"xagent/internal/redact"
)

const hookSchemaVersionError = "must be integer 1"

const (
	hookTimeoutMin             = time.Millisecond
	hookTimeoutMax             = 10 * time.Minute
	hookCommandDefaultTimeout  = 30 * time.Second
	hookHTTPDefaultTimeout     = 10 * time.Second
	hookSubAgentDefaultTimeout = 30 * time.Second
)

// validateHookSchemaVersion is the first semantic validation performed on a
// decoded hooks.yaml document. Keeping it independent from rule compilation
// ensures an unsupported format cannot expand env values or construct any
// action runtime state.
func validateHookSchemaVersion(node *yaml.Node) error {
	version, err := intScalar(node)
	if err != nil || version != 1 {
		return invalidAt("version", hookSchemaVersionError)
	}
	return nil
}

type compiledAction struct {
	typeName ActionType
	command  string
	env      map[string]string
	decision bool
	http     *httpAction
	template *compiledTemplate
	scope    PromptScope
	agent    string
	timeout  time.Duration
}

type Rule struct {
	Event     Event
	Source    Source
	Once      bool
	Async     bool
	Timeout   time.Duration
	condition *compiledCondition
	action    compiledAction
}

type validationError struct {
	path    string
	message string
}

func (e *validationError) Error() string { return e.message }

func invalidAt(path, message string) error {
	return &validationError{path: path, message: message}
}

func (r Rule) ActionType() ActionType { return r.action.typeName }

type Snapshot struct{ rules []Rule }

func newSnapshot(rules []Rule) Snapshot {
	out := make([]Rule, len(rules))
	for index := range rules {
		out[index] = cloneRule(rules[index])
	}
	return Snapshot{rules: out}
}
func (s Snapshot) Len() int { return len(s.rules) }
func (s Snapshot) Rules() []Rule {
	out := make([]Rule, len(s.rules))
	for index := range s.rules {
		out[index] = cloneRule(s.rules[index])
	}
	return out
}

func cloneRule(rule Rule) Rule {
	out := rule
	if rule.condition != nil {
		condition := *rule.condition
		condition.predicates = append([]compiledPredicate(nil), rule.condition.predicates...)
		out.condition = &condition
	}
	out.action.env = cloneStringMap(rule.action.env)
	if rule.action.http != nil {
		httpCopy := *rule.action.http
		httpCopy.url = cloneURL(rule.action.http.url)
		httpCopy.headers = rule.action.http.headers.Clone()
		out.action.http = &httpCopy
	}
	if rule.action.template != nil {
		templateCopy := *rule.action.template
		templateCopy.parts = make([]templatePart, len(rule.action.template.parts))
		for index, part := range rule.action.template.parts {
			templateCopy.parts[index] = part
			if part.field != nil {
				field := *part.field
				field.segments = append([]string(nil), part.field.segments...)
				templateCopy.parts[index].field = &field
			}
		}
		out.action.template = &templateCopy
	}
	return out
}

func compileRule(candidate candidateRule, limits Limits, lookup func(string) (string, bool), runtimeRedactor *redact.RuntimeRedactor) (Rule, error) {
	c := candidate.config
	if !validEvent(c.Event) {
		return Rule{}, invalidAt("event", "unknown event")
	}
	condition, err := validateAndCompileCondition(c.Event, c.If)
	if err != nil {
		return Rule{}, err
	}
	action, err := compileAction(c.Event, c.Async, c.Timeout, c.Action, limits, lookup, runtimeRedactor)
	if err != nil {
		return Rule{}, err
	}
	return Rule{Event: c.Event, Source: candidate.source, Once: c.Once, Async: c.Async, Timeout: action.timeout, condition: condition, action: action}, nil
}

func validEvent(event Event) bool {
	for _, item := range allEvents {
		if event == item {
			return true
		}
	}
	return false
}

func validateAndCompileCondition(event Event, group *ConditionGroup) (*compiledCondition, error) {
	if group == nil {
		return nil, nil
	}
	if (len(group.All) == 0) == (len(group.Any) == 0) {
		return nil, invalidAt("if", "exactly one non-empty all or any is required")
	}
	items := group.All
	groupName := "all"
	if len(items) == 0 {
		items = group.Any
		groupName = "any"
	}
	for index, p := range items {
		base := fmt.Sprintf("if.%s[%d]", groupName, index)
		if p.Field == "" {
			return nil, invalidAt(base+".field", "field is required")
		}
		switch p.Match {
		case MatchExact, MatchGlob, MatchRegex:
		default:
			return nil, invalidAt(base+".match", "unknown match")
		}
		if p.Match == MatchGlob || p.Match == MatchRegex {
			if _, ok := p.Value.(string); !ok {
				return nil, invalidAt(base+".value", "glob and regex require string values")
			}
		}
		if _, err := compileField(event, p.Field); err != nil {
			return nil, invalidAt(base+".field", "field is unavailable for event")
		}
		if p.Match == MatchGlob {
			if _, err := matcher.MatchGlob("", p.Value.(string)); err != nil {
				return nil, invalidAt(base+".value", "invalid glob")
			}
		}
		if p.Match == MatchRegex {
			if _, err := regexp.Compile(`\A(?:` + p.Value.(string) + `)\z`); err != nil {
				return nil, invalidAt(base+".value", "invalid regex")
			}
		}
	}
	condition, err := compileCondition(event, group)
	if err != nil {
		return nil, invalidAt("if", "invalid condition")
	}
	return condition, nil
}

func compileAction(event Event, async bool, timeout Duration, config ActionConfig, limits Limits, lookup func(string) (string, bool), runtimeRedactor *redact.RuntimeRedactor) (compiledAction, error) {
	var err error
	allowed := map[ActionType]map[string]bool{
		ActionCommand:  set("type", "command", "env", "decision"),
		ActionHTTP:     set("type", "url", "method", "headers", "send_event", "decision"),
		ActionPrompt:   set("type", "content", "scope"),
		ActionSubAgent: set("type", "agent", "input"),
	}
	fields, ok := allowed[config.Type]
	if !ok {
		return compiledAction{}, invalidAt("action.type", "unknown action type")
	}
	presentKeys := make([]string, 0, len(config.present))
	for key := range config.present {
		presentKeys = append(presentKeys, key)
	}
	sort.Strings(presentKeys)
	for _, key := range presentKeys {
		if !fields[key] {
			return compiledAction{}, invalidAt("action."+key, "field is not valid for action type")
		}
	}
	if async && (event == EventToolBefore || config.Type == ActionPrompt) {
		return compiledAction{}, invalidAt("async", "async is not allowed for this event/action")
	}
	if timeout.Set && config.Type == ActionPrompt {
		return compiledAction{}, invalidAt("timeout", "timeout is not valid for prompt")
	}
	if timeout.Set && (timeout.Duration < hookTimeoutMin || timeout.Duration > hookTimeoutMax) {
		return compiledAction{}, invalidAt("timeout", "timeout must be between 1ms and 10m")
	}
	if !timeout.Set && config.Type != ActionCommand && config.Type != ActionHTTP && config.Type != ActionSubAgent && timeout.Duration != 0 {
		return compiledAction{}, invalidAt("timeout", "timeout is not valid for action")
	}
	action := compiledAction{typeName: config.Type, decision: config.Decision, scope: config.Scope, agent: config.Agent}
	switch config.Type {
	case ActionCommand:
		if strings.TrimSpace(config.Command) == "" {
			return action, invalidAt("action.command", "command is required")
		}
		if len(config.Command) > limits.CommandBytes {
			return action, invalidAt("action.command", "command exceeds limit")
		}
		if strings.Contains(config.Command, "{{") {
			return action, invalidAt("action.command", "event placeholders are not allowed in command")
		}
		action.command = config.Command
		action.env, err = compileEnv(config.Env, limits, lookup, runtimeRedactor)
		if err != nil {
			return action, invalidAt("action.env", safeCompileMessage(err, "invalid command environment"))
		}
		if timeout.Set {
			action.timeout = timeout.Duration
		} else {
			action.timeout = hookCommandDefaultTimeout
		}
	case ActionHTTP:
		action.http, err = compileHTTPAction(config, limits, lookup, runtimeRedactor)
		if err != nil {
			return action, invalidAt(httpValidationPath(err), safeCompileMessage(err, "invalid HTTP action"))
		}
		if timeout.Set {
			action.timeout = timeout.Duration
		} else {
			action.timeout = hookHTTPDefaultTimeout
		}
	case ActionPrompt:
		if strings.TrimSpace(config.Content) == "" {
			return action, invalidAt("action.content", "content is required")
		}
		if action.scope == "" {
			if config.present["scope"] {
				return action, invalidAt("action.scope", "scope must not be empty")
			}
			action.scope = ScopeTurn
		}
		if !promptScopeAllowed(event, action.scope) {
			return action, invalidAt("action.scope", "prompt scope is not allowed for event")
		}
		action.template, err = compileTemplate(event, config.Content, limits.PromptTemplateBytes)
		if err != nil {
			return action, invalidAt("action.content", safeCompileMessage(err, "invalid prompt template"))
		}
	case ActionSubAgent:
		if strings.TrimSpace(config.Agent) == "" {
			return action, invalidAt("action.agent", "agent is required")
		}
		if strings.TrimSpace(config.Input) == "" {
			return action, invalidAt("action.input", "input is required")
		}
		if len(config.Agent) > limits.SubAgentNameBytes {
			return action, invalidAt("action.agent", "agent exceeds limit")
		}
		if len(config.Input) > limits.SubAgentInputBytes {
			return action, invalidAt("action.input", "input exceeds limit")
		}
		action.template, err = compileTemplate(event, config.Input, limits.SubAgentInputBytes)
		if err != nil {
			return action, invalidAt("action.input", safeCompileMessage(err, "invalid subagent input template"))
		}
		if timeout.Set {
			action.timeout = timeout.Duration
		} else {
			action.timeout = hookSubAgentDefaultTimeout
		}
	}
	if config.Decision {
		if event != EventToolBefore || async || (config.Type != ActionCommand && config.Type != ActionHTTP) {
			return action, invalidAt("action.decision", "decision is only valid for synchronous tool_before command/http")
		}
	}
	return action, nil
}

var safeCompileMessages = map[string]struct{}{
	"env count exceeds limit":                    {},
	"invalid env key":                            {},
	"event placeholders are not allowed":         {},
	"unterminated environment reference":         {},
	"invalid environment reference":              {},
	"undefined environment reference":            {},
	"env value exceeds limit or contains NUL":    {},
	"url is required":                            {},
	"invalid static URL":                         {},
	"invalid HTTP target":                        {},
	"unsupported HTTP scheme":                    {},
	"plaintext HTTP requires loopback":           {},
	"unsupported HTTP method":                    {},
	"header count exceeds limit":                 {},
	"invalid header name":                        {},
	"duplicate header":                           {},
	"forbidden header":                           {},
	"invalid header value":                       {},
	"template exceeds limit or is invalid UTF-8": {},
	"malformed template":                         {},
	"unterminated template token":                {},
	"invalid template token":                     {},
	"invalid template field":                     {},
}

func safeCompileMessage(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	message := err.Error()
	if _, ok := safeCompileMessages[message]; ok {
		return message
	}
	return fallback
}

func httpValidationPath(err error) string {
	if err == nil {
		return "action"
	}
	switch err.Error() {
	case "unsupported HTTP method":
		return "action.method"
	case "header count exceeds limit", "invalid header name", "duplicate header", "forbidden header", "invalid header value", "event placeholders are not allowed", "unterminated environment reference", "invalid environment reference", "undefined environment reference":
		return "action.headers"
	default:
		return "action.url"
	}
}

func set(items ...string) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		out[item] = true
	}
	return out
}

func promptScopeAllowed(event Event, scope PromptScope) bool {
	switch event {
	case EventSessionStart:
		return scope == ScopeNext || scope == ScopeSession
	case EventTurnStart, EventMessageBefore, EventMessageAfter, EventToolBefore, EventToolAfter, EventCompactBefore, EventCompactAfter:
		return scope == ScopeNext || scope == ScopeTurn || scope == ScopeSession
	case EventTurnEnd:
		return scope == ScopeSession
	default:
		return false
	}
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type environmentExpansion struct {
	name  string
	value string
}

func compileEnv(values map[string]string, limits Limits, lookup func(string) (string, bool), runtimeRedactor *redact.RuntimeRedactor) (map[string]string, error) {
	if len(values) > limits.CommandEnvCount {
		return nil, fmt.Errorf("env count exceeds limit")
	}
	result := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !envKeyPattern.MatchString(key) || len(key) > limits.EnvKeyBytes || key == "PWD" {
			return nil, fmt.Errorf("invalid env key")
		}
		expanded, expansions, err := expandEnvironment(values[key], lookup)
		if err != nil {
			return nil, err
		}
		if strings.ContainsRune(expanded, 0) || len(expanded) > limits.EnvValueBytes {
			return nil, fmt.Errorf("env value exceeds limit or contains NUL")
		}
		registerExpansion(runtimeRedactor, key, expanded, expansions)
		result[key] = expanded
	}
	return result, nil
}

func expandEnvironment(value string, lookup func(string) (string, bool)) (string, []environmentExpansion, error) {
	if strings.Contains(value, "{{") {
		return "", nil, fmt.Errorf("event placeholders are not allowed")
	}
	const escaped = "\x00hook-dollar-open\x00"
	value = strings.ReplaceAll(value, "$${", escaped)
	var output strings.Builder
	expansions := []environmentExpansion{}
	for len(value) > 0 {
		start := strings.Index(value, "${")
		if start < 0 {
			output.WriteString(value)
			break
		}
		output.WriteString(value[:start])
		value = value[start+2:]
		end := strings.IndexByte(value, '}')
		if end < 0 {
			return "", nil, fmt.Errorf("unterminated environment reference")
		}
		name := value[:end]
		if !envKeyPattern.MatchString(name) {
			return "", nil, fmt.Errorf("invalid environment reference")
		}
		expanded, ok := lookup(name)
		if !ok {
			return "", nil, fmt.Errorf("undefined environment reference")
		}
		output.WriteString(expanded)
		expansions = append(expansions, environmentExpansion{name: name, value: expanded})
		value = value[end+1:]
	}
	return strings.ReplaceAll(output.String(), escaped, "${"), expansions, nil
}

func registerExpansion(runtimeRedactor *redact.RuntimeRedactor, key, value string, expansions []environmentExpansion) {
	if runtimeRedactor == nil {
		return
	}
	sensitiveField := redact.IsSensitiveKey(key)
	if len(expansions) == 0 {
		if sensitiveField && value != "" {
			runtimeRedactor.RegisterSecret(value)
		}
		return
	}
	for _, expansion := range expansions {
		if expansion.value != "" && (sensitiveField || redact.IsSensitiveKey(expansion.name)) {
			runtimeRedactor.RegisterSecret(expansion.value)
		}
	}
}
