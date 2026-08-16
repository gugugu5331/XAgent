package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// ConfirmationPanel renders a capability-free security confirmation snapshot.
// It deliberately does not infer authorization policy: the security model is
// the sole authority for both the displayed scope and permanent eligibility.
type ConfirmationPanel struct {
	confirmation ConfirmationView
	region       Region
	regionSet    bool
}

func NewConfirmationPanel(confirmation ConfirmationView) ConfirmationPanel {
	return ConfirmationPanel{confirmation: confirmation}
}

// NewTaskConfirmationPanel renders a task-local decision. Permanent grants
// are impossible for child tasks and are removed here as a final UI boundary,
// independently of producer correctness.
func NewTaskConfirmationPanel(confirmation ConfirmationView) ConfirmationPanel {
	confirmation.allowPermanent = false
	confirmation.scopes = cloneSlice(confirmation.scopes)
	for index := range confirmation.scopes {
		if strings.EqualFold(strings.TrimSpace(confirmation.scopes[index].scope), "permanent") {
			confirmation.scopes[index].available = false
		}
	}
	return ConfirmationPanel{confirmation: confirmation}
}

func (panel *ConfirmationPanel) SetRegion(region Region) {
	panel.region = Region{
		X: nonNegative(region.X), Y: nonNegative(region.Y),
		Width: nonNegative(region.Width), Height: nonNegative(region.Height),
	}
	panel.regionSet = true
}

func (panel ConfirmationPanel) View() string {
	if !panel.regionSet || panel.region.Width == 0 || panel.region.Height == 0 {
		return ""
	}

	lines := panel.lines()
	if len(lines) > panel.region.Height {
		lines = prioritizedConfirmationLines(lines, panel.region.Height)
	}
	for index := range lines {
		lines[index] = ansi.Truncate(safeConfirmationLine(lines[index]), panel.region.Width, "")
	}
	return strings.Join(lines, "\n")
}

func (panel ConfirmationPanel) lines() []string {
	view := panel.confirmation
	actions := "操作:y本次 s会话 n拒绝 Esc取消"
	if view.AllowPermanent() {
		actions = "操作:y本次 s会话 p永久 n拒绝 Esc取消"
	}
	return []string{
		"权限确认 | 工具:" + strings.TrimSpace(view.Name()) + " | 风险:" + strings.TrimSpace(view.Risk()) +
			" | 模式:" + strings.TrimSpace(view.PermissionMode()) + " | 警告:" + view.Warning().Text(),
		"实际目标:" + view.Target().Text() + " | 范围:" + view.ScopePreview().Text(),
		"授权范围:" + confirmationScopeSummary(view) + " | 规则落点:" + view.RuleLocation().Text() + " | 撤销:" + view.RevokeHint().Text(),
		actions,
	}
}

func confirmationScopeSummary(view ConfirmationView) string {
	scopes := view.Scopes()
	if len(scopes) == 0 {
		return "未提供"
	}
	parts := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		availability := "不可用"
		if scope.Available() {
			availability = "可用"
		}
		parts = append(parts, scope.Scope()+"("+availability+")")
	}
	return strings.Join(parts, ",")
}

// The action row is always retained so even a severely constrained terminal
// exposes denial and cancellation. Remaining rows retain target, then risk,
// then rule/revocation detail in that order.
func prioritizedConfirmationLines(lines []string, height int) []string {
	if height <= 0 || len(lines) == 0 {
		return nil
	}
	if height >= len(lines) {
		return lines
	}
	if height == 1 {
		return []string{lines[len(lines)-1]}
	}
	visible := append([]string(nil), lines[:height-1]...)
	return append(visible, lines[len(lines)-1])
}

func safeConfirmationLine(value string) string {
	value = ansi.Strip(value)
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < ' ' || r >= '\x7f' && r <= '\x9f' {
			return -1
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}
