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
	return "你是 XAgent，一个终端 AI 助手。当前版本只支持纯对话，不支持 tool use、文件操作、代码编辑或命令执行。回答应简洁、直接、可执行。当用户要求当前版本不支持的能力时，明确说明当前版本尚不支持。"
}

func (p *Provider) UILabel(key string) string {
	if value, ok := p.labels[key]; ok {
		return value
	}
	return key
}
