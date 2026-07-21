package command

import (
	"fmt"
	"strings"
)

const ReviewPrompt = `请审查当前工作区的所有未提交改动。只做审查，不要修改文件，也不要创建提交。

优先查找并报告：
1. 正确性缺陷和边界条件错误；
2. 安全风险或敏感信息泄露；
3. 可能导致既有行为回退的改动；
4. 缺失、薄弱或无法证明行为的测试。

按严重程度从高到低列出发现。每项给出文件与尽可能精确的位置、触发条件、实际影响和修复方向。若没有发现问题，明确说明，并指出仍存在的验证盲区。`

func Builtins() []Definition {
	return []Definition{
		{Name: "help", Aliases: []string{"h", "?"}, Description: "显示可用命令", Usage: "/help", Type: TypeLocal, Handler: helpHandler},
		{Name: "compact", Aliases: []string{"ctx"}, Description: "压缩当前会话上下文", Usage: "/compact", Type: TypeLocal, Handler: compactHandler},
		{Name: "clear", Aliases: []string{"cls"}, Description: "清空当前消息显示", Usage: "/clear", Type: TypeUI, Handler: clearHandler},
		{Name: "plan", Aliases: []string{"p"}, Description: "进入当前会话的计划模式", Usage: "/plan", Type: TypeUI, Handler: planHandler},
		{Name: "do", Aliases: []string{"d"}, Description: "退出计划模式并恢复默认执行", Usage: "/do", Type: TypeUI, Handler: doHandler},
		{Name: "session", Aliases: []string{"sess"}, Description: "显示当前会话摘要", Usage: "/session", Type: TypeLocal, Handler: sessionHandler},
		{Name: "memory", Aliases: []string{"mem"}, Description: "显示记忆状态与条目数量", Usage: "/memory", Type: TypeLocal, Handler: memoryHandler},
		{Name: "permission", Aliases: []string{"perm"}, Description: "显示当前权限状态", Usage: "/permission", Type: TypeLocal, Handler: permissionHandler},
		{Name: "status", Aliases: []string{"st"}, Description: "显示统一运行状态", Usage: "/status", Type: TypeLocal, Handler: statusHandler},
		{Name: "review", Aliases: []string{"rv"}, Description: "让 AI 审查当前未提交改动", Usage: "/review", Type: TypePrompt, Handler: reviewHandler},
		{Name: "permissions", Description: "兼容旧权限状态命令", Usage: "/permissions status", Type: TypeLocal, Hidden: true, Handler: legacyPermissionsHandler},
		{Name: "mcp", Description: "兼容旧 MCP 状态命令", Usage: "/mcp status", Type: TypeLocal, Hidden: true, Handler: legacyMCPHandler},
		{Name: "diagnostics", Description: "兼容旧诊断命令", Usage: "/diagnostics", Type: TypeLocal, Hidden: true, Handler: legacyDiagnosticsHandler},
	}
}

func helpHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/help"); err != nil {
		return err
	}
	lines := []string{"可用命令："}
	for _, definition := range context.Registry.Visible() {
		aliases := make([]string, 0, len(definition.Aliases))
		for _, alias := range definition.Aliases {
			aliases = append(aliases, "/"+alias)
		}
		line := fmt.Sprintf("/%s", definition.Name)
		if len(aliases) > 0 {
			line += " (" + strings.Join(aliases, ", ") + ")"
		}
		line += " — " + definition.Description + "；用法: " + definition.Usage
		if definition.ArgHint != "" {
			line += "；参数: " + definition.ArgHint
		}
		lines = append(lines, line)
	}
	context.Controller.DisplayNotice(strings.Join(lines, "\n"))
	return nil
}

func compactHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/compact"); err != nil {
		return err
	}
	message, err := context.Controller.CompactContext()
	if err != nil {
		return err
	}
	context.Controller.DisplayNotice(message)
	return nil
}

func clearHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/clear"); err != nil {
		return err
	}
	context.Controller.ClearMessages()
	context.Controller.DisplayNotice("当前消息显示已清空，会话历史和上下文保持不变")
	return nil
}

func planHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/plan"); err != nil {
		return err
	}
	context.Controller.SwitchMode(ModePlan)
	context.Controller.RefreshStatus()
	context.Controller.DisplayNotice("已进入计划模式")
	return nil
}

func doHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/do"); err != nil {
		return err
	}
	context.Controller.SwitchMode(ModeDefault)
	context.Controller.RefreshStatus()
	context.Controller.DisplayNotice("已恢复默认执行模式")
	return nil
}

func sessionHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/session"); err != nil {
		return err
	}
	status := context.Controller.SessionStatus()
	id := status.ID
	if id == "" {
		id = "none"
	}
	context.Controller.DisplayNotice(fmt.Sprintf("会话: id=%s messages=%d mode=%s streaming=%t", id, status.MessageCount, modeText(status.Mode), status.Streaming))
	return nil
}

func memoryHandler(context ExecutionContext, invocation Invocation) error {
	if strings.TrimSpace(invocation.Args) != "" {
		message, err := context.Controller.LegacyMemory(invocation.Args)
		if err != nil {
			return err
		}
		context.Controller.DisplayNotice(message)
		return nil
	}
	status := context.Controller.MemoryStatus()
	message := fmt.Sprintf("memory 状态: user=%s(%d) project=%s(%d)", enabledText(status.User.Enabled), status.User.Count, enabledText(status.Project.Enabled), status.Project.Count)
	if len(status.Diagnostics) > 0 {
		message += " | diagnostics=" + strings.Join(status.Diagnostics, "; ")
	}
	context.Controller.DisplayNotice(message)
	return nil
}

func permissionHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/permission"); err != nil {
		return err
	}
	status := context.Controller.PermissionStatus()
	context.Controller.DisplayNotice(fmt.Sprintf("权限状态: mode=%s session=%d local=%d project=%d user=%d load_errors=%d", status.Mode, status.SessionRules, status.LocalRules, status.ProjectRules, status.UserRules, status.LoadErrors))
	return nil
}

func statusHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/status"); err != nil {
		return err
	}
	status := context.Controller.RuntimeStatus()
	usage := context.Controller.TokenUsage()
	recentError := status.RecentError
	if strings.TrimSpace(recentError) == "" {
		recentError = "none"
	}
	mcp := status.MCP
	if strings.TrimSpace(mcp) == "" {
		mcp = "none"
	}
	context.Controller.DisplayNotice(fmt.Sprintf("运行状态: provider=%s model=%s mode=%s streaming=%t tokens=%d_in/%d_out cache=%d_create/%d_read mcp=%s recent_error=%s", status.Provider, status.Model, modeText(status.Mode), status.Streaming, usage.Input, usage.Output, usage.CacheCreation, usage.CacheRead, mcp, recentError))
	return nil
}

func reviewHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/review"); err != nil {
		return err
	}
	context.Controller.SendUserMessage(ReviewPrompt)
	return nil
}

func legacyPermissionsHandler(context ExecutionContext, invocation Invocation) error {
	if strings.TrimSpace(invocation.Args) != "status" {
		return fmt.Errorf("用法: /permissions status")
	}
	status := context.Controller.PermissionStatus()
	context.Controller.DisplayNotice(fmt.Sprintf("本地权限状态，不会发送给模型: mode=%s session=%d local=%d project=%d user=%d load_errors=%d", status.Mode, status.SessionRules, status.LocalRules, status.ProjectRules, status.UserRules, status.LoadErrors))
	return nil
}

func legacyMCPHandler(context ExecutionContext, invocation Invocation) error {
	if strings.TrimSpace(invocation.Args) != "status" {
		return fmt.Errorf("用法: /mcp status")
	}
	message, err := context.Controller.LegacyMCPStatus()
	if err != nil {
		return err
	}
	context.Controller.DisplayNotice(message)
	return nil
}

func legacyDiagnosticsHandler(context ExecutionContext, invocation Invocation) error {
	if err := rejectArgs(invocation, "/diagnostics"); err != nil {
		return err
	}
	message, err := context.Controller.LegacyDiagnostics()
	if err != nil {
		return err
	}
	context.Controller.DisplayNotice(message)
	return nil
}

func rejectArgs(invocation Invocation, usage string) error {
	if strings.TrimSpace(invocation.Args) != "" {
		return fmt.Errorf("用法: %s", usage)
	}
	return nil
}

func enabledText(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func modeText(mode Mode) string {
	if mode == ModePlan {
		return string(ModePlan)
	}
	return string(ModeDefault)
}
