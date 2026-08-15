package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"xagent/internal/redact"
)

func TestConfirmationShowsCancellableAuthorizationScope(t *testing.T) {
	t.Run("safe permanent scope shows exact rule and revocation", func(t *testing.T) {
		view := confirmationTestView(t, ConfirmationViewSpec{
			Present: true, Name: "Bash", Prompt: confirmationSafeText("run tests"),
			Target: confirmationSafeText("Bash(command=go test ./...)"), Risk: "high", PermissionMode: "default",
			Scopes: []ConfirmationScopeViewSpec{
				{Scope: "once", Available: true, Description: confirmationSafeText("this call only")},
				{Scope: "session", Available: true, Description: confirmationSafeText("until session ends")},
				{Scope: "permanent", Available: true, Description: confirmationSafeText("saved exact rule")},
			},
			ScopePreview: confirmationSafeText("Bash exact: go test ./..."),
			RuleLocation: confirmationSafeText(".xagent/permissions.local.yaml"),
			Warning:      confirmationSafeText("Bash is not sandboxed"),
			RevokeHint:   confirmationSafeText("remove rule"), AllowPermanent: true,
		})
		panel := NewConfirmationPanel(view)
		panel.SetRegion(Region{Width: 120, Height: 4})

		output := panel.View()
		for _, want := range []string{
			"权限确认", "工具:Bash", "风险:high", "模式:default", "警告:Bash is not sandboxed",
			"实际目标:Bash(command=go test ./...)", "once(可用)", "session(可用)", "permanent(可用)",
			"范围:Bash exact: go test ./...", "规则落点:.xagent/permissions.local.yaml", "撤销:remove rule",
			"y本次", "s会话", "p永久", "n拒绝", "Esc取消",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("confirmation output missing %q: %q", want, output)
			}
		}
		assertConfirmationBounds(t, output, Region{Width: 120, Height: 4})
	})

	t.Run("credential or degraded scope never offers permanent action", func(t *testing.T) {
		runtimeRedactor := redact.NewRuntimeRedactor()
		secret := "sk-ant-confirmation-secret"
		view := confirmationTestView(t, ConfirmationViewSpec{
			Present: true, Name: "Bash", Prompt: runtimeRedactor.Redact("target " + secret),
			Target: runtimeRedactor.Redact("Bash(command=export TOKEN=" + secret + ")"),
			Risk:   "high", PermissionMode: "degraded",
			Scopes: []ConfirmationScopeViewSpec{
				{Scope: "once", Available: true, Description: confirmationSafeText("this call only")},
				{Scope: "session", Available: true, Description: confirmationSafeText("until session ends")},
				{Scope: "permanent", Available: false, Description: confirmationSafeText("unavailable")},
			},
			ScopePreview: confirmationSafeText("Bash exact redacted target"),
			Warning:      confirmationSafeText("权限配置已降级"),
			RevokeHint:   confirmationSafeText("本次调用不支持永久授权"), AllowPermanent: false,
		})
		panel := NewConfirmationPanel(view)
		panel.SetRegion(Region{Width: 120, Height: 4})

		output := panel.View()
		if strings.Contains(output, secret) || strings.Contains(output, "p永久") || strings.Contains(output, "permanent(可用)") {
			t.Fatalf("unsafe confirmation exposed a secret or permanent action: %q", output)
		}
		for _, want := range []string{"模式:degraded", "权限配置已降级", "permanent(不可用)", "不支持永久授权", "y本次", "s会话", "n拒绝", "Esc取消"} {
			if !strings.Contains(output, want) {
				t.Fatalf("restricted confirmation output missing %q: %q", want, output)
			}
		}
	})

	t.Run("terminal controls are neutralized and compact actions remain visible", func(t *testing.T) {
		view := confirmationTestView(t, ConfirmationViewSpec{
			Present: true, Name: "Bash\x1b[31m", Risk: "high", PermissionMode: "default",
			Target:       confirmationSafeText("target\x1b]52;c;clipboard\a\rrewritten\b"),
			ScopePreview: confirmationSafeText("scope\x1b[2J\nnext"),
			Warning:      confirmationSafeText("warning\x00\x1b[H"),
			RevokeHint:   confirmationSafeText("revoke\x7f\tcarefully"),
			Scopes: []ConfirmationScopeViewSpec{
				{Scope: "once", Available: true}, {Scope: "session", Available: true}, {Scope: "permanent", Available: false},
			},
		})
		panel := NewConfirmationPanel(view)
		region := Region{Width: 40, Height: 4}
		panel.SetRegion(region)

		output := panel.View()
		if strings.ContainsAny(output, "\x1b\a\r\b\x00\x7f") || strings.Contains(output, "52;c;clipboard") {
			t.Fatalf("terminal control sequence reached confirmation output: %q", output)
		}
		for _, want := range []string{"y本次", "s会话", "n拒绝", "Esc取消"} {
			if !strings.Contains(output, want) {
				t.Fatalf("compact confirmation hid action %q: %q", want, output)
			}
		}
		assertConfirmationBounds(t, output, region)

		panel.SetRegion(Region{X: -1, Y: -1, Width: -1, Height: -1})
		if output := panel.View(); output != "" {
			t.Fatalf("zero-sized confirmation remained visible: %q", output)
		}
	})

	t.Run("panel never reinterprets prompt or invents a rule location", func(t *testing.T) {
		view := confirmationTestView(t, ConfirmationViewSpec{
			Present: true, Name: "Read", Prompt: confirmationSafeText("prompt-must-not-be-parsed"),
			Risk: "medium", PermissionMode: "default",
			Scopes:     []ConfirmationScopeViewSpec{{Scope: "once", Available: true}},
			RevokeHint: confirmationSafeText("expires after this call"),
		})
		panel := NewConfirmationPanel(view)
		panel.SetRegion(Region{Width: 120, Height: 4})

		output := panel.View()
		if strings.Contains(output, "prompt-must-not-be-parsed") || strings.Contains(output, "规则落点:不适用") {
			t.Fatalf("confirmation reinterpreted a prompt or invented model data: %q", output)
		}
		if !strings.Contains(output, "实际目标:") || !strings.Contains(output, "规则落点:") {
			t.Fatalf("confirmation lost empty-model labels: %q", output)
		}
	})
}

