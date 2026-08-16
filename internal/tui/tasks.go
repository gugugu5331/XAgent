package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"xagent/internal/redact"
)

// TaskUsageViewSpec is a capability-free token accounting projection.
type TaskUsageViewSpec struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// TaskViewSpec contains only detached display values. Parent routing,
// invocation identity, runners, brokers and services are intentionally absent.
type TaskViewSpec struct {
	ID             string
	Revision       uint64
	Type           string
	Origin         string
	Role           string
	RoleSource     string
	RoleSourceID   string
	RoleProviderID string
	RoleOrigin     redact.SafeText
	RoleGeneration uint64
	Placement      string
	Status         string

	CreatedAtUnixMilli int64
	HasStartedAt       bool
	StartedAtUnixMilli int64
	HasEndedAt         bool
	EndedAtUnixMilli   int64

	Iteration        int
	MaxIterations    int
	StopReason       string
	Summary          redact.SafeText
	SummaryTruncated bool
	TruncationReason redact.SafeText
	Error            SafeErrorViewSpec
	Usage            TaskUsageViewSpec
	EventsDropped    uint64

	PendingConfirmation ConfirmationViewSpec
}

type TaskListViewSpec struct {
	Watermark uint64
	Tasks     []TaskViewSpec
	Notice    redact.SafeText
}

// TaskNotificationViewSpec is the bounded, persistence-compatible projection
// displayed in the active main conversation. It has no parent or trace data.
type TaskNotificationViewSpec struct {
	NotificationID string
	TaskID         string
	Status         string
	Summary        redact.SafeText
	StopReason     string
	Error          SafeErrorViewSpec
}

type TaskNotification struct {
	notificationID string
	taskID         string
	status         string
	summary        redact.SafeText
	stopReason     string
	error          SafeErrorView
	hasError       bool
}

func NewTaskNotification(spec TaskNotificationViewSpec) TaskNotification {
	return TaskNotification{
		notificationID: spec.NotificationID, taskID: spec.TaskID, status: spec.Status,
		summary: spec.Summary, stopReason: spec.StopReason,
		error:    SafeErrorView{code: spec.Error.Code, source: spec.Error.Source, message: spec.Error.Message, recoverable: spec.Error.Recoverable},
		hasError: spec.Error.Present,
	}
}

func (notification TaskNotification) View() string {
	parts := []string{
		"子任务通知", "notification=" + notification.notificationID, "id=" + notification.taskID,
		"status=" + notification.status, "stop=" + notification.stopReason,
	}
	if notification.summary.Text() != "" {
		parts = append(parts, "summary="+notification.summary.Text())
	}
	if notification.hasError {
		parts = append(parts, "error="+notification.error.code+":"+notification.error.message.Text())
	}
	return safeTaskLine(strings.Join(parts, " | "))
}

type TaskUsageView struct {
	inputTokens              int64
	outputTokens             int64
	cacheCreationInputTokens int64
	cacheReadInputTokens     int64
}

type TaskView struct {
	id             string
	revision       uint64
	executionType  string
	origin         string
	role           string
	roleSource     string
	roleSourceID   string
	roleProviderID string
	roleOrigin     redact.SafeText
	roleGeneration uint64
	placement      string
	status         string

	createdAtUnixMilli int64
	hasStartedAt       bool
	startedAtUnixMilli int64
	hasEndedAt         bool
	endedAtUnixMilli   int64

	iteration        int
	maxIterations    int
	stopReason       string
	summary          redact.SafeText
	summaryTruncated bool
	truncationReason redact.SafeText
	error            SafeErrorView
	hasError         bool
	usage            TaskUsageView
	eventsDropped    uint64

	pendingConfirmation ConfirmationView
	hasConfirmation     bool
}

type TaskListView struct {
	watermark uint64
	tasks     []TaskView
	notice    redact.SafeText
}

// NewTaskListView owns a deep, capability-free copy of the supplied spec.
func NewTaskListView(spec TaskListViewSpec) TaskListView {
	view := TaskListView{watermark: spec.Watermark, notice: spec.Notice}
	if spec.Tasks != nil {
		view.tasks = make([]TaskView, len(spec.Tasks))
		for index := range spec.Tasks {
			view.tasks[index] = projectTaskView(spec.Tasks[index])
		}
	}
	return view
}

