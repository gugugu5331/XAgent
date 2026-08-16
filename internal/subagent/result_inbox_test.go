package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

func TestResultInboxPublishesReservedResultsInStableConversationOrder(t *testing.T) {
	inbox := mustResultInbox(t, resultInboxTestLimits(), sequenceResultIDs("claim"))
	parentA := ParentRef{ConversationID: "conversation-a", ExecutionID: "source-a", RequestGeneration: 1}
	parentB := ParentRef{ConversationID: "conversation-b", ExecutionID: "source-b", RequestGeneration: 1}

	for _, reservation := range []struct {
		taskID ID
		parent ParentRef
	}{
		{taskID: "task-z", parent: parentA},
		{taskID: "task-b", parent: parentA},
		{taskID: "task-a", parent: parentA},
		{taskID: "task-other", parent: parentB},
	} {
		if err := inbox.Reserve(context.Background(), reservation.taskID, reservation.parent); err != nil {
			t.Fatalf("Reserve(%s): %v", reservation.taskID, err)
		}
	}

	// Publish deliberately disagrees with the required revision/task ordering.
	for _, notification := range []ResultNotification{
		resultInboxNotification("notification-z", "task-z", parentA, 20, "z"),
		resultInboxNotification("notification-b", "task-b", parentA, 10, "b"),
		resultInboxNotification("notification-other", "task-other", parentB, 1, "other"),
		resultInboxNotification("notification-a", "task-a", parentA, 10, "a"),
	} {
		if err := inbox.Publish(context.Background(), notification); err != nil {
			t.Fatalf("Publish(%s): %v", notification.TaskID, err)
		}
	}

	ownerA := ParentRef{ConversationID: parentA.ConversationID, ExecutionID: "consumer-a", RequestGeneration: 9}
	claim, err := inbox.Claim(context.Background(), ResultClaimOptions{
		Owner: ownerA, MaxNotifications: 10, MaxBytes: math.MaxInt64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claim.ClaimID != "claim-1" || claim.Owner != ownerA {
		t.Fatalf("claim identity = %#v", claim)
	}
	wantTasks := []ID{"task-a", "task-b", "task-z"}
	if got := resultClaimTaskIDs(claim); !equalResultIDs(got, wantTasks) {
		t.Fatalf("claim task order = %v, want %v", got, wantTasks)
	}
	wantBytes := int64(0)
	for _, notification := range claim.Notifications {
		payload, err := MarshalResultMessage(notification)
		if err != nil {
			t.Fatal(err)
		}
		wantBytes += int64(len(payload))
	}
	if claim.SerializedBytes != wantBytes {
		t.Fatalf("SerializedBytes=%d, want measured fixed projection %d", claim.SerializedBytes, wantBytes)
	}

	// Returned claims are deep snapshots, not aliases of retained notifications.
	claim.Notifications[0].Summary = redact.NewRuntimeRedactor().Redact("mutated")
	claim.Notifications[0].Error = SafeError(ErrInternal, redact.NewRuntimeRedactor().Redact("mutated"), false)
	if err := inbox.Release(context.Background(), claim.ClaimID, ownerA); err != nil {
		t.Fatal(err)
	}
	retry, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: ownerA, MaxNotifications: 10, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if got := retry.Notifications[0].Summary.Text(); got != "a" {
		t.Fatalf("retained summary aliased claim: %q", got)
	}
	if retry.Notifications[0].Error != nil {
		t.Fatalf("retained error aliased claim: %#v", retry.Notifications[0].Error)
	}
	if err := inbox.Ack(context.Background(), retry.ClaimID, ownerA); err != nil {
		t.Fatal(err)
	}

	ownerB := ParentRef{ConversationID: parentB.ConversationID, ExecutionID: "consumer-b", RequestGeneration: 1}
	other, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: ownerB, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultClaimTaskIDs(other); !equalResultIDs(got, []ID{"task-other"}) {
		t.Fatalf("conversation B claim = %v", got)
	}
	if err := inbox.Ack(context.Background(), other.ClaimID, ownerB); err != nil {
		t.Fatal(err)
	}
}

func TestResultInboxRequiresReservationAndPreservesPublishedResultOnReleaseReservation(t *testing.T) {
	inbox := mustResultInbox(t, resultInboxTestLimits(), sequenceResultIDs("claim"))
	parent := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 2}
	notification := resultInboxNotification("notification", "task", parent, 1, "safe")

	requireResultInboxCode(t, inbox.Publish(context.Background(), notification), ErrInvalidTransition)
	if err := inbox.Reserve(context.Background(), notification.TaskID, parent); err != nil {
		t.Fatal(err)
	}
	requireResultInboxCode(t, inbox.ReleaseReservation(context.Background(), notification.TaskID, ParentRef{
		ConversationID: parent.ConversationID, ExecutionID: parent.ExecutionID, RequestGeneration: parent.RequestGeneration + 1,
	}), ErrInvalidTransition)
	if err := inbox.Publish(context.Background(), notification); err != nil {
		t.Fatal(err)
	}
	if err := inbox.ReleaseReservation(context.Background(), notification.TaskID, parent); err != nil {
		t.Fatalf("ReleaseReservation after Publish must be harmless: %v", err)
	}
	if err := inbox.ReleaseReservation(context.Background(), notification.TaskID, parent); err != nil {
		t.Fatalf("repeated ReleaseReservation must be idempotent: %v", err)
	}
	requireResultInboxCode(t, inbox.Publish(context.Background(), notification), ErrInvalidTransition)

	owner := ParentRef{ConversationID: parent.ConversationID, ExecutionID: "consumer", RequestGeneration: 3}
	claim, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.Notifications) != 1 || claim.Notifications[0].TaskID != notification.TaskID {
		t.Fatalf("ReleaseReservation removed a published result: %#v", claim)
	}
}

