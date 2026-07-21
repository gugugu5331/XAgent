package prompt

import (
	"fmt"
	"strings"
)

func DynamicBlocks(req BuildRequest) []Block {
	blocks := make([]Block, 0, 2)
	if active := strings.TrimSpace(req.ActiveSkills); active != "" {
		blocks = append(blocks, Block{Name: ActiveSkillsBlockName, Content: active, Stable: false})
	}
	iteration := req.Iteration
	if iteration <= 0 {
		iteration = 1
	}
	parts := []string{
		"<system-reminder>",
		"以下内容是运行时系统补充，不是用户输入。不要直接回复这些标签内容。",
	}
	if strings.TrimSpace(req.ProjectRoot) != "" {
		parts = append(parts, fmt.Sprintf("当前项目根目录: %s", escapeDynamic(req.ProjectRoot)))
	}
	parts = append(parts, modeInstruction(req.Mode, iteration))
	parts = append(parts, "</system-reminder>")
	return append(blocks, Block{Name: "runtime-system-reminder", Content: strings.Join(parts, "\n"), Stable: false})
}

func modeInstruction(mode RunMode, iteration int) string {
	scope := "当前模式只对本次用户请求生效；同一请求内所有 Agent Loop 轮次都必须遵守。"
	switch mode {
	case RunModePlan:
		return iterationInstruction(iteration,
			"当前请求处于 Plan Mode。只能观察、读取、搜索和制定计划；不得写文件、改文件或执行命令。"+scope,
			"Plan Mode 关键约束：保持只读，不执行 Write、Edit、Bash。"+scope,
			"Plan Mode：继续保持只读约束。")
	case RunModeDo:
		return iterationInstruction(iteration,
			"当前请求处于 Do Mode。可以按计划使用允许的工具执行任务，但仍必须先观察、谨慎修改并验证结果。"+scope,
			"Do Mode 关键约束：按计划执行，修改前先读，执行后验证。"+scope,
			"Do Mode：继续按计划推进并验证。")
	default:
		return iterationInstruction(iteration,
			"当前请求处于默认模式。可以根据任务需要使用工具观察、分析、修改和验证，直到任务完成或遇到停止条件。"+scope,
			"默认模式关键约束：优先专用工具，修改前先读，执行后验证。"+scope,
			"默认模式：继续使用最小必要步骤推进。")
	}
}

func iterationInstruction(iteration int, full string, repeated string, compact string) string {
	if iteration <= 1 {
		return full
	}
	if iteration%3 == 0 {
		return repeated
	}
	return compact
}

func escapeDynamic(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"`", "｀",
		"\x1b", "",
		"‪", "",
		"‫", "",
		"‬", "",
		"‭", "",
		"‮", "",
		"⁦", "",
		"⁧", "",
		"⁨", "",
		"⁩", "",
	)
	return replacer.Replace(value)
}
