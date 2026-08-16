package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xagent/internal/command"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tui"
)

type taskNotificationStoreFake struct {
	saves []*conversation.Conversation
	err   error
}

func (*taskNotificationStoreFake) Create(context.Context) (*conversation.Conversation, error) {
	return nil, errors.New("unexpected Create")
}
func (*taskNotificationStoreFake) List(context.Context) (conversation.ListResult, error) {
	return conversation.ListResult{}, errors.New("unexpected List")
}
func (*taskNotificationStoreFake) Load(context.Context, string) (conversation.LoadResult, error) {
	return conversation.LoadResult{}, errors.New("unexpected Load")
}
func (fake *taskNotificationStoreFake) Save(_ context.Context, value *conversation.Conversation) (conversation.SaveResult, error) {
	if fake.err != nil {
		return conversation.SaveResult{}, fake.err
	}
	copy := *value
	copy.Messages = append([]conversation.Message(nil), value.Messages...)
	fake.saves = append(fake.saves, &copy)
	return conversation.SaveResult{Kind: conversation.SaveBatch}, nil
}
func (*taskNotificationStoreFake) Maintain(context.Context) (conversation.MaintenanceResult, error) {
	return conversation.MaintenanceResult{}, errors.New("unexpected Maintain")
}

func TestTaskNotificationOrderedCommitDeduplicatesAndDetaches(t *testing.T) {
	now := time.Unix(100, 0)
	active := conversation.NewConversation("conversation-1", now.Add(-time.Minute))
	store := &taskNotificationStoreFake{}
	model := Model{
		deps: Deps{Store: store}, conversation: active,
		messages: tui.NewMessagesView(false),
	}
	event := completedTaskResultEvent(now, "notification-1", "conversation-1", "safe summary")

	if err := model.ApplyTaskEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	event.Result.Summary = redact.NewRuntimeRedactor().Redact("mutated after delivery")
	if len(store.saves) != 1 || len(active.Messages) != 1 {
		t.Fatalf("notification commit mismatch: saves=%d messages=%#v", len(store.saves), active.Messages)
	}
	message := active.Messages[0]
	if message.Role != conversation.RoleSubagentNotification || message.Subagent == nil ||
		message.Subagent.NotificationID != "notification-1" || message.Subagent.Summary.Text() != "safe summary" {
		t.Fatalf("notification was not detached and persisted safely: %#v", message)
	}
	if strings.Contains(model.messages.View(), "mutated after delivery") || !strings.Contains(model.messages.View(), "safe summary") {
		t.Fatalf("visible notification changed after source mutation: %q", model.messages.View())
	}

	if err := model.ApplyTaskEvent(context.Background(), completedTaskResultEvent(now, "notification-1", "conversation-1", "duplicate")); err != nil {
		t.Fatal(err)
	}
	if len(store.saves) != 1 || len(active.Messages) != 1 || strings.Contains(model.status.Notice, "duplicate") {
		t.Fatalf("replayed NotificationID was displayed or saved twice: saves=%d messages=%#v notice=%q", len(store.saves), active.Messages, model.status.Notice)
	}
}

func TestTaskNotificationSaveFailureRollsBackAndCanRetry(t *testing.T) {
	const secret = "raw-store-secret"
	now := time.Unix(200, 0)
	active := conversation.NewConversation("conversation-1", now.Add(-time.Minute))
	store := &taskNotificationStoreFake{err: errors.New("save failed: " + secret)}
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(secret)
	model := Model{deps: Deps{Store: store, RuntimeRedactor: redactor}, conversation: active, messages: tui.NewMessagesView(false)}
	event := completedTaskResultEvent(now, "notification-retry", "conversation-1", "safe summary")

	err := model.ApplyTaskEvent(context.Background(), event)
	var safe *diagnostics.SafeError
	if !errors.As(err, &safe) || safe.Code != string(subagent.ErrInternal) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "save failed") {
		t.Fatalf("save failure was not fixed safe error: %#v", err)
	}
	if len(active.Messages) != 0 || len(store.saves) != 0 || strings.Contains(model.messages.View(), "safe summary") {
		t.Fatalf("failed save leaked a partial commit: messages=%#v saves=%d view=%q", active.Messages, len(store.saves), model.messages.View())
	}

	store.err = nil
	if err := model.ApplyTaskEvent(context.Background(), event); err != nil {
		t.Fatalf("retry after rollback failed: %v", err)
	}
	if len(active.Messages) != 1 || len(store.saves) != 1 {
		t.Fatalf("retry was incorrectly deduplicated: messages=%#v saves=%d", active.Messages, len(store.saves))
	}
}