func TestResultInboxReservationsEnforceCountAndByteCapacityBeforePublish(t *testing.T) {
	limits := resultInboxTestLimits()
	limits.MaxPendingResults = 3
	limits.MaxResultBytes = 512
	limits.MaxResultTotalBytes = 1400
	inbox := mustResultInbox(t, limits, sequenceResultIDs("claim"))
	parent := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}

	if err := inbox.Reserve(context.Background(), "task-1", parent); err != nil {
		t.Fatal(err)
	}
	if err := inbox.Reserve(context.Background(), "task-2", parent); err != nil {
		t.Fatal(err)
	}
	requireResultInboxCode(t, inbox.Reserve(context.Background(), "task-3", parent), ErrInboxFull)

	first := resultInboxNotification("notification-1", "task-1", parent, 1, "small")
	if payload, err := MarshalResultMessage(first); err != nil || len(payload) >= int(limits.MaxResultBytes) {
		t.Fatalf("test projection must leave reservation slack: bytes=%d err=%v", len(payload), err)
	}
	if err := inbox.Publish(context.Background(), first); err != nil {
		t.Fatalf("reserved Publish lost capacity to contention: %v", err)
	}
	if err := inbox.Reserve(context.Background(), "task-3", parent); err != nil {
		t.Fatalf("actual projection slack was not released: %v", err)
	}

	if err := inbox.ReleaseReservation(context.Background(), "task-2", parent); err != nil {
		t.Fatal(err)
	}
	if err := inbox.ReleaseReservation(context.Background(), "task-3", parent); err != nil {
		t.Fatal(err)
	}
	if err := inbox.ReleaseReservation(context.Background(), "task-2", parent); err != nil {
		t.Fatal(err)
	}
	if err := inbox.Reserve(context.Background(), "task-4", parent); err != nil {
		t.Fatalf("released reservation did not restore capacity: %v", err)
	}
}

func TestResultInboxRejectsSingleProjectionOverLimitWithoutConsumingReservation(t *testing.T) {
	limits := resultInboxTestLimits()
	limits.MaxResultBytes = 320
	limits.MaxResultTotalBytes = 640
	inbox := mustResultInbox(t, limits, sequenceResultIDs("claim"))
	parent := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}
	if err := inbox.Reserve(context.Background(), "task-large", parent); err != nil {
		t.Fatal(err)
	}
	large := resultInboxNotification("notification-large", "task-large", parent, 1, strings.Repeat("x", 256))
	payload, err := MarshalResultMessage(large)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(payload)) <= limits.MaxResultBytes {
		t.Fatalf("test payload bytes=%d, want > %d", len(payload), limits.MaxResultBytes)
	}
	requireResultInboxCode(t, inbox.Publish(context.Background(), large), ErrInboxFull)
	if err := inbox.ReleaseReservation(context.Background(), "task-large", parent); err != nil {
		t.Fatalf("oversize Publish consumed its reservation: %v", err)
	}
	if err := inbox.Reserve(context.Background(), "task-next", parent); err != nil {
		t.Fatalf("oversize reservation capacity leaked: %v", err)
	}
}

