package permission

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"xagent/internal/redact"

	"gopkg.in/yaml.v3"
)

type Writer struct {
	ProjectRoot string
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
	if ruleContainsSecret(rule) {
		return fmt.Errorf("permission rule contains sensitive value")
	}
	if err := rule.Validate(); err != nil {
		return err
	}
	path := LocalRulePath(w.ProjectRoot)
	if err := validateLocalRulePath(w.ProjectRoot); err != nil {
		return err
	}
	file := RuleFile{Version: SupportedRuleVersion}
	if existing, err := LoadRuleFile(path); err == nil {
		file = existing
	} else if !os.IsNotExist(err) {
		return err
	}
	file.Rules = appendUniqueRule(file.Rules, rule)
	if err := ValidateRuleFile(file); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(file); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".permissions-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(buffer.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func validateLocalRulePath(projectRoot string) error {
	root, err := realProjectRoot(projectRoot)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, ".xagent")
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return os.ErrPermission
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if !isPathInsideRoot(root, resolved) {
			return os.ErrPermission
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
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
