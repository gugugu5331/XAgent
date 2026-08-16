package worktree

import (
	"fmt"
	"strings"
	"time"
)

// Config 是已经由上层完成默认值解析的 Worktree 配置。
type Config struct {
	Lifecycle LifecycleConfig
	Limits    Limits
	Init      InitConfig
}

type LifecycleConfig struct {
	RetentionTTL    time.Duration
	JanitorInterval time.Duration
	GitTimeout      time.Duration
	LockTimeout     time.Duration
	InitTimeout     time.Duration
	RecoveryTimeout time.Duration
	SettleTimeout   time.Duration
	JanitorTimeout  time.Duration
}

type Limits struct {
	MaxActive             int
	MaxRetained           int
	MaxNameBytes          int
	MaxSegmentBytes       int
	MaxDepth              int
	MaxInitFiles          int
	MaxInitBytes          int64
	MaxInitDepth          int
	MaxJanitorCandidates  int
	MaxJanitorConcurrency int
}

type InitConfig struct {
	Copy        []CopyRule
	Link        []LinkRule
	IgnoredCopy []CopyRule
	GitHooks    GitHooksRule
}

type CopyRule struct {
	Source string
	Target string
}

type LinkRule struct {
	Source string
	Target string
}

type GitHooksRule struct {
	Enabled bool
	Path    string
}

func (c Config) Clone() Config {
	clone := c
	clone.Init.Copy = cloneCopyRules(c.Init.Copy)
	clone.Init.Link = cloneLinkRules(c.Init.Link)
	clone.Init.IgnoredCopy = cloneCopyRules(c.Init.IgnoredCopy)
	return clone
}

func cloneCopyRules(rules []CopyRule) []CopyRule {
	if rules == nil {
		return nil
	}
	clone := make([]CopyRule, len(rules))
	copy(clone, rules)
	return clone
}

func cloneLinkRules(rules []LinkRule) []LinkRule {
	if rules == nil {
		return nil
	}
	clone := make([]LinkRule, len(rules))
	copy(clone, rules)
	return clone
}

// Validate 只做领域基础合法性校验；默认值与部署级硬上限由 internal/config 负责。
func (c Config) Validate() error {
	durations := []time.Duration{
		c.Lifecycle.RetentionTTL, c.Lifecycle.JanitorInterval, c.Lifecycle.GitTimeout,
		c.Lifecycle.LockTimeout, c.Lifecycle.InitTimeout, c.Lifecycle.RecoveryTimeout,
		c.Lifecycle.SettleTimeout, c.Lifecycle.JanitorTimeout,
	}
	for _, value := range durations {
		if value < 0 {
			return fmt.Errorf("worktree lifecycle duration must not be negative")
		}
	}
	limits := []int{
		c.Limits.MaxActive, c.Limits.MaxRetained, c.Limits.MaxNameBytes,
		c.Limits.MaxSegmentBytes, c.Limits.MaxDepth, c.Limits.MaxInitFiles,
		c.Limits.MaxInitDepth, c.Limits.MaxJanitorCandidates, c.Limits.MaxJanitorConcurrency,
	}
	for _, value := range limits {
		if value < 0 {
			return fmt.Errorf("worktree limit must not be negative")
		}
	}
	if c.Limits.MaxInitBytes < 0 {
		return fmt.Errorf("worktree byte limit must not be negative")
	}
	for _, rule := range append(append([]CopyRule(nil), c.Init.Copy...), c.Init.IgnoredCopy...) {
		if strings.TrimSpace(rule.Source) == "" || strings.TrimSpace(rule.Target) == "" {
			return fmt.Errorf("worktree copy rule requires source and target")
		}
	}
	for _, rule := range c.Init.Link {
		if strings.TrimSpace(rule.Source) == "" || strings.TrimSpace(rule.Target) == "" {
			return fmt.Errorf("worktree link rule requires source and target")
		}
	}
	if c.Init.GitHooks.Enabled && strings.TrimSpace(c.Init.GitHooks.Path) == "" {
		return fmt.Errorf("enabled worktree git hooks require a path")
	}
	return nil
}