func TestResultClaimUsesOldestContiguousPrefixWithinBothLimits(t *testing.T) {
	limits := resultInboxTestLimits()
	limits.MaxResultsPerClaim = 2
	inbox := mustResultInbox(t, limits, sequenceResultIDs("claim"))
	parent := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}
	notifications := []ResultNotification{
		resultInboxNotification("notification-1", "task-1", parent, 1, "one"),
		resultInboxNotification("notification-2", "task-2", parent, 2, "two"),
		resultInboxNotification("notification-3", "task-3", parent, 3, "three"),
	}
	for _, notification := range notifications {
		if err := inbox.Reserve(context.Background(), notification.TaskID, parent); err != nil {
			t.Fatal(err)
		}
		if err := inbox.Publish(context.Background(), notification); err != nil {
			t.Fatal(err)
		}
	}
	firstPayload, _ := MarshalResultMessage(notifications[0])
	secondPayload, _ := MarshalResultMessage(notifications[1])
	owner := ParentRef{ConversationID: parent.ConversationID, ExecutionID: "consumer", RequestGeneration: 10}

	empty, err := inbox.Claim(context.Background(), ResultClaimOptions{
		Owner: owner, MaxNotifications: math.MaxInt, MaxBytes: int64(len(firstPayload) - 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if empty.ClaimID != "" || len(empty.Notifications) != 0 || empty.SerializedBytes != 0 {
		t.Fatalf("first item over budget created lease: %#v", empty)
	}

	claim, err := inbox.Claim(context.Background(), ResultClaimOptions{
		Owner: owner, MaxNotifications: math.MaxInt, MaxBytes: int64(len(firstPayload) + len(secondPayload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultClaimTaskIDs(claim); !equalResultIDs(got, []ID{"task-1", "task-2"}) {
		t.Fatalf("bounded claim = %v", got)
	}
	if claim.SerializedBytes != int64(len(firstPayload)+len(secondPayload)) {
		t.Fatalf("bounded bytes=%d", claim.SerializedBytes)
	}
	if err := inbox.Ack(context.Background(), claim.ClaimID, owner); err != nil {
		t.Fatal(err)
	}

	last, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultClaimTaskIDs(last); !equalResultIDs(got, []ID{"task-3"}) {
		t.Fatalf("Ack did not delete exactly claimed prefix: %v", got)
	}
}

func TestResultLeaseRequiresExactCurrentOwnerAndAckOnlyDeletesAfterAcceptance(t *testing.T) {
	inbox := mustResultInbox(t, resultInboxTestLimits(), sequenceResultIDs("claim"))
	source := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}
	reserveAndPublishResult(t, inbox, resultInboxNotification("notification", "task", source, 1, "safe"))
	owner := ParentRef{ConversationID: source.ConversationID, ExecutionID: "consumer", RequestGeneration: 4}
	claim, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}

	wrongOwners := []ParentRef{
		{ConversationID: "other-conversation", ExecutionID: owner.ExecutionID, RequestGeneration: owner.RequestGeneration},
		{ConversationID: owner.ConversationID, ExecutionID: "other-execution", RequestGeneration: owner.RequestGeneration},
		{ConversationID: owner.ConversationID, ExecutionID: owner.ExecutionID, RequestGeneration: owner.RequestGeneration - 1},
	}
	for _, wrong := range wrongOwners {
		requireResultInboxCode(t, inbox.Ack(context.Background(), claim.ClaimID, wrong), ErrInvalidTransition)
		requireResultInboxCode(t, inbox.Release(context.Background(), claim.ClaimID, wrong), ErrInvalidTransition)
	}
	requireResultInboxCode(t, inbox.Ack(context.Background(), "wrong-claim", owner), ErrInvalidTransition)
	requireResultInboxCode(t, inbox.Release(context.Background(), "wrong-claim", owner), ErrInvalidTransition)

	// Claim itself does not consume. A failed Provider preparation releases it,
	// and a later request in the same conversation gets a fresh owner-bound ID.
	requireResultInboxCode(t, func() error {
		_, claimErr := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
		return claimErr
	}(), ErrInvalidTransition)
	if err := inbox.Release(context.Background(), claim.ClaimID, owner); err != nil {
		t.Fatal(err)
	}
	if err := inbox.Release(context.Background(), claim.ClaimID, owner); err != nil {
		t.Fatalf("Release must be retryable: %v", err)
	}
	nextOwner := ParentRef{ConversationID: source.ConversationID, ExecutionID: "next-request", RequestGeneration: 5}
	retry, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: nextOwner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if retry.ClaimID == "" || retry.ClaimID == claim.ClaimID || len(retry.Notifications) != 1 {
		t.Fatalf("released result did not receive fresh lease: %#v", retry)
	}
	acceptedCtx, cancelAccepted := context.WithCancel(context.Background())
	cancelAccepted()
	if err := inbox.Ack(acceptedCtx, retry.ClaimID, nextOwner); err != nil {
		t.Fatal(err)
	}
	requireResultInboxCode(t, inbox.Ack(context.Background(), retry.ClaimID, nextOwner), ErrAlreadyConsumed)
	requireResultInboxCode(t, inbox.Release(context.Background(), retry.ClaimID, nextOwner), ErrAlreadyConsumed)

	empty, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: nextOwner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if empty.ClaimID != "" || len(empty.Notifications) != 0 {
		t.Fatalf("Ack did not consume notification: %#v", empty)
	}
}

func TestResultLeaseContextCancellationAutomaticallyReleasesForNextRequest(t *testing.T) {
	inbox := mustResultInbox(t, resultInboxTestLimits(), sequenceResultIDs("claim"))
	source := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}
	reserveAndPublishResult(t, inbox, resultInboxNotification("notification", "task", source, 1, "safe"))
	owner := ParentRef{ConversationID: source.ConversationID, ExecutionID: "consumer", RequestGeneration: 2}
	ctx, cancel := context.WithCancel(context.Background())
	claim, err := inbox.Claim(ctx, ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	nextOwner := ParentRef{ConversationID: source.ConversationID, ExecutionID: "next", RequestGeneration: 3}
	deadline := time.Now().Add(2 * time.Second)
	for {
		retry, claimErr := inbox.Claim(context.Background(), ResultClaimOptions{Owner: nextOwner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
		if claimErr == nil && retry.ClaimID != "" {
			if retry.ClaimID == claim.ClaimID {
				t.Fatal("auto Release reused stale claim identity")
			}
			if err := inbox.Ack(context.Background(), retry.ClaimID, nextOwner); err != nil {
				t.Fatal(err)
			}
			break
		}
		if claimErr != nil {
			requireResultInboxCode(t, claimErr, ErrInvalidTransition)
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled claim context did not auto Release")
		}
		time.Sleep(time.Millisecond)
	}
	if err := inbox.Release(context.Background(), claim.ClaimID, owner); err != nil {
		t.Fatalf("explicit Release racing auto Release must be idempotent: %v", err)
	}
}

func TestResultInboxRejectsMalformedOrMismatchedIdentities(t *testing.T) {
	limits := resultInboxTestLimits()
	limits.MaxIDBytes = 16
	inbox := mustResultInbox(t, limits, sequenceResultIDs("claim"))
	validParent := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}

	for _, taskID := range []ID{"", "bad\n", ID(strings.Repeat("x", 17))} {
		requireResultInboxCode(t, inbox.Reserve(context.Background(), taskID, validParent), ErrInvalidTask)
	}
	for _, parent := range []ParentRef{
		{},
		{ConversationID: "bad\n"},
		{ConversationID: strings.Repeat("x", 17)},
	} {
		requireResultInboxCode(t, inbox.Reserve(context.Background(), "task", parent), ErrInvalidParent)
	}
	if err := inbox.Reserve(context.Background(), "task", validParent); err != nil {
		t.Fatal(err)
	}
	requireResultInboxCode(t, inbox.Reserve(context.Background(), "task", validParent), ErrInvalidTransition)
	mismatched := resultInboxNotification("notification", "task", ParentRef{
		ConversationID: validParent.ConversationID, ExecutionID: "other", RequestGeneration: validParent.RequestGeneration,
	}, 1, "safe")
	requireResultInboxCode(t, inbox.Publish(context.Background(), mismatched), ErrInvalidTransition)

	invalidNotifications := []ResultNotification{
		resultInboxNotification("", "task", validParent, 1, "safe"),
		resultInboxNotification("bad\n", "task", validParent, 1, "safe"),
		resultInboxNotification("notification", "task", validParent, 0, "safe"),
	}
	invalidNotifications = append(invalidNotifications, func() ResultNotification {
		value := resultInboxNotification("notification", "task", validParent, 1, "safe")
		value.Status = StatusRunning
		return value
	}())
	invalidNotifications = append(invalidNotifications, func() ResultNotification {
		value := resultInboxNotification("notification", "task", validParent, 1, "safe")
		value.SummaryTruncated = true
		return value
	}())
	invalidNotifications = append(invalidNotifications, func() ResultNotification {
		value := resultInboxNotification("notification", "task", validParent, 1, "safe")
		value.Workspace = WorkspaceSummary{Isolation: "worktree", State: "unknown"}
		return value
	}())
	for _, notification := range invalidNotifications {
		requireResultInboxCode(t, inbox.Publish(context.Background(), notification), ErrInvalidTransition)
	}

	valid := resultInboxNotification("notification", "task", validParent, 1, "safe")
	if err := inbox.Publish(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	invalidOwners := []ParentRef{
		{ConversationID: validParent.ConversationID},
		{ConversationID: validParent.ConversationID, ExecutionID: "consumer"},
		{ConversationID: validParent.ConversationID, ExecutionID: "consumer", RequestGeneration: 0},
	}
	for _, owner := range invalidOwners {
		_, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
		requireResultInboxCode(t, err, ErrInvalidParent)
	}
	owner := ParentRef{ConversationID: validParent.ConversationID, ExecutionID: "consumer", RequestGeneration: 2}
	for _, options := range []ResultClaimOptions{
		{Owner: owner, MaxNotifications: 0, MaxBytes: 1},
		{Owner: owner, MaxNotifications: 1, MaxBytes: 0},
	} {
		_, err := inbox.Claim(context.Background(), options)
		requireResultInboxCode(t, err, ErrInvalidTransition)
	}
}

func TestResultInboxValidatesGeneratedClaimIDsAndRejectsReuse(t *testing.T) {
	for _, generated := range []ID{"", "bad\n", ID(strings.Repeat("x", 129))} {
		limits := resultInboxTestLimits()
		inbox := mustResultInbox(t, limits, func() (ID, error) { return generated, nil })
		source := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}
		reserveAndPublishResult(t, inbox, resultInboxNotification("notification", "task", source, 1, "safe"))
		_, err := inbox.Claim(context.Background(), ResultClaimOptions{
			Owner:            ParentRef{ConversationID: source.ConversationID, ExecutionID: "consumer", RequestGeneration: 2},
			MaxNotifications: 1,
			MaxBytes:         math.MaxInt64,
		})
		requireResultInboxCode(t, err, ErrInternal)
	}

	generatorCalls := 0
	inbox := mustResultInbox(t, resultInboxTestLimits(), func() (ID, error) {
		generatorCalls++
		return "duplicate", nil
	})
	source := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}
	for index := 1; index <= 2; index++ {
		reserveAndPublishResult(t, inbox, resultInboxNotification(
			ID("notification-"+string(rune('0'+index))), ID("task-"+string(rune('0'+index))), source, uint64(index), "safe",
		))
	}
	owner := ParentRef{ConversationID: source.ConversationID, ExecutionID: "consumer", RequestGeneration: 2}
	claim, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.Release(context.Background(), claim.ClaimID, owner); err != nil {
		t.Fatal(err)
	}
	_, err = inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	requireResultInboxCode(t, err, ErrInternal)
	if generatorCalls != 2 {
		t.Fatalf("ID generator calls=%d, want 2", generatorCalls)
	}

	inbox = mustResultInbox(t, resultInboxTestLimits(), func() (ID, error) { return "", errors.New("entropy unavailable") })
	reserveAndPublishResult(t, inbox, resultInboxNotification("notification-error", "task-error", source, 1, "safe"))
	_, err = inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	requireResultInboxCode(t, err, ErrInternal)
}

func TestMarshalResultMessageUsesFixedSafeSchemaAndExactFieldOrder(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	notification := ResultNotification{
		NotificationID:     "notification-secret-parent",
		CompletionRevision: 7,
		CompletionSequence: 8,
		CreatedAt:          time.Unix(100, 0),
		TaskID:             "task-1",
		Parent: ParentRef{
			ConversationID: "must-not-serialize", ExecutionID: "secret-execution", RequestGeneration: 91,
		},
		Status:           StatusFailed,
		Summary:          redactor.Redact("safe summary"),
		SummaryTruncated: true,
		TruncationReason: redactor.Redact("summary_bytes"),
		StopReason:       StopProviderError,
		Usage:            Usage{InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheReadInputTokens: 4},
		Error:            SafeError(ErrProviderFailed, redactor.Redact("safe failure"), true),
	}
	payload, err := MarshalResultMessage(notification)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"task_id":"task-1","status":"failed","summary":"safe summary","summary_truncated":true,"truncation_reason":"summary_bytes","stop_reason":"provider_error","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4},"error":{"code":"provider_failed","message":"safe failure","recoverable":true}}`
	if string(payload) != want {
		t.Fatalf("fixed result projection:\n got %s\nwant %s", payload, want)
	}
	for _, forbidden := range []string{"notification-secret-parent", "must-not-serialize", "secret-execution", "completion_revision", "created_at", "parent"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("fixed projection leaked %q: %s", forbidden, payload)
		}
	}
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatal(err)
	}
	if len(value) != 9 {
		t.Fatalf("fixed projection fields=%d, want 9: %#v", len(value), value)
	}

	notification.Error.Recoverable = false
	payload, err = MarshalResultMessage(notification)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "recoverable") {
		t.Fatalf("optional false recoverable field was not omitted: %s", payload)
	}
}

func TestWorkspaceSummaryResultProjectionUsesStablePathFreeSchema(t *testing.T) {
	workspaceID := strings.Repeat("a", 32)
	notification := resultInboxNotification("notification-workspace", "task-workspace", ParentRef{
		ConversationID: "conversation-workspace", ExecutionID: "execution-workspace", RequestGeneration: 1,
	}, 7, "settled")
	notification.Workspace = WorkspaceSummary{
		WorkspaceID: workspaceID, Isolation: "worktree", State: "retained",
		BaseOID: strings.Repeat("b", 40), Branch: "xagent/worktree/" + workspaceID,
		Dirty: true, Unpushed: true, Cleanup: "retained", RetentionCause: "dirty_worktree",
		Error: SafeError(ErrInternal, redact.NewRuntimeRedactor().Redact("/private/worktree/root must not project"), false),
	}

	payload, err := MarshalResultMessage(notification)
	if err != nil {
		t.Fatal(err)
	}
	wantWorkspace := `"workspace":{"workspace_id":"` + workspaceID + `","isolation":"worktree","state":"retained","base_oid":"` + strings.Repeat("b", 40) + `","branch":"xagent/worktree/` + workspaceID + `","dirty":true,"unpushed":true,"cleanup":"retained","retention_cause":"dirty_worktree"}`
	if !strings.Contains(string(payload), wantWorkspace) {
		t.Fatalf("workspace projection is missing or unstable: %s", payload)
	}
	for _, forbidden := range []string{"/private/worktree/root", `"workspace":{"error"`, `"message"`} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("workspace projection leaked untrusted field %q: %s", forbidden, payload)
		}
	}
}