func projectTaskView(spec TaskViewSpec) TaskView {
	confirmation := projectConfirmationView(spec.PendingConfirmation)
	if spec.PendingConfirmation.Present {
		// A child task can never create permanent authorization. The TUI keeps
		// this fail-closed even if a malformed producer advertises otherwise.
		confirmation.allowPermanent = false
		for index := range confirmation.scopes {
			if strings.EqualFold(strings.TrimSpace(confirmation.scopes[index].scope), "permanent") {
				confirmation.scopes[index].available = false
			}
		}
	}
	return TaskView{
		id: spec.ID, revision: spec.Revision, executionType: spec.Type, origin: spec.Origin,
		role: spec.Role, roleSource: spec.RoleSource, roleSourceID: spec.RoleSourceID,
		roleProviderID: spec.RoleProviderID, roleOrigin: spec.RoleOrigin, roleGeneration: spec.RoleGeneration,
		placement: spec.Placement, status: spec.Status,
		createdAtUnixMilli: spec.CreatedAtUnixMilli, hasStartedAt: spec.HasStartedAt,
		startedAtUnixMilli: spec.StartedAtUnixMilli, hasEndedAt: spec.HasEndedAt, endedAtUnixMilli: spec.EndedAtUnixMilli,
		iteration: spec.Iteration, maxIterations: spec.MaxIterations, stopReason: spec.StopReason,
		summary: spec.Summary, summaryTruncated: spec.SummaryTruncated, truncationReason: spec.TruncationReason,
		error:    SafeErrorView{code: spec.Error.Code, source: spec.Error.Source, message: spec.Error.Message, recoverable: spec.Error.Recoverable},
		hasError: spec.Error.Present,
		usage: TaskUsageView{
			inputTokens: spec.Usage.InputTokens, outputTokens: spec.Usage.OutputTokens,
			cacheCreationInputTokens: spec.Usage.CacheCreationInputTokens, cacheReadInputTokens: spec.Usage.CacheReadInputTokens,
		},
		eventsDropped:       spec.EventsDropped,
		pendingConfirmation: confirmation, hasConfirmation: spec.PendingConfirmation.Present,
	}
}

func projectConfirmationView(spec ConfirmationViewSpec) ConfirmationView {
	return ConfirmationView{
		confirmationID: spec.ConfirmationID, callID: spec.CallID, name: spec.Name,
		prompt: spec.Prompt, target: spec.Target, risk: spec.Risk, permissionMode: spec.PermissionMode,
		scopePreview: spec.ScopePreview, ruleLocation: spec.RuleLocation,
		scopes: projectConfirmationScopes(spec.Scopes), warning: spec.Warning, revokeHint: spec.RevokeHint,
		allowPermanent: spec.AllowPermanent,
	}
}

func (view TaskListView) clone() TaskListView {
	cloned := view
	if view.tasks != nil {
		cloned.tasks = make([]TaskView, len(view.tasks))
		for index := range view.tasks {
			cloned.tasks[index] = view.tasks[index].clone()
		}
	}
	return cloned
}

func (view TaskView) clone() TaskView {
	cloned := view
	cloned.pendingConfirmation.scopes = cloneSlice(view.pendingConfirmation.scopes)
	return cloned
}

func (view TaskListView) Watermark() uint64       { return view.watermark }
func (view TaskListView) Notice() redact.SafeText { return view.notice }
func (view TaskListView) Tasks() []TaskView {
	if view.tasks == nil {
		return nil
	}
	result := make([]TaskView, len(view.tasks))
	for index := range view.tasks {
		result[index] = view.tasks[index].clone()
	}
	return result
}

