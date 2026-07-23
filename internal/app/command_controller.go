package app

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/command"
	"xagent/internal/memory"
	"xagent/internal/orchestrator"
)

type commandController struct {
	model *Model
	cmd   tea.Cmd
}

var _ command.Controller = (*commandController)(nil)

func (c *commandController) DisplayNotice(text string) {
	c.model.status.Notice = c.model.redactText(text)
	c.model.status.Error = nil
}

func (c *commandController) DisplayError(err error) {
	if err == nil {
		c.model.status.Error = nil
		return
	}
	safe := c.model.redactError(err)
	c.model.status.Notice = ""
	c.model.status.Error = safe
	c.model.lastError = safe
}

func (c *commandController) SendUserMessage(text string) {
	c.cmd = c.model.submitUserMessage(text)
}

func (c *commandController) ExecuteSkill(name string, args string, raw string) error {
	cmd, err := c.model.executeSkill(name, args, raw)
	if err != nil {
		return err
	}
	c.cmd = cmd
	return nil
}

func (c *commandController) ClearMessages() {
	c.model.messages.Clear()
	c.model.messagesCleared = true
	if c.model.skillActivity != nil {
		c.model.skillActivity.Clear()
	}
	c.model.status.ActiveSkills = ""
	c.model.status.RequestModel = ""
}

func (c *commandController) SwitchMode(mode command.Mode) {
	if mode == command.ModePlan {
		c.model.mode = orchestrator.RunModePlan
	} else {
		c.model.mode = orchestrator.RunModeDefault
	}
	c.model.status.Mode = string(c.model.mode)
}

func (c *commandController) CurrentMode() command.Mode {
	if c.model.mode == orchestrator.RunModePlan {
		return command.ModePlan
	}
	return command.ModeDefault
}

func (c *commandController) RefreshStatus() {
	c.model.status.Mode = string(c.model.mode)
	c.model.refreshMCPStatus()
}

func (c *commandController) CompactContext() (string, error) {
	if c.model.conversation == nil {
		return "当前没有会话需要压缩", nil
	}
	if c.model.orchestrator == nil {
		return "", fmt.Errorf("Orchestrator 未启用")
	}
	result, err := c.model.orchestrator.CompactContext(context.Background(), c.model.conversation)
	if err != nil {
		return "", err
	}
	if !c.model.messagesCleared {
		c.model.messages.SetMessages(c.model.conversation.Messages)
	}
	return compactNotice(result.Changed, result.Externalized, result.Summarized), nil
}

func (c *commandController) SessionStatus() command.SessionStatus {
	status := command.SessionStatus{Mode: c.CurrentMode(), Streaming: c.model.streaming}
	if c.model.conversation != nil {
		status.ID = c.model.conversation.ID
		status.MessageCount = len(c.model.conversation.Messages)
	}
	return status
}

func (c *commandController) MemoryStatus() command.MemoryStatus {
	result := command.MemoryStatus{}
	manager := c.model.deps.Memory
	if manager == nil {
		result.Diagnostics = []string{"memory 管理器未启用"}
		return result
	}
	status := manager.Status()
	result.User.Enabled = !status.UserDisabled
	result.Project.Enabled = !status.ProjectDisabled
	for _, item := range status.Diagnostics {
		if text := item.Safe(c.model.redactText).Text(); text != "" {
			result.Diagnostics = append(result.Diagnostics, text)
		}
	}
	if index, err := manager.LoadIndex(memory.ScopeUser); err != nil {
		result.Diagnostics = append(result.Diagnostics, c.model.redactText(err.Error()))
	} else {
		result.User.Count = len(index.Entries)
	}
	if index, err := manager.LoadIndex(memory.ScopeProject); err != nil {
		result.Diagnostics = append(result.Diagnostics, c.model.redactText(err.Error()))
	} else {
		result.Project.Count = len(index.Entries)
	}
	return result
}

func (c *commandController) PermissionStatus() command.PermissionStatus {
	if c.model.orchestrator == nil {
		return command.PermissionStatus{Mode: "default"}
	}
	status := c.model.orchestrator.PermissionStatus()
	return command.PermissionStatus{
		Mode: status.Mode, SessionRules: status.SessionRules, LocalRules: status.LocalRules,
		ProjectRules: status.ProjectRules, UserRules: status.UserRules, LoadErrors: status.LoadErrors,
	}
}