func TestTaskConfirmationAlwaysRejectsPermanentAuthorizationUI(t *testing.T) {
	view := confirmationTestView(t, ConfirmationViewSpec{
		Present: true, ConfirmationID: "confirmation-task", CallID: "call-task", Name: "Bash",
		Scopes: []ConfirmationScopeViewSpec{
			{Scope: "once", Available: true},
			{Scope: "session", Available: true},
			{Scope: "permanent", Available: true},
		},
		AllowPermanent: true,
	})
	panel := NewTaskConfirmationPanel(view)
	panel.SetRegion(Region{Width: 120, Height: 4})
	output := panel.View()
	if strings.Contains(output, "p永久") || strings.Contains(output, "permanent(可用)") {
		t.Fatalf("task confirmation offered permanent authorization: %q", output)
	}
	for _, want := range []string{"y本次", "s会话", "n拒绝", "Esc取消", "permanent(不可用)"} {
		if !strings.Contains(output, want) {
			t.Fatalf("task confirmation missing %q: %q", want, output)
		}
	}
}

func confirmationTestView(t *testing.T, spec ConfirmationViewSpec) ConfirmationView {
	t.Helper()
	view := NewStateViewModel(ViewModelSpec{Request: RequestViewSpec{Confirmation: spec}})
	confirmation, present := view.Request().Confirmation()
	if !present {
		t.Fatal("confirmation fixture was not projected")
	}
	return confirmation
}

func confirmationSafeText(value string) redact.SafeText {
	return redact.NewRuntimeRedactor().Redact(value)
}

func assertConfirmationBounds(t *testing.T, output string, region Region) {
	t.Helper()
	if width, height := lipgloss.Width(output), lipgloss.Height(output); width > region.Width || height > region.Height {
		t.Fatalf("confirmation size %dx%d exceeds region %#v: %q", width, height, region, output)
	}
}