func TestWorkspaceSummaryStoredResultDoesNotAliasPublisherOrClaim(t *testing.T) {
	inbox := mustResultInbox(t, resultInboxTestLimits(), sequenceResultIDs("claim-workspace"))
	parent := ParentRef{ConversationID: "conversation-workspace-clone", ExecutionID: "source", RequestGeneration: 1}
	owner := ParentRef{ConversationID: parent.ConversationID, ExecutionID: "consumer", RequestGeneration: 2}
	workspaceID := strings.Repeat("c", 32)
	notification := resultInboxNotification("notification-workspace-clone", "task-workspace-clone", parent, 9, "settled")
	notification.Workspace = WorkspaceSummary{
		WorkspaceID: workspaceID, Isolation: "worktree", State: "partial",
		BaseOID: strings.Repeat("d", 40), Branch: "xagent/worktree/" + workspaceID,
		Cleanup: "partial", RetentionCause: "settlement_failed",
		Error: SafeError(ErrInternal, redact.NewRuntimeRedactor().Redact("settlement failed"), false),
	}
	reserveAndPublishResult(t, inbox, notification)
	notification.Workspace.Error.Code = "publisher-mutated"

	claim, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if got := claim.Notifications[0].Workspace.Error.Code; got != string(ErrInternal) {
		t.Fatalf("stored workspace aliased publisher: code=%q", got)
	}
	claim.Notifications[0].Workspace.Error.Code = "claim-mutated"
	if err := inbox.Release(context.Background(), claim.ClaimID, owner); err != nil {
		t.Fatal(err)
	}
	retry, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if got := retry.Notifications[0].Workspace.Error.Code; got != string(ErrInternal) {
		t.Fatalf("stored workspace aliased released claim: code=%q", got)
	}
}

