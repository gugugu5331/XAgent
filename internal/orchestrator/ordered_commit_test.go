package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

func testToolResultsHaveSingleOrderedCommit(t *testing.T) {
	t.Helper()

	t.Run("safe projections commit by call ordinal before one save", func(t *testing.T) {
		orch, factory := newProjectionCandidate(t, nil)
		store := &orderedCommitStore{Store: orch.store}
		orch.store = store
		conv := conversation.NewConversation("ordered-tool-results", time.Now())
		state := &executionState{profile: skill.ExecutionProfile{Persist: true}}
		if err := orch.configureCandidateState(state, conv.ID); err != nil {
			t.Fatal(err)
		}

		const compatibilityCanary = "COMPATIBILITY_RESULT_MUST_NOT_BE_READ"
		second := orderedCommitExecution(t, factory, 1, "call-1", "safe-one")
		first := orderedCommitExecution(t, factory, 0, "call-0", "safe-zero")
		for _, execution := range []*ToolExecution{&second, &first} {
			execution.Result.CallID = compatibilityCanary
			execution.Result.Name = compatibilityCanary
			execution.Result.Status = tool.StatusError
			execution.Result.Summary = compatibilityCanary
			execution.Result.Content = compatibilityCanary
			execution.Result.Data = map[string]any{"unsafe": compatibilityCanary}
			execution.Result.Error = &tool.Error{Code: compatibilityCanary, Message: compatibilityCanary}
		}

		out := make(chan events.Event, 2)
		published, err := orch.publishToolExecutions(context.Background(), conv, state, 1, []ToolExecution{second, first}, out)
		if err != nil {
			t.Fatal(err)
		}
		if len(published) != 2 || published[0].Index != 1 || published[1].Index != 0 || !published[0].Projected || !published[1].Projected {
			t.Fatalf("return-slot contract changed: %#v", published)
		}

		saves, snapshots := store.snapshot()
		if saves != 1 || len(snapshots) != 1 {
			t.Fatalf("ordered save count = %d, snapshots=%d", saves, len(snapshots))
		}
		assertOrderedToolFacts(t, conv.Messages, compatibilityCanary)
		assertOrderedToolFacts(t, snapshots[0], compatibilityCanary)

		for index, callID := range []string{"call-0", "call-1"} {
			event := <-out
			if event.Tool == nil || event.Tool.CallID != callID || strings.Contains(event.Tool.Summary.Text()+event.Tool.Stdout.Text()+event.Tool.Stderr.Text(), compatibilityCanary) {
				t.Fatalf("ordered safe event %d = %#v", index, event)
			}
		}
	})

	t.Run("event failure still saves committed execution fact", func(t *testing.T) {
		orch, factory := newProjectionCandidate(t, nil)
		store := &orderedCommitStore{Store: orch.store}
		orch.store = store
		conv := conversation.NewConversation("event-failure-tool-result", time.Now())
		state := &executionState{profile: skill.ExecutionProfile{Persist: true}}
		if err := orch.configureCandidateState(state, conv.ID); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		published, err := orch.publishToolExecutions(ctx, conv, state, 1, []ToolExecution{
			orderedCommitExecution(t, factory, 0, "call-event", "event-safe"),
		}, make(chan events.Event))
		if !isOrderedToolCommitError(err) || !errors.Is(err, context.Canceled) {
			t.Fatalf("event failure = %v", err)
		}
		if len(published) != 1 || !published[0].Projected || len(conv.Messages) != 2 {
			t.Fatalf("event failure discarded execution fact: published=%#v messages=%#v", published, conv.Messages)
		}
		saves, snapshots := store.snapshot()
		if saves != 1 || len(snapshots) != 1 || len(snapshots[0]) != 2 {
			t.Fatalf("event failure save = %d snapshots=%#v", saves, snapshots)
		}
	})

	t.Run("save failure retains in-memory execution fact without retry", func(t *testing.T) {
		wantErr := errors.New("ordered save failed")
		orch, factory := newProjectionCandidate(t, nil)
		store := &orderedCommitStore{Store: orch.store, saveErr: wantErr}
		orch.store = store
		conv := conversation.NewConversation("save-failure-tool-result", time.Now())
		state := &executionState{profile: skill.ExecutionProfile{Persist: true}}
		if err := orch.configureCandidateState(state, conv.ID); err != nil {
			t.Fatal(err)
		}

		published, err := orch.publishToolExecutions(context.Background(), conv, state, 1, []ToolExecution{
			orderedCommitExecution(t, factory, 0, "call-save", "save-safe"),
		}, make(chan events.Event, 1))
		if !isOrderedToolCommitError(err) || !errors.Is(err, wantErr) {
			t.Fatalf("save failure = %v", err)
		}
		if len(published) != 1 || !published[0].Projected || len(conv.Messages) != 2 {
			t.Fatalf("save failure rolled back execution fact: published=%#v messages=%#v", published, conv.Messages)
		}
		saves, snapshots := store.snapshot()
		if saves != 1 || len(snapshots) != 1 || len(snapshots[0]) != 2 {
			t.Fatalf("save failure attempts = %d snapshots=%#v", saves, snapshots)
		}
	})
}

func orderedCommitExecution(t *testing.T, factory *tool.ResultFactory, index int, callID, preview string) ToolExecution {
	t.Helper()
	result, err := factory.Build(tool.ResultFactoryInput{
		CallID: callID, Name: "Read", State: tool.Completed, Status: tool.StatusSuccess,
		Summary: "completed " + callID, Preview: preview, CapturedBytes: int64(len(preview)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ToolExecution{
		Call:  tool.Call{ID: callID, Name: "Read", ArgumentsJSON: `{"path":"safe.txt"}`},
		State: tool.Completed, Result: result, HasResult: true, Index: index,
	}
}

func assertOrderedToolFacts(t *testing.T, messages []conversation.Message, forbidden string) {
	t.Helper()
	if len(messages) != 4 {
		t.Fatalf("tool fact message count = %d, want 4", len(messages))
	}
	for index, callID := range []string{"call-0", "call-1"} {
		callMessage := messages[index*2]
		resultMessage := messages[index*2+1]
		if callMessage.Tool == nil || resultMessage.Tool == nil || callMessage.Tool.CallID != callID || resultMessage.Tool.CallID != callID {
			t.Fatalf("tool fact order = %#v", messages)
		}
		joined := callMessage.Content.Text() + callMessage.Tool.ArgumentsJSON.Text() + resultMessage.Content.Text() + resultMessage.Tool.Result.Text()
		if strings.Contains(joined, forbidden) {
			t.Fatalf("compatibility Result leaked into committed projection: %q", joined)
		}
	}
}

type orderedCommitStore struct {
	conversation.Store

	mu        sync.Mutex
	saves     int
	snapshots [][]conversation.Message
	saveErr   error
}

func (s *orderedCommitStore) Save(_ context.Context, conv *conversation.Conversation) (conversation.SaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if conv != nil {
		s.snapshots = append(s.snapshots, cloneMessages(conv.Messages))
	}
	return conversation.SaveResult{Kind: conversation.SaveNoop}, s.saveErr
}

func (s *orderedCommitStore) snapshot() (int, [][]conversation.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([][]conversation.Message, len(s.snapshots))
	for index := range s.snapshots {
		result[index] = cloneMessages(s.snapshots[index])
	}
	return s.saves, result
}