func (view TaskView) ID() string                  { return view.id }
func (view TaskView) Revision() uint64            { return view.revision }
func (view TaskView) Type() string                { return view.executionType }
func (view TaskView) Origin() string              { return view.origin }
func (view TaskView) Role() string                { return view.role }
func (view TaskView) RoleSource() string          { return view.roleSource }
func (view TaskView) RoleSourceID() string        { return view.roleSourceID }
func (view TaskView) RoleProviderID() string      { return view.roleProviderID }
func (view TaskView) RoleOrigin() redact.SafeText { return view.roleOrigin }
func (view TaskView) RoleGeneration() uint64      { return view.roleGeneration }
func (view TaskView) Placement() string           { return view.placement }
func (view TaskView) Status() string              { return view.status }
func (view TaskView) CreatedAtUnixMilli() int64   { return view.createdAtUnixMilli }
func (view TaskView) StartedAtUnixMilli() (int64, bool) {
	return view.startedAtUnixMilli, view.hasStartedAt
}
func (view TaskView) EndedAtUnixMilli() (int64, bool)   { return view.endedAtUnixMilli, view.hasEndedAt }
func (view TaskView) Iteration() int                    { return view.iteration }
func (view TaskView) MaxIterations() int                { return view.maxIterations }
func (view TaskView) StopReason() string                { return view.stopReason }
func (view TaskView) Summary() redact.SafeText          { return view.summary }
func (view TaskView) SummaryTruncated() bool            { return view.summaryTruncated }
func (view TaskView) TruncationReason() redact.SafeText { return view.truncationReason }
func (view TaskView) Usage() TaskUsageView              { return view.usage }
func (view TaskView) EventsDropped() uint64             { return view.eventsDropped }
func (view TaskView) Error() (SafeErrorView, bool)      { return view.error, view.hasError }
func (view TaskView) PendingConfirmation() (ConfirmationView, bool) {
	confirmation := view.pendingConfirmation
	confirmation.scopes = cloneSlice(view.pendingConfirmation.scopes)
	return confirmation, view.hasConfirmation
}

func (view TaskUsageView) InputTokens() int64              { return view.inputTokens }
func (view TaskUsageView) OutputTokens() int64             { return view.outputTokens }
func (view TaskUsageView) CacheCreationInputTokens() int64 { return view.cacheCreationInputTokens }
func (view TaskUsageView) CacheReadInputTokens() int64     { return view.cacheReadInputTokens }

// TaskList is the independent tasks screen renderer.
type TaskList struct {
	view      TaskListView
	region    Region
	regionSet bool
}

func NewTaskList(view TaskListView) TaskList { return TaskList{view: view.clone()} }

func (screen *TaskList) SetRegion(region Region) {
	screen.region = Region{X: nonNegative(region.X), Y: nonNegative(region.Y), Width: nonNegative(region.Width), Height: nonNegative(region.Height)}
	screen.regionSet = true
}

func (screen TaskList) View() string {
	lines := []string{fmt.Sprintf("任务列表 | watermark=%d | count=%d", screen.view.watermark, len(screen.view.tasks))}
	for _, task := range screen.view.tasks {
		lines = append(lines, fmt.Sprintf(
			"id=%s status=%s placement=%s type=%s origin=%s role=%s source=%s source_id=%s provider=%s generation=%d iteration=%d/%d",
			task.id, task.status, task.placement, task.executionType, task.origin, task.role, task.roleSource,
			task.roleSourceID, task.roleProviderID, task.roleGeneration, task.iteration, task.maxIterations,
		))
	}
	if notice := strings.TrimSpace(screen.view.notice.Text()); notice != "" {
		lines = append(lines, "notice="+notice)
	}
	return renderTaskLines(lines, screen.region, screen.regionSet)
}

// TaskIntent is emitted by task screens and contains user values only.
type TaskIntentKind string

const (
	TaskIntentShowList            TaskIntentKind = "show_list"
	TaskIntentShowDetail          TaskIntentKind = "show_detail"
	TaskIntentCancel              TaskIntentKind = "cancel"
	TaskIntentMoveToBackground    TaskIntentKind = "move_to_background"
	TaskIntentResolveConfirmation TaskIntentKind = "resolve_confirmation"
)

type TaskIntent struct {
	kind           TaskIntentKind
	taskID         string
	confirmationID string
	callID         string
	action         string
}

func NewTaskIntent(kind TaskIntentKind, taskID, confirmationID, callID, action string) TaskIntent {
	return TaskIntent{kind: kind, taskID: taskID, confirmationID: confirmationID, callID: callID, action: action}
}

func (intent TaskIntent) Kind() TaskIntentKind   { return intent.kind }
func (intent TaskIntent) TaskID() string         { return intent.taskID }
func (intent TaskIntent) ConfirmationID() string { return intent.confirmationID }
func (intent TaskIntent) CallID() string         { return intent.callID }
func (intent TaskIntent) Action() string         { return intent.action }

func renderTaskLines(lines []string, region Region, regionSet bool) string {
	if regionSet {
		if region.Width == 0 || region.Height == 0 {
			return ""
		}
		if len(lines) > region.Height {
			lines = lines[:region.Height]
		}
	}
	result := make([]string, len(lines))
	for index := range lines {
		line := safeTaskLine(lines[index])
		if regionSet {
			line = ansi.Truncate(line, region.Width, "")
		}
		result[index] = line
	}
	return strings.Join(result, "\n")
}

func safeTaskLine(value string) string {
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