func TestResultInboxCloseIsIdempotentAndStopsAllOperations(t *testing.T) {
	inbox := mustResultInbox(t, resultInboxTestLimits(), sequenceResultIDs("claim"))
	source := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}
	reserveAndPublishResult(t, inbox, resultInboxNotification("notification", "task", source, 1, "safe"))
	owner := ParentRef{ConversationID: source.ConversationID, ExecutionID: "consumer", RequestGeneration: 2}
	claim, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	shutdown := SafeError(ErrShutdown, redact.NewRuntimeRedactor().Redact("application closed"), false)
	inbox.Close(shutdown)
	inbox.Close(SafeError(ErrInternal, redact.NewRuntimeRedactor().Redact("later"), false))

	requireResultInboxCode(t, inbox.Reserve(context.Background(), "future", source), ErrShutdown)
	requireResultInboxCode(t, inbox.ReleaseReservation(context.Background(), "task", source), ErrShutdown)
	requireResultInboxCode(t, inbox.Publish(context.Background(), resultInboxNotification("future", "future", source, 2, "safe")), ErrShutdown)
	_, err = inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
	requireResultInboxCode(t, err, ErrShutdown)
	requireResultInboxCode(t, inbox.Ack(context.Background(), claim.ClaimID, owner), ErrShutdown)
	requireResultInboxCode(t, inbox.Release(context.Background(), claim.ClaimID, owner), ErrShutdown)
}

