package tui

import (
	"fmt"
	"strings"
	"time"

	"xagent/internal/redact"
)

type TaskEventViewSpec struct {
	Revision    uint64
	Sequence    uint64
	AtUnixMilli int64
	Kind        string
	Status      string
	Placement   string
	ToolName    string
	Text        redact.SafeText
	Error       SafeErrorViewSpec
}

type TaskDetailViewSpec struct {
	Watermark    uint64
	Task         TaskViewSpec
	RecentEvents []TaskEventViewSpec
}

type TaskEventView struct {
	revision    uint64
	sequence    uint64
	atUnixMilli int64
	kind        string
	status      string
	placement   string
	toolName    string
	text        redact.SafeText
	error       SafeErrorView
	hasError    bool
}

type TaskDetailView struct {
	watermark    uint64
	task         TaskView
	recentEvents []TaskEventView
}

func NewTaskDetailView(spec TaskDetailViewSpec) TaskDetailView {
	view := TaskDetailView{watermark: spec.Watermark, task: projectTaskView(spec.Task)}
	if spec.RecentEvents != nil {
		view.recentEvents = make([]TaskEventView, len(spec.RecentEvents))
		for index, event := range spec.RecentEvents {
			view.recentEvents[index] = TaskEventView{
				revision: event.Revision, sequence: event.Sequence, atUnixMilli: event.AtUnixMilli,
				kind: event.Kind, status: event.Status, placement: event.Placement, toolName: event.ToolName, text: event.Text,
				error:    SafeErrorView{code: event.Error.Code, source: event.Error.Source, message: event.Error.Message, recoverable: event.Error.Recoverable},
				hasError: event.Error.Present,
			}
		}
	}
	return view
}

func (view TaskDetailView) clone() TaskDetailView {
	cloned := view
	cloned.task = view.task.clone()
	cloned.recentEvents = cloneSlice(view.recentEvents)
	return cloned
}

func (view TaskDetailView) Watermark() uint64 { return view.watermark }
func (view TaskDetailView) Task() TaskView    { return view.task.clone() }
func (view TaskDetailView) RecentEvents() []TaskEventView {
	return cloneSlice(view.recentEvents)
}

func (view TaskEventView) Revision() uint64             { return view.revision }
func (view TaskEventView) Sequence() uint64             { return view.sequence }
func (view TaskEventView) AtUnixMilli() int64           { return view.atUnixMilli }
func (view TaskEventView) Kind() string                 { return view.kind }
func (view TaskEventView) Status() string               { return view.status }
func (view TaskEventView) Placement() string            { return view.placement }
func (view TaskEventView) ToolName() string             { return view.toolName }
func (view TaskEventView) Text() redact.SafeText        { return view.text }
func (view TaskEventView) Error() (SafeErrorView, bool) { return view.error, view.hasError }

type TaskDetail struct {
	view      TaskDetailView
	region    Region
	regionSet bool
}

func NewTaskDetail(view TaskDetailView) TaskDetail { return TaskDetail{view: view.clone()} }

func (screen *TaskDetail) SetRegion(region Region) {
	screen.region = Region{X: nonNegative(region.X), Y: nonNegative(region.Y), Width: nonNegative(region.Width), Height: nonNegative(region.Height)}
	screen.regionSet = true
}

func (screen TaskDetail) View() string {
	task := screen.view.task
	lines := []string{
		fmt.Sprintf("任务详情 | watermark=%d | id=%s | revision=%d", screen.view.watermark, task.id, task.revision),
		fmt.Sprintf("status=%s placement=%s type=%s origin=%s iteration=%d/%d", task.status, task.placement, task.executionType, task.origin, task.iteration, task.maxIterations),
		fmt.Sprintf("role=%s source=%s source_id=%s provider=%s generation=%d origin=%s", task.role, task.roleSource, task.roleSourceID, task.roleProviderID, task.roleGeneration, task.roleOrigin.Text()),
		fmt.Sprintf("created=%s started=%s ended=%s", formatTaskTime(task.createdAtUnixMilli, true), formatTaskTime(task.startedAtUnixMilli, task.hasStartedAt), formatTaskTime(task.endedAtUnixMilli, task.hasEndedAt)),
		fmt.Sprintf("usage=%d in / %d out | cache=%d create / %d read | dropped=%d", task.usage.inputTokens, task.usage.outputTokens, task.usage.cacheCreationInputTokens, task.usage.cacheReadInputTokens, task.eventsDropped),
	}
	if task.stopReason != "" {
		lines = append(lines, "stop="+task.stopReason)
	}
	if task.summary.Text() != "" || task.summaryTruncated {
		lines = append(lines, fmt.Sprintf("summary=%s truncated=%t reason=%s", task.summary.Text(), task.summaryTruncated, task.truncationReason.Text()))
	}
	if task.hasError {
		lines = append(lines, fmt.Sprintf("error=%s source=%s recoverable=%t message=%s", task.error.code, task.error.source, task.error.recoverable, task.error.message.Text()))
	}
	if task.hasConfirmation {
		confirmation := task.pendingConfirmation
		lines = append(lines, fmt.Sprintf("等待确认 | confirmation=%s call=%s tool=%s risk=%s", confirmation.confirmationID, confirmation.callID, confirmation.name, confirmation.risk))
		panel := NewTaskConfirmationPanel(confirmation)
		panel.SetRegion(Region{Width: 4096, Height: 4})
		if value := panel.View(); value != "" {
			lines = append(lines, strings.Split(value, "\n")...)
		}
	}
	if len(screen.view.recentEvents) > 0 {
		lines = append(lines, "最近轨迹")
		for _, event := range screen.view.recentEvents {
			parts := []string{fmt.Sprintf("revision=%d", event.revision), fmt.Sprintf("sequence=%d", event.sequence), "kind=" + event.kind}
			if event.atUnixMilli != 0 {
				parts = append(parts, "at="+formatTaskTime(event.atUnixMilli, true))
			}
			if event.status != "" {
				parts = append(parts, "status="+event.status)
			}
			if event.placement != "" {
				parts = append(parts, "placement="+event.placement)
			}
			if event.toolName != "" {
				parts = append(parts, "tool="+event.toolName)
			}
			if event.text.Text() != "" {
				parts = append(parts, "detail="+event.text.Text())
			}
			if event.hasError {
				parts = append(parts, "error="+event.error.code+":"+event.error.message.Text())
			}
			lines = append(lines, strings.Join(parts, " | "))
		}
	}
	return renderTaskLines(lines, screen.region, screen.regionSet)
}

func formatTaskTime(unixMilli int64, present bool) string {
	if !present || unixMilli == 0 {
		return "-"
	}
	return time.UnixMilli(unixMilli).UTC().Format(time.RFC3339Nano)
}
