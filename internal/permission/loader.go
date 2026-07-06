package permission

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const SupportedRuleVersion = 1

type LoadError struct {
	Source Source
	Err    error
}

type LoadedRules struct {
	User    RuleLayer
	Project RuleLayer
	Local   RuleLayer
	Errors  []LoadError
}

func UserRulePath(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "xagent", "permissions.yaml")
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".config", "xagent", "permissions.yaml")
}

func ProjectRulePath(projectRoot string) string {
	return filepath.Join(projectRoot, ".xagent", "permissions.yaml")
}

func LocalRulePath(projectRoot string) string {
	return filepath.Join(projectRoot, ".xagent", "permissions.local.yaml")
}

func LoadRules(projectRoot string) LoadedRules {
	loaded := LoadedRules{
		User:    RuleLayer{Source: Source{Kind: SourceUserRule, Description: "user permissions"}},
		Project: RuleLayer{Source: Source{Kind: SourceProjectRule, Description: "project permissions"}},
		Local:   RuleLayer{Source: Source{Kind: SourceLocalRule, Description: "local permissions"}},
	}
	for _, item := range []struct {
		path   string
		layer  *RuleLayer
		source Source
	}{
		{UserRulePath(""), &loaded.User, loaded.User.Source},
		{ProjectRulePath(projectRoot), &loaded.Project, loaded.Project.Source},
		{LocalRulePath(projectRoot), &loaded.Local, loaded.Local.Source},
	} {
		rules, err := LoadRuleFile(item.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			loaded.Errors = append(loaded.Errors, LoadError{Source: item.source, Err: err})
			continue
		}
		item.layer.Rules = rules.Rules
	}
	return loaded
}

func LoadRuleFile(path string) (RuleFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RuleFile{}, err
	}
	file, err := DecodeRuleFile(data)
	if err != nil {
		return RuleFile{}, err
	}
	return file, nil
}

func DecodeRuleFile(data []byte) (RuleFile, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var file RuleFile
	if err := decoder.Decode(&file); err != nil {
		return RuleFile{}, err
	}
	if file.Version == 0 {
		file.Version = 1
	}
	if err := ValidateRuleFile(file); err != nil {
		return RuleFile{}, err
	}
	return file, nil
}

func ConfigErrorDecision(call Call, message string) Decision {
	return Decision{
		Kind:         DecisionDeny,
		Reason:       ReasonConfigError,
		Source:       Source{Kind: SourceHardConstraint, Description: "permission config error"},
		UserMessage:  message,
		ModelMessage: "Permission configuration is invalid, so this action cannot be performed safely.",
		Recoverable:  true,
	}
}

func configErrorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
