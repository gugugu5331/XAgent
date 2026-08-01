package permission

import (
	"context"
	"io"

	"xagent/internal/redact"
	"xagent/internal/safefs"

	"gopkg.in/yaml.v3"
)

const localRuleSlot = ".xagent/permissions.local.yaml"

type Writer struct {
	root       *safefs.Root
	capability safefs.Capability
}

// NewWriter constructs the only permission-file writer. The capability is
// deliberately kept separate from Root by safefs and is validated again by
// AtomicWrite for every publication.
func NewWriter(root *safefs.Root, capability safefs.Capability) Writer {
	return Writer{root: root, capability: capability}
}

func (w Writer) PreviewRule(normalized NormalizedCall) Rule {
	return Rule{
		Tool:        normalized.Call.Name,
		Pattern:     normalized.RuleValue,
		MatchType:   string(MatchExact),
		Effect:      string(EffectAllow),
		Description: "Permanent allow from confirmation",
	}
}

func (w Writer) WriteLocal(rule Rule) error {
	if err := rule.ValidatePermanent(); err != nil {
		return err
	}
	file := RuleFile{Version: SupportedRuleVersion}
	if data, err := readRuleFile(w.root, localRuleSlot); err == nil {
		existing, err := DecodeRuleFile(data)
		if err != nil {
			return err
		}
		file = existing
	}
	file.Rules = appendUniqueRule(file.Rules, rule)
	if err := ValidateRuleFile(file); err != nil {
		return err
	}
	return w.root.AtomicWrite(context.Background(), w.capability, localRuleSlot, 0o600, func(destination io.Writer) error {
		encoder := yaml.NewEncoder(destination)
		encoder.SetIndent(2)
		if err := encoder.Encode(file); err != nil {
			_ = encoder.Close()
			return err
		}
		return encoder.Close()
	})
}

func ruleContainsSecret(rule Rule) bool {
	for _, value := range []string{rule.Pattern, rule.PathParam, rule.Description} {
		if value != "" && redact.Text(value) != value {
			return true
		}
	}
	return false
}

func appendUniqueRule(rules []Rule, rule Rule) []Rule {
	for _, existing := range rules {
		if existing.Tool == rule.Tool && existing.Pattern == rule.Pattern && existing.MatchType == rule.MatchType && existing.Effect == rule.Effect && existing.PathParam == rule.PathParam {
			return rules
		}
	}
	return append(rules, rule)
}