func (c *commandController) RuntimeStatus() command.RuntimeStatus {
	recentError := ""
	if c.model.lastError != nil {
		recentError = c.model.lastError.Error()
	}
	return command.RuntimeStatus{
		Provider: c.model.status.Provider, Model: c.model.status.Model, Mode: c.CurrentMode(),
		Streaming: c.model.streaming, MCP: c.model.status.MCP, RecentError: recentError,
	}
}

func (c *commandController) TokenUsage() command.TokenUsage {
	return command.TokenUsage{
		Input: c.model.status.InputTokens, Output: c.model.status.OutputTokens,
		CacheCreation: c.model.status.CacheCreationInputTokens, CacheRead: c.model.status.CacheReadInputTokens,
	}
}

func (c *commandController) LegacyMemory(args string) (string, error) {
	manager := c.model.deps.Memory
	if manager == nil {
		return "", fmt.Errorf("memory 管理器未启用")
	}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", fmt.Errorf("用法: /memory status|index|off|delete|rebuild")
	}
	switch fields[0] {
	case "status":
		if len(fields) != 1 {
			return "", fmt.Errorf("用法: /memory status")
		}
		return formatMemoryStatus(manager.Status()), nil
	case "index":
		if len(fields) > 2 {
			return "", fmt.Errorf("用法: /memory index [user|project]")
		}
		scope := memory.ScopeProject
		if len(fields) == 2 {
			parsed, err := parseMemoryScope(fields[1])
			if err != nil {
				return "", err
			}
			scope = parsed
		}
		index, err := manager.LoadIndex(scope)
		if err != nil {
			return "", err
		}
		return formatMemoryIndex(index), nil
	case "off":
		if len(fields) > 2 {
			return "", fmt.Errorf("用法: /memory off [user|project]")
		}
		scope := memory.ScopeProject
		if len(fields) == 2 {
			parsed, err := parseMemoryScope(fields[1])
			if err != nil {
				return "", err
			}
			scope = parsed
		}
		manager.Disable(scope)
		return fmt.Sprintf("memory %s 自动记忆已关闭", scope), nil
	case "delete":
		if len(fields) != 3 {
			return "", fmt.Errorf("用法: /memory delete <user|project> <id>")
		}
		scope, err := parseMemoryScope(fields[1])
		if err != nil {
			return "", err
		}
		if err := manager.DeleteNote(scope, fields[2]); err != nil {
			return "", err
		}
		return fmt.Sprintf("memory %s 记忆 %s 已删除", scope, fields[2]), nil
	case "rebuild":
		if len(fields) != 2 {
			return "", fmt.Errorf("用法: /memory rebuild <user|project>")
		}
		scope, err := parseMemoryScope(fields[1])
		if err != nil {
			return "", err
		}
		index, err := manager.RebuildIndex(scope)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("memory %s 索引已重建，%d 条", scope, len(index.Entries)), nil
	default:
		return "", fmt.Errorf("未知 memory 命令: %s", fields[0])
	}
}

func (c *commandController) LegacyMCPStatus() (string, error) {
	if c.model.deps.MCPStatus == nil {
		return "本地 MCP 状态: 未配置 MCP", nil
	}
	summary := c.model.deps.MCPStatus.Summary()
	parts := []string{fmt.Sprintf("本地 MCP 状态，不会发送给模型: configured=%d ready=%d failed=%d disabled=%d closed=%d", summary.Configured, summary.Ready, summary.Failed, summary.Disabled, summary.Closed)}
	for _, item := range summary.Diagnostics {
		text := strings.TrimSpace(item.Message)
		if item.Server != "" {
			text = item.Server + ": " + text
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return c.model.redactText(strings.Join(parts, "；")), nil
}

func (c *commandController) LegacyDiagnostics() (string, error) {
	collector := c.model.diagnostics
	if collector == nil {
		collector = c.model.deps.Diagnostics
	}
	if collector == nil || len(collector.List()) == 0 {
		return "本地诊断: 暂无诊断，不会发送给模型", nil
	}
	parts := make([]string, 0)
	for _, item := range collector.List() {
		text := item.Safe(c.model.redactText).Text()
		if item.Source != "" {
			text += " source=" + c.model.redactText(item.Source)
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return c.model.redactText("本地诊断，不会发送给模型: " + strings.Join(parts, "；")), nil
}