func TestResultInboxConcurrentPublishMaintainsStableOrderAndDetachedSnapshots(t *testing.T) {
	limits := resultInboxTestLimits()
	limits.MaxPendingResults = 128
	limits.MaxResultsPerClaim = 128
	limits.MaxResultBytes = 1024
	limits.MaxResultTotalBytes = 128 * 1024
	inbox := mustResultInbox(t, limits, sequenceResultIDs("claim"))
	parent := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}
	const count = 64
	notifications := make([]ResultNotification, count)
	for index := range count {
		// Stable unique lexical IDs without relying on publish order.
		taskID := ID("task-" + leftPadResultInt(index, 3))
		notifications[index] = resultInboxNotification(ID("notification-"+leftPadResultInt(index, 3)), taskID, parent, uint64(count-index), "safe")
		if err := inbox.Reserve(context.Background(), taskID, parent); err != nil {
			t.Fatal(err)
		}
	}
	var wait sync.WaitGroup
	errorsByTask := make(chan error, count)
	for index := range notifications {
		notification := notifications[index]
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByTask <- inbox.Publish(context.Background(), notification)
		}()
	}
	wait.Wait()
	close(errorsByTask)
	for err := range errorsByTask {
		if err != nil {
			t.Fatal(err)
		}
	}

	owner := ParentRef{ConversationID: parent.ConversationID, ExecutionID: "consumer", RequestGeneration: 2}
	claim, err := inbox.Claim(context.Background(), ResultClaimOptions{Owner: owner, MaxNotifications: count, MaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.Notifications) != count {
		t.Fatalf("concurrent claim count=%d, want %d", len(claim.Notifications), count)
	}
	if !sort.SliceIsSorted(claim.Notifications, func(i, j int) bool {
		left, right := claim.Notifications[i], claim.Notifications[j]
		if left.CompletionRevision != right.CompletionRevision {
			return left.CompletionRevision < right.CompletionRevision
		}
		return left.TaskID < right.TaskID
	}) {
		t.Fatalf("concurrent Publish order is unstable: %v", resultClaimTaskIDs(claim))
	}
	if err := inbox.Ack(context.Background(), claim.ClaimID, owner); err != nil {
		t.Fatal(err)
	}
}

