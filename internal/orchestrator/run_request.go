package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"xagent/internal/config"
	"xagent/internal/provider"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

type RunMode string

const (
	RunModeDefault RunMode = "default"
	RunModePlan    RunMode = "plan"
	RunModeDo      RunMode = "do"
)

type RunRequest struct {
	UserText string
	Mode     RunMode
	Profile  skill.ExecutionProfile
	Activity *skill.Activity
}

type RunResult struct {
	FinalText string
	Usage     provider.Usage
	Duration  time.Duration
	Reason    StopReason
}

type RunOptions struct {
	MaxIterations       int
	MaxUnknownToolCalls int
}

type StopReason string

const (
	StopReasonCompleted     StopReason = "completed"
	StopReasonMaxIterations StopReason = "max_iterations"
	StopReasonCancelled     StopReason = "cancelled"
	StopReasonUnknownTool   StopReason = "unknown_tool"
	StopReasonProviderError StopReason = "provider_error"
)

func defaultRunOptions() RunOptions {
	return RunOptions{MaxIterations: 10, MaxUnknownToolCalls: 2}
}

func runOptionsFromConfig(agent config.AgentConfig) RunOptions {
	options := defaultRunOptions()
	if agent.MaxIterations > 0 {
		options.MaxIterations = agent.MaxIterations
	}
	if agent.MaxUnknownToolCalls > 0 {
		options.MaxUnknownToolCalls = agent.MaxUnknownToolCalls
	}
	return options
}

func parseRunRequest(text string) (RunRequest, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return RunRequest{}, fmt.Errorf("请输入非空内容")
	}
	for _, prefix := range []struct {
		text string
		mode RunMode
	}{
		{text: "/plan", mode: RunModePlan},
		{text: "/do", mode: RunModeDo},
	} {
		if strings.HasPrefix(trimmed, prefix.text) {
			rest := strings.TrimPrefix(trimmed, prefix.text)
			if rest == "" {
				return RunRequest{}, fmt.Errorf("请输入非空内容")
			}
			if strings.TrimSpace(rest) != rest || rest[0] == ' ' {
				userText := strings.TrimSpace(rest)
				if userText == "" {
					return RunRequest{}, fmt.Errorf("请输入非空内容")
				}
				return RunRequest{UserText: userText, Mode: prefix.mode}, nil
			}
		}
	}
	return RunRequest{UserText: trimmed, Mode: RunModeDefault}, nil
}

func (o *Orchestrator) registryForMode(mode RunMode) (*tool.Registry, error) {
	if mode == RunModePlan {
		if o.readOnlyRegistry == nil {
			return nil, fmt.Errorf("只读工具注册中心不可用")
		}
		return o.readOnlyRegistry, nil
	}
	return o.registry, nil
}