func TestTaskNotificationPersistsThroughTrackedJSONLStorePointer(t *testing.T) {
	now := time.Date(2026, time.August, 15, 18, 0, 0, 0, time.UTC)
	redactor := redact.NewRuntimeRedactor()
	options := conversation.JSONLStoreOptions{
		DataDir: filepath.Join(t.TempDir(), "sessions"), Redactor: redactor,
		MaxRecordBytes: 1 << 20, MaxSessionBytes: 8 << 20,
		MaxScanFiles: 32, MaxScanBytes: 8 << 20,
		RetentionDays: 7, GapReminderDays: 7, Now: func() time.Time { return now },
	}
	store, err := conversation.NewJSONLStore(options)
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	model := Model{
		deps: Deps{Store: store, RuntimeRedactor: redactor}, conversation: active,
		messages: tui.NewMessagesView(false),
	}
	event := completedTaskResultEvent(now, "notification-jsonl", active.ID, "persisted summary")

	if err := model.ApplyTaskEvent(context.Background(), event); err != nil {
		t.Fatalf("persist notification through tracked store pointer: %v", err)
	}
	if model.conversation != active || len(active.Messages) != 1 {
		t.Fatalf("tracked pointer or live notification lost: active=%p model=%p messages=%#v", active, model.conversation, active.Messages)
	}

	restarted, err := conversation.NewJSONLStore(options)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.Load(context.Background(), active.ID)
	if err != nil || !loaded.Available || loaded.Conversation == nil || len(loaded.Conversation.Messages) != 1 {
		t.Fatalf("reload persisted notification = %#v, %v", loaded, err)
	}
	message := loaded.Conversation.Messages[0]
	if message.Role != conversation.RoleSubagentNotification || message.Subagent == nil ||
		message.Subagent.NotificationID != "notification-jsonl" || message.Subagent.Summary.Text() != "persisted summary" {
		t.Fatalf("reloaded notification = %#v", message)
	}
}

