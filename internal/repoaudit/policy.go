package repoaudit

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	PolicyVersion        = 1
	MaxPolicyBytes       = 1 << 20
	MaxBinaryBytes int64 = 1 << 20
)

// GitlinkDeclaration is the closed allow-list of intentional gitlinks.
type GitlinkDeclaration struct {
	Path string `yaml:"path"`
}

// FixtureExemption allows one rule for one exact repository path and content.
type FixtureExemption struct {
	Path          string `yaml:"path"`
	RuleID        RuleID `yaml:"rule_id"`
	ContentSHA256 string `yaml:"content_sha256"`
}

// BinaryDeclaration permits the binary type of one exact repository object.
// It never permits an oversized object or bypasses any other rule.
type BinaryDeclaration struct {
	Path          string `yaml:"path"`
	ContentSHA256 string `yaml:"content_sha256"`
	Bytes         int64  `yaml:"bytes"`
}

// Policy contains only controls that cannot weaken the fixed audit rules.
type Policy struct {
	Version           int                  `yaml:"version"`
	MaxBinaryBytes    int64                `yaml:"max_binary_bytes"`
	BinaryAllowlist   []BinaryDeclaration  `yaml:"binary_allowlist"`
	Gitlinks          []GitlinkDeclaration `yaml:"gitlinks"`
	FixtureExemptions []FixtureExemption   `yaml:"fixture_exemptions"`
}

// ParsePolicy strictly decodes one bounded YAML document.
func ParsePolicy(data []byte) (Policy, error) {
	if len(data) > MaxPolicyBytes {
		return Policy{}, fmt.Errorf("repoaudit policy exceeds %d bytes", MaxPolicyBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return Policy{}, errors.New("repoaudit policy contains NUL")
	}

	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return Policy{}, fmt.Errorf("decode repoaudit policy: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return Policy{}, errors.New("repoaudit policy must be one mapping document")
	}
	if err := validatePolicyNode(document.Content[0]); err != nil {
		return Policy{}, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Policy{}, errors.New("repoaudit policy contains a second document")
		}
		return Policy{}, fmt.Errorf("decode trailing repoaudit policy: %w", err)
	}

	var policy Policy
	strictDecoder := yaml.NewDecoder(bytes.NewReader(data))
	strictDecoder.KnownFields(true)
	if err := strictDecoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("decode repoaudit policy fields: %w", err)
	}
	var strictTrailing yaml.Node
	if err := strictDecoder.Decode(&strictTrailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Policy{}, errors.New("repoaudit policy contains a second document")
		}
		return Policy{}, fmt.Errorf("decode trailing repoaudit policy fields: %w", err)
	}
	if err := policy.validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func validatePolicyNode(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Alias != nil {
		return errors.New("repoaudit policy aliases are not allowed")
	}
	if node.Tag == "!!null" {
		return errors.New("repoaudit policy null values are not allowed")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return errors.New("repoaudit policy keys must be strings")
			}
			if _, ok := seen[key.Value]; ok {
				return fmt.Errorf("repoaudit policy contains duplicate key %q", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := validatePolicyNode(child); err != nil {
			return err
		}
	}
	return nil
}

func (p Policy) validate() error {
	if p.Version != PolicyVersion {
		return fmt.Errorf("unsupported repoaudit policy version %d", p.Version)
	}
	if p.MaxBinaryBytes != MaxBinaryBytes {
		return fmt.Errorf("max_binary_bytes must be fixed at %d", MaxBinaryBytes)
	}
	binaries := make(map[string]struct{}, len(p.BinaryAllowlist))
	for i, declaration := range p.BinaryAllowlist {
		if err := validateRepoPath(declaration.Path); err != nil {
			return fmt.Errorf("binary_allowlist[%d]: %w", i, err)
		}
		if !isLowerSHA256(declaration.ContentSHA256) {
			return fmt.Errorf("binary_allowlist[%d]: content_sha256 must be 64 lowercase hexadecimal characters", i)
		}
		if declaration.Bytes <= 0 || declaration.Bytes > MaxBinaryBytes {
			return fmt.Errorf("binary_allowlist[%d]: bytes must be between 1 and %d", i, MaxBinaryBytes)
		}
		if _, exists := binaries[declaration.Path]; exists {
			return fmt.Errorf("duplicate binary allowlist path %q", declaration.Path)
		}
		binaries[declaration.Path] = struct{}{}
	}
	gitlinks := make(map[string]struct{}, len(p.Gitlinks))
	for i, declaration := range p.Gitlinks {
		if err := validateRepoPath(declaration.Path); err != nil {
			return fmt.Errorf("gitlinks[%d]: %w", i, err)
		}
		if _, exists := gitlinks[declaration.Path]; exists {
			return fmt.Errorf("duplicate gitlink declaration %q", declaration.Path)
		}
		gitlinks[declaration.Path] = struct{}{}
	}
	exemptions := make(map[string]struct{}, len(p.FixtureExemptions))
	for i, exemption := range p.FixtureExemptions {
		if err := validateRepoPath(exemption.Path); err != nil {
			return fmt.Errorf("fixture_exemptions[%d]: %w", i, err)
		}
		if exemption.RuleID == "" {
			return fmt.Errorf("fixture_exemptions[%d]: rule_id is required", i)
		}
		if exemption.RuleID != RuleSecret {
			return fmt.Errorf("fixture_exemptions[%d]: only the secret rule may be exempted", i)
		}
		if !isLowerSHA256(exemption.ContentSHA256) {
			return fmt.Errorf("fixture_exemptions[%d]: content_sha256 must be 64 lowercase hexadecimal characters", i)
		}
		key := exemption.Path + "\x00" + string(exemption.RuleID) + "\x00" + exemption.ContentSHA256
		if _, exists := exemptions[key]; exists {
			return fmt.Errorf("duplicate fixture exemption for %q", exemption.Path)
		}
		exemptions[key] = struct{}{}
	}
	return nil
}

func validateRepoPath(value string) error {
	if value == "" {
		return errors.New("path is required")
	}
	if strings.ContainsRune(value, 0) || strings.Contains(value, "\\") {
		return errors.New("path must use repository-relative slash form")
	}
	if strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return errors.New("path must be a normalized repository-relative path")
	}
	return nil
}

func isLowerSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