func TestResultLeaseCancelReleaseAndAckRaceHasOneSafeOutcome(t *testing.T) {
	for iteration := range 50 {
		inbox := mustResultInbox(t, resultInboxTestLimits(), sequenceResultIDs("claim"))
		source := ParentRef{ConversationID: "conversation", ExecutionID: "source", RequestGeneration: 1}
		reserveAndPublishResult(t, inbox, resultInboxNotification("notification", "task", source, 1, "safe"))
		owner := ParentRef{ConversationID: source.ConversationID, ExecutionID: "consumer", RequestGeneration: 2}
		ctx, cancel := context.WithCancel(context.Background())
		claim, err := inbox.Claim(ctx, ResultClaimOptions{Owner: owner, MaxNotifications: 1, MaxBytes: math.MaxInt64})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		outcomes := make(chan error, 2)
		go func() {
			<-start
			outcomes <- inbox.Ack(context.Background(), claim.ClaimID, owner)
		}()
		go func() {
			<-start
			cancel()
			outcomes <- inbox.Release(context.Background(), claim.ClaimID, owner)
		}()
		close(start)
		first, second := <-outcomes, <-outcomes
		valid := func(err error) bool {
			if err == nil {
				return true
			}
			var safe *diagnostics.SafeError
			return errors.As(err, &safe) && (safe.Code == string(ErrAlreadyConsumed) || safe.Code == string(ErrInvalidTransition))
		}
		if !valid(first) || !valid(second) {
			t.Fatalf("iteration %d race outcomes: %v, %v", iteration, first, second)
		}
		inbox.Close(nil)
	}
}

