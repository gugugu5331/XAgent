package permission

import (
	"fmt"
	"strings"
)

type Rule struct {
	Tool        string    `yaml:"tool"`
	Pattern     string    `yaml:"pattern"`
	MatchType   string    `yaml:"match_type"`
	Effect      string    `yaml:"effect"`
	PathParam   string    `yaml:"path_param,omitempty"`
	Description string    `yaml:"description,omitempty"`
	Trust       RuleTrust `yaml:"-"`
}

type RuleTrust string

const RuleTrustLegacyUntrusted RuleTrust = "legacy_untrusted"

type RuleFile struct {
	Version int    `yaml:"version"`
	Rules   []Rule `yaml:"rules"`
}

type RuleLayer struct {
	Source Source
	Rules  []Rule
}

type MatchType string

const (
	MatchExact MatchType = "exact"
	MatchGlob  MatchType = "glob"
)

type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

var knownTools = map[string]bool{
	"Bash":  true,
	"Read":  true,
	"Write": true,
	"Edit":  true,
	"Glob":  true,
	"Grep":  true,
}

func isKnownTool(toolName string) bool {
	return knownTools[toolName] || strings.HasPrefix(toolName, "mcp__")
}

func (r Rule) Display() string {
	return fmt.Sprintf("%s(%s)", r.Tool, r.Pattern)
}

func (r Rule) Validate() error {
	if !isKnownTool(r.Tool) {
		return fmt.Errorf("unknown tool %q", r.Tool)
	}
	if strings.TrimSpace(r.Pattern) == "" {
		return fmt.Errorf("rule pattern is empty")
	}
	if r.MatchType != string(MatchExact) && r.MatchType != string(MatchGlob) {
		return fmt.Errorf("unsupported match_type %q", r.MatchType)
	}
	if r.Effect != string(EffectAllow) && r.Effect != string(EffectDeny) {
		return fmt.Errorf("unsupported effect %q", r.Effect)
	}
	return nil
}

func ValidateRuleFile(file RuleFile) error {
	if file.Version != SupportedRuleVersion {
		return fmt.Errorf("unsupported permission rule version %d", file.Version)
	}
	for index, rule := range file.Rules {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("rule %d: %w", index, err)
		}
	}
	return nil
}

func DefaultPathParam(toolName string, arguments map[string]any) string {
	switch toolName {
	case "Read", "Write", "Edit":
		return "path"
	case "Glob", "Grep":
		if _, ok := arguments["path"]; ok {
			return "path"
		}
		if _, ok := arguments["root"]; ok {
			return "root"
		}
	}
	return ""
}
