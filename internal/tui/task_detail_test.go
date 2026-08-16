package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"xagent/internal/redact"
)

func TestTaskDetailShowsLifecycleUsageFailureConfirmationAndTrace(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	spec := TaskDetailViewSpec{
		Watermark: 41,
		Task: TaskViewSpec{
			ID: "task-9", Revision: 40, Type: "fork", Origin: "model", Role: "review",
			RoleSource: "builtin", RoleSourceID: "review", RoleProviderID: "provider-b",
			RoleOrigin: redactor.Redact("builtin role"), RoleGeneration: 2,
			Placement: "foreground", Status: "failed", CreatedAtUnixMilli: 1000,
			HasStartedAt: true, StartedAtUnixMilli: 2000, HasEndedAt: true, EndedAtUnixMilli: 3000,
			Iteration: 3, MaxIterations: 3, StopReason: "tool_error",
			Summary: redactor.Redact("safe summary"), SummaryTruncated: true,
			TruncationReason: redactor.Redact("max_summary_bytes"),
			Error:            SafeErrorViewSpec{Present: true, Code: "tool_failed", Source: "subagent", Message: redactor.Redact("safe failure"), Recoverable: false},
			Usage:            TaskUsageViewSpec{InputTokens: 5, OutputTokens: 8, CacheCreationInputTokens: 13, CacheReadInputTokens: 21},
			EventsDropped:    6,
			PendingConfirmation: ConfirmationViewSpec{
				Present: true, ConfirmationID: "confirmation-9", CallID: "call-9", Name: "Bash", Risk: "high",
				Target: redactor.Redact("go test ./..."), AllowPermanent: true,
				Scopes: []ConfirmationScopeViewSpec{{Scope: "permanent", Available: true}, {Scope: "once", Available: true}},
			},
		},
		RecentEvents: []TaskEventViewSpec{
			{Revision: 39, Sequence: 7, AtUnixMilli: 2500, Kind: "tool", ToolName: "Bash", Text: redactor.Redact("go test finished")},
			{Revision: 40, Sequence: 8, AtUnixMilli: 3000, Kind: "failed", Status: "failed", Text: redactor.Redact("safe failure")},
		},
	}
	viewModel := NewStateViewModel(ViewModelSpec{Screen: ScreenTaskDetail, TaskDetail: spec})

	// Mutating every producer-owned nested slice must not affect the view.
	spec.Task.PendingConfirmation.Scopes[0] = ConfirmationScopeViewSpec{Scope: "mutated"}
	spec.RecentEvents[0] = TaskEventViewSpec{Kind: "mutated"}
	detail := viewModel.TaskDetail()
	events := detail.RecentEvents()
	if detail.Watermark() != 41 || detail.Task().ID() != "task-9" || len(events) != 2 || events[0].Kind() != "tool" || events[0].ToolName() != "Bash" {
		t.Fatalf("task detail did not detach source: %#v %#v", detail, events)
	}
	events[0] = TaskEventView{}
	if viewModel.TaskDetail().RecentEvents()[0].Kind() != "tool" {
		t.Fatal("task detail getter exposed mutable event storage")
	}

	screen := NewTaskDetail(detail)
	output := screen.View()
	for _, want := range []string{
		"任务详情", "watermark=41", "task-9", "failed", "foreground", "review", "builtin", "provider-b",
		"created=", "started=", "ended=", "3/3", "5 in / 8 out", "13 create / 21 read",
		"tool_error", "safe summary", "max_summary_bytes", "tool_failed", "safe failure", "dropped=6",
		"confirmation-9", "call-9", "Bash", "最近轨迹", "revision=39", "sequence=7", "go test finished",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("task detail missing %q: %q", want, output)
		}
	}
	if strings.Contains(output, "p永久") || strings.Contains(output, "permanent(可用)") {
		t.Fatalf("subagent detail exposed permanent authorization: %q", output)
	}
}

func TestTaskScreensNeutralizeTerminalControlsAndRespectRegion(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	detail := NewStateViewModel(ViewModelSpec{TaskDetail: TaskDetailViewSpec{
		Task:         TaskViewSpec{ID: "task\x1b[31m", Status: "running", Summary: redactor.Redact("safe\x1b]52;c;clipboard\a\nnext")},
		RecentEvents: []TaskEventViewSpec{{Revision: 1, Sequence: 1, Kind: "progress\rrewritten", Text: redactor.Redact("trace\x1b[2J")}},
	}}).TaskDetail()
	screen := NewTaskDetail(detail)
	region := Region{Width: 44, Height: 5}
	screen.SetRegion(region)
	output := screen.View()
	if strings.ContainsAny(output, "\x1b\a\r") || strings.Contains(output, "52;c;clipboard") {
		t.Fatalf("terminal control reached task detail: %q", output)
	}
	if width, height := lipgloss.Width(output), lipgloss.Height(output); width > region.Width || height > region.Height {
		t.Fatalf("task detail %dx%d exceeds region %#v: %q", width, height, region, output)
	}
}
