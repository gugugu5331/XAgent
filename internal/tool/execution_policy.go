package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ExecutionPolicy describes scheduling properties independently from a
// tool's user-facing risk classification.
type ExecutionPolicy struct {
	ReadOnly       bool
	SideEffectFree bool
	ConcurrentSafe bool
}

// AllowsConcurrentExecution reports whether the policy satisfies both
// requirements for concurrent scheduling.
func (p ExecutionPolicy) AllowsConcurrentExecution() bool {
	return p.ReadOnly && p.ConcurrentSafe
}

// AllowsBackgroundByDefault reports whether a tool is safe to expose to a
// detached task without an explicit background-tool override. These three
// properties are deliberately independent from the user-facing Risk value.
func (p ExecutionPolicy) AllowsBackgroundByDefault() bool {
	return p.ReadOnly && p.SideEffectFree && p.ConcurrentSafe
}

// BackgroundPolicy is an immutable-by-convention placement filter. A nil
// AllowedNames list asks construction helpers to derive the metadata-safe
// default; a non-nil empty list intentionally exposes no background tools.
type BackgroundPolicy struct {
	Enabled      bool
	AllowedNames []string
	Fingerprint  string
}

// DefaultBackgroundPolicy derives the conservative background allowlist from
// a sealed registry. Explicit configuration may later expand this list, but
// it still participates in the same monotonic capability intersection.
func DefaultBackgroundPolicy(registry *Registry) (BackgroundPolicy, error) {
	if registry == nil {
		return BackgroundPolicy{}, fmt.Errorf("工具注册中心不能为空")
	}
	if !registry.IsSealed() {
		return BackgroundPolicy{}, fmt.Errorf("工具注册中心尚未封存")
	}
	names := make([]string, 0)
	for _, name := range registry.Names() {
		descriptor, ok := registry.Descriptor(name)
		if !ok {
			return BackgroundPolicy{}, fmt.Errorf("工具 %q 元数据缺失", name)
		}
		if descriptor.Policy.AllowsBackgroundByDefault() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	policy := BackgroundPolicy{Enabled: true, AllowedNames: names}
	policy.Fingerprint = backgroundPolicyFingerprint(policy.Enabled, policy.AllowedNames)
	return policy, nil
}

// Validate checks that an explicit policy references only frozen tools. The
// input order is not trusted or significant; fingerprints use canonical sort.
func (p BackgroundPolicy) Validate(registry *Registry) error {
	if registry == nil {
		return fmt.Errorf("工具注册中心不能为空")
	}
	if !registry.IsSealed() {
		return fmt.Errorf("工具注册中心尚未封存")
	}
	seen := make(map[string]struct{}, len(p.AllowedNames))
	for _, name := range p.AllowedNames {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("后台工具名称无效")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("后台工具 %q 重复", name)
		}
		if !registry.knowsRegisteredName(name) {
			return fmt.Errorf("后台工具 %q 未注册", name)
		}
		seen[name] = struct{}{}
	}
	if p.Fingerprint != "" {
		canonical := append([]string(nil), p.AllowedNames...)
		sort.Strings(canonical)
		if expected := backgroundPolicyFingerprint(p.Enabled, canonical); p.Fingerprint != expected {
			return fmt.Errorf("后台工具策略 fingerprint 不匹配")
		}
	}
	return nil
}

// Allows reports membership without changing placement state.
func (p BackgroundPolicy) Allows(name string) bool {
	if !p.Enabled {
		return true
	}
	for _, allowed := range p.AllowedNames {
		if allowed == name {
			return true
		}
	}
	return false
}

func normalizeBackgroundPolicy(registry *Registry, policy BackgroundPolicy) (BackgroundPolicy, error) {
	if policy.AllowedNames == nil {
		derived, err := DefaultBackgroundPolicy(registry)
		if err != nil {
			return BackgroundPolicy{}, err
		}
		derived.Enabled = policy.Enabled
		derived.Fingerprint = backgroundPolicyFingerprint(derived.Enabled, derived.AllowedNames)
		return derived, nil
	}
	if err := policy.Validate(registry); err != nil {
		return BackgroundPolicy{}, err
	}
	policy.AllowedNames = append([]string(nil), policy.AllowedNames...)
	sort.Strings(policy.AllowedNames)
	policy.Fingerprint = backgroundPolicyFingerprint(policy.Enabled, policy.AllowedNames)
	return policy, nil
}

func backgroundPolicyFingerprint(enabled bool, names []string) string {
	hash := sha256.New()
	if enabled {
		_, _ = hash.Write([]byte("background\x001\x00"))
	} else {
		_, _ = hash.Write([]byte("background\x000\x00"))
	}
	for _, name := range names {
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