func TestTaskNotificationSurvivesSameIDNavigationCandidateInterleaving(t *testing.T) {
	now := time.Date(2026, time.August, 15, 18, 30, 0, 0, time.UTC)
	redactor := redact.NewRuntimeRedactor()
	options := conversation.JSONLStoreOptions{
		DataDir: filepath.Join(t.TempDir(), "sessions"), Redactor: redactor,
		MaxRecordBytes: 1 << 20, MaxSessionBytes: 8 << 20,
		MaxScanFiles: 32, MaxScanBytes: 8 << 20,
		RetentionDays: 7, GapReminderDays: 7, Now: func() time.Time { return now },
	}
	store, err := conversation.NewJSONLStore(options)
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	model := Model{
		deps: Deps{Store: store, RuntimeRedactor: redactor}, conversation: active,
		messages: tui.NewMessagesView(false),
	}

	// Seal a same-ID reload before the task notification arrives. The Store
	// now tracks both the live pointer and the Load candidate pointer at the
	// same persisted revision.
	navigation := newNavigationState(newNavigationTestSink(t, redactor), redactor)
	request, err := navigation.beginNavigation(command.IntentOpenConversation, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	trace := []string{}
	prepared, safeErr := navigation.prepareNavigation(
		context.Background(), request, &navigationWaiterFake{trace: &trace}, store, active,
	)
	if safeErr != nil || !prepared.ready || !prepared.saved {
		t.Fatalf("navigation preparation = %#v error=%#v", prepared, safeErr)
	}
	candidate, safeErr := navigation.buildNavigationCandidate(context.Background(), prepared, store, navigationViewState{
		screen: screenChat, conversation: navigationConversationState(active),
		messages: navigationMessageProjection(active), active: active,
	})
	if safeErr != nil || candidate == nil || candidate.trackedActive == active {
		t.Fatalf("same-ID candidate = %#v error=%#v", candidate, safeErr)
	}

	event := completedTaskResultEvent(now.Add(time.Second), "notification-navigation-race", active.ID, "survives reload")
	if err := model.ApplyTaskEvent(context.Background(), event); err != nil {
		t.Fatalf("persist interleaved notification: %v", err)
	}
	if !hasTaskNotification(active, event.Result.NotificationID) {
		t.Fatal("notification did not reach the original tracked pointer")
	}
	liveNotification := active.Messages[len(active.Messages)-1].Subagent

	screenState := screenList
	conversationState := navigationConversationState(active)
	messageState := navigationMessageProjection(active)
	if !navigation.commitNavigationCandidate(context.Background(), candidate, navigationCommitTarget{
		screen: &screenState, conversation: &conversationState,
		messages: &messageState, active: &model.conversation,
	}, nil) {
		t.Fatal("same-ID navigation candidate was not committed")
	}
	if model.conversation != active || !hasTaskNotification(model.conversation, event.Result.NotificationID) {
		t.Fatalf("same-ID commit replaced the tracked pointer or lost its notification: original=%p active=%p messages=%#v",
			active, model.conversation, model.conversation.Messages)
	}
	if committed := model.conversation.Messages[len(model.conversation.Messages)-1].Subagent; committed == liveNotification {
		t.Fatal("same-ID merge retained the pre-commit notification payload pointer")
	}
	liveNotification.NotificationID = "mutated-stale-payload"
	if !hasTaskNotification(model.conversation, event.Result.NotificationID) {
		t.Fatal("committed notification changed through the stale payload alias")
	}

	// Replay must deduplicate against the merged live value and must not try to
	// Save through the stale Load pointer.
	if err := model.ApplyTaskEvent(context.Background(), event); err != nil {
		t.Fatalf("duplicate notification after same-ID commit: %v", err)
	}
	if countTaskNotifications(model.conversation, event.Result.NotificationID) != 1 {
		t.Fatalf("notification was duplicated after navigation: %#v", model.conversation.Messages)
	}
	restarted, err := conversation.NewJSONLStore(options)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.Load(context.Background(), active.ID)
	if err != nil || !loaded.Available || loaded.Conversation == nil ||
		countTaskNotifications(loaded.Conversation, event.Result.NotificationID) != 1 {
		t.Fatalf("persisted notification after navigation = %#v error=%v", loaded, err)
	}
}

func TestTaskNotificationOnlyTargetsCurrentConversationAndIgnoresOtherEvents(t *testing.T) {
	now := time.Unix(300, 0)
	active := conversation.NewConversation("current", now.Add(-time.Minute))
	store := &taskNotificationStoreFake{}
	model := Model{deps: Deps{Store: store}, conversation: active, messages: tui.NewMessagesView(false)}

	if err := model.ApplyTaskEvent(context.Background(), completedTaskResultEvent(now, "notification-other", "other", "not current")); err != nil {
		t.Fatal(err)
	}
	if err := model.ApplyTaskEvent(context.Background(), subagent.Event{Revision: 2, TaskID: "task-1", Sequence: 2, At: now, Kind: subagent.EventProgress, Snapshot: &subagent.TaskSnapshot{ID: "task-1"}}); err != nil {
		t.Fatal(err)
	}
	if len(active.Messages) != 0 || len(store.saves) != 0 || strings.Contains(model.messages.View(), "not current") {
		t.Fatalf("non-current or non-result event reached main conversation: messages=%#v saves=%d", active.Messages, len(store.saves))
	}

	// Ignoring another conversation must not consume the ID; a later current
	// conversation replay may still commit it exactly once.
	model.conversation = conversation.NewConversation("other", now.Add(-time.Minute))
	if err := model.ApplyTaskEvent(context.Background(), completedTaskResultEvent(now, "notification-other", "other", "now current")); err != nil {
		t.Fatal(err)
	}
	if len(model.conversation.Messages) != 1 || len(store.saves) != 1 {
		t.Fatalf("ignored notification ID was consumed globally: messages=%#v saves=%d", model.conversation.Messages, len(store.saves))
	}
}

func completedTaskResultEvent(now time.Time, notificationID, conversationID, summary string) subagent.Event {
	return subagent.Event{
		Revision: 11, TaskID: "task-1", Sequence: 4, At: now, Kind: subagent.EventResultPublished,
		Result: &subagent.ResultNotification{
			NotificationID: notificationID, CompletionRevision: 10, CompletionSequence: 3,
			CreatedAt: now, TaskID: "task-1", Parent: subagent.ParentRef{ConversationID: conversationID},
			Status: subagent.StatusCompleted, Summary: redact.NewRuntimeRedactor().Redact(summary), StopReason: subagent.StopCompleted,
		},
	}
}

func countTaskNotifications(value *conversation.Conversation, notificationID string) int {
	count := 0
	if value == nil {
		return count
	}
	for _, message := range value.Messages {
		if message.Role == conversation.RoleSubagentNotification && message.Subagent != nil && message.Subagent.NotificationID == notificationID {
			count++
		}
	}
	return count
}