func TestNewResultInboxRejectsInvalidOptions(t *testing.T) {
	_, err := NewResultInbox(ResultInboxOptions{})
	requireResultInboxCode(t, err, ErrInternal)
	limits := resultInboxTestLimits()
	limits.MaxResultBytes = 0
	_, err = NewResultInbox(ResultInboxOptions{Limits: limits, IDGenerator: sequenceResultIDs("claim")})
	requireResultInboxCode(t, err, ErrInternal)
}

func mustResultInbox(t *testing.T, limits Limits, generator func() (ID, error)) *resultInbox {
	t.Helper()
	inbox, err := NewResultInbox(ResultInboxOptions{Limits: limits, IDGenerator: generator})
	if err != nil {
		t.Fatal(err)
	}
	return inbox
}

func resultInboxTestLimits() Limits {
	return DefaultLimits()
}

func sequenceResultIDs(prefix string) func() (ID, error) {
	var mu sync.Mutex
	next := 0
	return func() (ID, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return ID(prefix + "-" + leftPadResultInt(next, 1)), nil
	}
}

func resultInboxNotification(notificationID, taskID ID, parent ParentRef, revision uint64, summary string) ResultNotification {
	return ResultNotification{
		NotificationID:     string(notificationID),
		CompletionRevision: revision,
		CompletionSequence: revision,
		CreatedAt:          time.Unix(int64(revision), 0),
		TaskID:             taskID,
		Parent:             parent,
		Status:             StatusCompleted,
		Summary:            redact.NewRuntimeRedactor().Redact(summary),
		StopReason:         StopCompleted,
	}
}

func reserveAndPublishResult(t *testing.T, inbox ResultInbox, notification ResultNotification) {
	t.Helper()
	if err := inbox.Reserve(context.Background(), notification.TaskID, notification.Parent); err != nil {
		t.Fatal(err)
	}
	if err := inbox.Publish(context.Background(), notification); err != nil {
		t.Fatal(err)
	}
}

func resultClaimTaskIDs(claim ResultClaim) []ID {
	ids := make([]ID, len(claim.Notifications))
	for index := range claim.Notifications {
		ids[index] = claim.Notifications[index].TaskID
	}
	return ids
}

func equalResultIDs(left, right []ID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func requireResultInboxCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error=nil, want code %q", code)
	}
	var safe *diagnostics.SafeError
	if !errors.As(err, &safe) || safe.Code != string(code) {
		t.Fatalf("error=%T %v, want SafeError code %q", err, err, code)
	}
}

func leftPadResultInt(value, width int) string {
	digits := []byte{}
	if value == 0 {
		digits = append(digits, '0')
	}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	for len(digits) < width {
		digits = append([]byte{'0'}, digits...)
	}
	return string(digits)
}
