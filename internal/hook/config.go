package hook

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

type MatchType string

const (
	MatchExact MatchType = "exact"
	MatchGlob  MatchType = "glob"
	MatchRegex MatchType = "regex"
)

type PromptScope string

const (
	ScopeNext    PromptScope = "next"
	ScopeTurn    PromptScope = "turn"
	ScopeSession PromptScope = "session"
)

type ActionType string

const (
	ActionCommand  ActionType = "command"
	ActionHTTP     ActionType = "http"
	ActionPrompt   ActionType = "prompt"
	ActionSubAgent ActionType = "subagent"
)

type Duration struct {
	time.Duration
	Set bool
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" || node.Value == "" {
		return fmt.Errorf("duration must be a non-empty string")
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration")
	}
	d.Duration, d.Set = parsed, true
	return nil
}

type File struct {
	Version int          `yaml:"version"`
	Hooks   []RuleConfig `yaml:"hooks"`
}

type RuleConfig struct {
	Event        Event           `yaml:"event"`
	If           *ConditionGroup `yaml:"if,omitempty"`
	Once         bool            `yaml:"once,omitempty"`
	Async        bool            `yaml:"async,omitempty"`
	Timeout      Duration        `yaml:"timeout,omitempty"`
	Action       ActionConfig    `yaml:"action"`
	present      map[string]bool
	line, column int
	locations    map[string]sourcePosition
}

type sourcePosition struct {
	line   int
	column int
}

type ConditionGroup struct {
	All     []Predicate `yaml:"all,omitempty"`
	Any     []Predicate `yaml:"any,omitempty"`
	present map[string]bool
}

type Predicate struct {
	Field        string    `yaml:"field"`
	Match        MatchType `yaml:"match"`
	Value        any       `yaml:"value"`
	Negate       bool      `yaml:"negate,omitempty"`
	present      map[string]bool
	line, column int
}

type ActionConfig struct {
	Type         ActionType        `yaml:"type"`
	Command      string            `yaml:"command,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	Decision     bool              `yaml:"decision,omitempty"`
	URL          string            `yaml:"url,omitempty"`
	Method       string            `yaml:"method,omitempty"`
	Headers      map[string]string `yaml:"headers,omitempty"`
	SendEvent    *bool             `yaml:"send_event,omitempty"`
	Content      string            `yaml:"content,omitempty"`
	Scope        PromptScope       `yaml:"scope,omitempty"`
	Agent        string            `yaml:"agent,omitempty"`
	Input        string            `yaml:"input,omitempty"`
	present      map[string]bool
	line, column int
}

type Source struct {
	Path             string
	Ordinal          int
	EffectiveOrdinal int
}

func (s Source) Key() string { return fmt.Sprintf("%s#%d", s.Path, s.Ordinal) }
