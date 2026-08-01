package permission

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"xagent/internal/safefs"

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
	absolute, err := filepath.Abs(path)
	if err != nil {
		return RuleFile{}, errors.New("resolve permission rule file failed")
	}
	absolute = filepath.Clean(absolute)
	parent := filepath.Dir(absolute)
	if _, err := os.Lstat(parent); err != nil {
		if os.IsNotExist(err) {
			return RuleFile{}, os.ErrNotExist
		}
		return RuleFile{}, errors.New("inspect permission rule directory failed")
	}
	opened, err := safefs.Bootstrap(parent, safefs.Policy{})
	if err != nil {
		return RuleFile{}, errors.New("open permission rule directory failed")
	}
	data, readErr := readRuleFile(opened.Root, filepath.Base(absolute))
	closeErr := opened.Root.Close()
	if readErr != nil {
		if _, statErr := os.Lstat(absolute); os.IsNotExist(statErr) {
			return RuleFile{}, os.ErrNotExist
		}
		return RuleFile{}, readErr
	}
	if closeErr != nil {
		return RuleFile{}, errors.New("close permission rule directory failed")
	}
	return DecodeRuleFile(data)
}

func readRuleFile(root *safefs.Root, relative string) ([]byte, error) {
	file, err := root.OpenRead(context.Background(), relative)
	if err != nil {
		return nil, errors.New("open permission rule file failed")
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.New("read permission rule file failed")
	}
	return data, nil
}

func DecodeRuleFile(data []byte) (RuleFile, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var file RuleFile
	if err := decoder.Decode(&file); err != nil {
		return RuleFile{}, err
	}
	legacy := file.Version == 0
	if legacy {
		file.Version = 1
	}
	if err := ValidateRuleFile(file); err != nil {
		return RuleFile{}, err
	}
	if legacy {
		markLegacyUntrustedRules(file.Rules)
	}
	return file, nil
}

func markLegacyUntrustedRules(rules []Rule) {
	for index := range rules {
		if rules[index].Tool == "Bash" && rules[index].Effect == string(EffectAllow) {
			rules[index].Trust = RuleTrustLegacyUntrusted
		}
	}
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
