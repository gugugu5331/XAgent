package resources

type Provider struct {
	labels map[string]string
}

func New() *Provider {
	return &Provider{labels: map[string]string{
		"app_title":        "XAgent",
		"new_conversation": "新建会话",
		"history_title":    "历史会话",
		"input_prompt":     "输入问题，Enter 提交",
		"empty_input":      "请输入非空内容",
		"streaming":        "正在响应...",
		"error":            "错误",
		"duration":         "耗时",
		"quit_hint":        "按 q 或 ctrl+c 退出",
	}}
}

func (p *Provider) SystemPrompt() string {
	return "你是 XAgent，一个终端 AI 编程助手。回答应使用中文，简洁、直接、可执行；需要操作项目时优先使用专用工具，并遵守安全边界。"
}

func (p *Provider) UILabel(key string) string {
	if value, ok := p.labels[key]; ok {
		return value
	}
	return key
}
