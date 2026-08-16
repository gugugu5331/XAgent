package worktree

import (
	"errors"
	"testing"
	"time"
)

func TestConfigCloneDeepCopiesRules(t *testing.T) {
	original := Config{
		Lifecycle: LifecycleConfig{RetentionTTL: 24 * time.Hour, GitTimeout: time.Second},
		Limits:    Limits{MaxActive: 2, MaxNameBytes: 64},
		Init: InitConfig{
			Copy:        []CopyRule{{Source: "local.yaml", Target: "config/local.yaml"}},
			Link:        []LinkRule{{Source: "node_modules", Target: "node_modules"}},
			IgnoredCopy: []CopyRule{{Source: ".env.test", Target: ".env.test"}},
			GitHooks:    GitHooksRule{Enabled: true, Path: ".githooks"},
		},
	}

	cloned := original.Clone()
	cloned.Init.Copy[0].Source = "changed"
	cloned.Init.Link[0].Target = "changed"
	cloned.Init.IgnoredCopy[0].Target = "changed"

	if original.Init.Copy[0].Source != "local.yaml" || original.Init.Link[0].Target != "node_modules" || original.Init.IgnoredCopy[0].Target != ".env.test" {
		t.Fatal("Clone 必须深拷贝初始化规则")
	}
}

func TestConfigClonePreservesAbsentAndExplicitEmpty(t *testing.T) {
	absent := Config{}.Clone()
	if absent.Init.Copy != nil {
		t.Fatal("absent 切片必须保持 nil")
	}
	explicitEmpty := Config{Init: InitConfig{Copy: []CopyRule{}}}.Clone()
	if explicitEmpty.Init.Copy == nil || len(explicitEmpty.Init.Copy) != 0 {
		t.Fatal("显式空切片必须保持 non-nil empty")
	}
}

func TestConfigValidateRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "negative duration", cfg: Config{Lifecycle: LifecycleConfig{GitTimeout: -time.Second}}},
		{name: "negative limit", cfg: Config{Limits: Limits{MaxActive: -1}}},
		{name: "empty copy source", cfg: Config{Init: InitConfig{Copy: []CopyRule{{Target: "x"}}}}},
		{name: "empty link target", cfg: Config{Init: InitConfig{Link: []LinkRule{{Source: "x"}}}}},
		{name: "enabled hooks without path", cfg: Config{Init: InitConfig{GitHooks: GitHooksRule{Enabled: true}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Fatal("预期拒绝非法配置")
			}
		})
	}
}

func TestStateValidationAndTransitions(t *testing.T) {
	for _, state := range []State{StateCreating, StateInitializing, StateReady, StateActive, StateSettling, StateRetained, StateDeleting, StateDeleted, StatePartial, StateManualAttention} {
		if !state.Valid() {
			t.Fatalf("合法状态被拒绝：%q", state)
		}
	}
	if State("unknown").Valid() {
		t.Fatal("未知状态不应合法")
	}
	if !CanTransition(StateCreating, StateInitializing) || !CanTransition(StateSettling, StateRetained) || !CanTransition(StateDeleting, StateDeleted) {
		t.Fatal("合法生命周期迁移被拒绝")
	}
	if CanTransition(StateActive, StateDeleted) || CanTransition(StateDeleted, StateActive) {
		t.Fatal("不得跨过结算或从终态复活")
	}
}

func TestSafeErrorUsesStableCodeAndSupportsErrorsAs(t *testing.T) {
	err := NewSafeError(CodeInvalidLogicalName, "logical name is invalid", errors.New("private /tmp/path"))
	if err.Error() != "logical name is invalid" || err.Code != CodeInvalidLogicalName {
		t.Fatalf("SafeError 不稳定：%#v", err)
	}
	var target *SafeError
	if !errors.As(err, &target) || !errors.Is(err, err.Unwrap()) {
		t.Fatal("SafeError 应保留内部因果链但只展示安全消息")
	}
}
