package orchestrator

import (
	"context"
	"math"
	"reflect"
	"sort"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/hook"
	"xagent/internal/skill"
)

func TestModelContentTemporarySlotLifecycleMatrix(t *testing.T) {
	canonical := []string{
		"success_clears_after_stream_handoff",
		"provider_error_clears",
		"cancel_clears",
		"save_failure_clears",
		"session_switch_clears",
		"same_call_id_concurrent_runs_isolated",
		"iteration_ordinal_reset_isolated",
		"survivor_rebase_preserves_unique_mapping",
		"ambiguous_rebase_fails_closed",
		"record_limit_fails_before_side_effect",
		"session_limit_fails_before_side_effect",
		"checked_add_overflow_fails_before_side_effect",
		"restart_uses_persisted_content_only",
	}
	seen := make([]string, 0, len(canonical))
	run := func(name string, test func(*testing.T)) {
		t.Run(name, func(t *testing.T) {
			seen = append(seen, name)
			test(t)
		})
	}

	run("success_clears_after_stream_handoff", func(t *testing.T) {
		state, key := stagedModelContentFixture(t, "session", 1, 1, 3, "call", "model-only")
		assertModelContentLookup(t, state, key, "call", "model-only")
		conv := modelContentConversationFixture("session", "call")
		provider := &captureProvider{}
		orch, _ := newProjectionCandidate(t, hook.Noop())
		orch.provider = provider
		stream, err := orch.streamWithExecutionState(context.Background(), conv, RunModeDefault, skill.ExecutionProfile{}, false, 1, hook.ExecutionRef{}, state)
		if err != nil || stream == nil {
			t.Fatalf("StreamChat handoff = (%v,%v)", stream, err)
		}
		if closeErr := closeProviderStream(stream); closeErr != nil {
			t.Fatalf("close handed-off stream: %v", closeErr)
		}
		foundModelOnly := false
		for _, message := range provider.request.Messages {
			foundModelOnly = foundModelOnly || message.Role == "tool_result" && message.Content.Text() == "model-only"
		}
		if !foundModelOnly {
			t.Fatalf("Provider handoff did not receive ModelContent: %#v", provider.request.Messages)
		}
		assertModelContentSlotsEmpty(t, state)
		assertModelContentMissing(t, state, key, "call")
	})

	run("provider_error_clears", func(t *testing.T) {
		state, key := stagedModelContentFixture(t, "session", 1, 1, 3, "call", "provider-error-model-only")
		conv := modelContentConversationFixture("session", "call")
		orch, _ := newProjectionCandidate(t, hook.Noop())
		orch.provider = &immediateProviderError{err: context.DeadlineExceeded}
		if stream, err := orch.streamWithExecutionState(context.Background(), conv, RunModeDefault, skill.ExecutionProfile{}, false, 1, hook.ExecutionRef{}, state); err == nil || stream != nil {
			t.Fatalf("Provider error handoff = (%v,%v), want nil,error", stream, err)
		}
		assertModelContentSlotsEmpty(t, state)
		assertModelContentMissing(t, state, key, "call")
	})

	run("cancel_clears", func(t *testing.T) {
		state, key := stagedModelContentFixture(t, "session", 1, 1, 3, "call", "cancel-model-only")
		conv := modelContentConversationFixture("session", "call")
		provider := newCancelDuringStreamStartProvider()
		orch, _ := newProjectionCandidate(t, hook.Noop())
		orch.provider = provider
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			stream, err := orch.streamWithExecutionState(ctx, conv, RunModeDefault, skill.ExecutionProfile{}, false, 1, hook.ExecutionRef{}, state)
			if stream != nil {
				_ = closeProviderStream(stream)
			}
			done <- err
		}()
		select {
		case <-provider.entered:
		case <-time.After(time.Second):
			t.Fatal("Provider did not enter StreamChat")
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("canceled StreamChat returned no error")
			}
		case <-time.After(time.Second):
			t.Fatal("canceled StreamChat did not return")
		}
		assertModelContentSlotsEmpty(t, state)
		assertModelContentMissing(t, state, key, "call")
	})

	run("save_failure_clears", func(t *testing.T) {
		state, key := stagedModelContentFixture(t, "session", 1, 1, 3, "call", "save-failure-model-only")
		conv := modelContentConversationFixture("session", "call")
		provider := &fakeProvider{}
		orch, _ := newProjectionCandidate(t, hook.Noop())
		orch.provider = provider
		orch.store = failingSaveStore{}
		orch.sessionContext = &fakeSessionContext{changed: true}
		if stream, err := orch.streamWithExecutionState(context.Background(), conv, RunModeDefault, skill.ExecutionProfile{Persist: true}, false, 1, hook.ExecutionRef{}, state); err == nil || stream != nil {
			t.Fatalf("save failure handoff = (%v,%v), want nil,error", stream, err)
		}
		if provider.calls != 0 {
			t.Fatalf("Provider called before failed save: %d", provider.calls)
		}
		assertModelContentSlotsEmpty(t, state)
		assertModelContentMissing(t, state, key, "call")
	})

	run("session_switch_clears", func(t *testing.T) {
		state, key := stagedModelContentFixture(t, "session-a", 1, 1, 3, "call", "model-only")
		if err := state.beginModelContentIteration("session-b", 1, 1); err == nil {
			t.Fatal("session switch unexpectedly retained the slot owner")
		}
		assertModelContentSlotsEmpty(t, state)
		assertModelContentMissing(t, state, key, "call")
	})

	run("same_call_id_concurrent_runs_isolated", func(t *testing.T) {
		first, firstKey := stagedModelContentFixture(t, "session", 1, 1, 3, "same-call", "first-run")
		second, secondKey := stagedModelContentFixture(t, "session", 2, 1, 3, "same-call", "second-run")
		assertModelContentLookup(t, first, firstKey, "same-call", "first-run")
		assertModelContentLookup(t, second, secondKey, "same-call", "second-run")
		first.clearModelContentSlots()
		assertModelContentSlotsEmpty(t, first)
		assertModelContentLookup(t, second, secondKey, "same-call", "second-run")
	})

	run("iteration_ordinal_reset_isolated", func(t *testing.T) {
		state, firstKey := stagedModelContentFixture(t, "session", 1, 1, 3, "call-1", "first")
		if firstKey.ordinal != 0 {
			t.Fatalf("first iteration ordinal = %d, want 0", firstKey.ordinal)
		}
		state.clearModelContentSlots()
		if err := state.beginModelContentIteration("session", 1, 2); err != nil {
			t.Fatalf("begin second iteration: %v", err)
		}
		secondKey, err := state.stageModelContent("session", 1, 2, 5, "call-2", testSafeText("second"))
		if err != nil {
			t.Fatalf("stage second iteration: %v", err)
		}
		if secondKey.ordinal != 0 || secondKey.iteration != 2 {
			t.Fatalf("second iteration key = %#v, want reset ordinal 0", secondKey)
		}
		assertModelContentMissing(t, state, firstKey, "call-1")
		assertModelContentLookup(t, state, secondKey, "call-2", "second")
	})

	run("survivor_rebase_preserves_unique_mapping", func(t *testing.T) {
		state, keptKey := stagedModelContentFixture(t, "session", 1, 1, 3, "kept", "keep-me")
		evictedKey, err := state.stageModelContent("session", 1, 1, 5, "evicted", testSafeText("drop-me"))
		if err != nil {
			t.Fatalf("stage evicted slot: %v", err)
		}
		if err := state.rebaseModelContentSlots(map[int][]int{3: {1}}); err != nil {
			t.Fatalf("rebase unique survivor: %v", err)
		}
		rebasedKey := keptKey
		rebasedKey.messageIndex = 1
		assertModelContentMissing(t, state, keptKey, "kept")
		assertModelContentLookup(t, state, rebasedKey, "kept", "keep-me")
		assertModelContentMissing(t, state, evictedKey, "evicted")
		count, total := state.modelContentSlotStats()
		if count != 1 || total != int64(len("keep-me")) {
			t.Fatalf("rebased stats = (%d,%d), want (1,%d)", count, total, len("keep-me"))
		}
	})

	run("ambiguous_rebase_fails_closed", func(t *testing.T) {
		state, key := stagedModelContentFixture(t, "session", 1, 1, 3, "call", "model-only")
		if err := state.rebaseModelContentSlots(map[int][]int{3: {1, 2}}); err == nil {
			t.Fatal("ambiguous survivor mapping unexpectedly succeeded")
		}
		assertModelContentSlotsEmpty(t, state)
		assertModelContentMissing(t, state, key, "call")
	})

	run("record_limit_fails_before_side_effect", func(t *testing.T) {
		state := configuredModelContentState(t, "session", 1, 4, 8)
		if err := state.beginModelContentIteration("session", 1, 1); err != nil {
			t.Fatalf("begin iteration: %v", err)
		}
		if _, err := state.stageModelContent("session", 1, 1, 3, "call", testSafeText("12345")); err == nil {
			t.Fatal("record limit unexpectedly accepted oversized model content")
		}
		assertModelContentSlotsEmpty(t, state)
		if state.modelContents.nextOrdinal != 0 {
			t.Fatalf("failed record stage consumed ordinal %d", state.modelContents.nextOrdinal)
		}
	})

	run("session_limit_fails_before_side_effect", func(t *testing.T) {
		state := configuredModelContentState(t, "session", 1, 4, 6)
		if err := state.beginModelContentIteration("session", 1, 1); err != nil {
			t.Fatalf("begin iteration: %v", err)
		}
		firstKey, err := state.stageModelContent("session", 1, 1, 3, "first", testSafeText("1234"))
		if err != nil {
			t.Fatalf("stage first content: %v", err)
		}
		if _, err := state.stageModelContent("session", 1, 1, 5, "second", testSafeText("567")); err == nil {
			t.Fatal("session limit unexpectedly accepted cumulative overflow")
		}
		assertModelContentLookup(t, state, firstKey, "first", "1234")
		count, total := state.modelContentSlotStats()
		if count != 1 || total != 4 || state.modelContents.nextOrdinal != 1 {
			t.Fatalf("failed session stage mutated state: count=%d total=%d ordinal=%d", count, total, state.modelContents.nextOrdinal)
		}
	})

	run("checked_add_overflow_fails_before_side_effect", func(t *testing.T) {
		state := configuredModelContentState(t, "session", 1, math.MaxInt64, math.MaxInt64)
		if err := state.beginModelContentIteration("session", 1, 1); err != nil {
			t.Fatalf("begin iteration: %v", err)
		}
		state.modelContents.totalBytes = math.MaxInt64
		if _, err := state.stageModelContent("session", 1, 1, 3, "call", testSafeText("x")); err == nil {
			t.Fatal("checked-add overflow unexpectedly succeeded")
		}
		count, total := state.modelContentSlotStats()
		if count != 0 || total != math.MaxInt64 || state.modelContents.nextOrdinal != 0 {
			t.Fatalf("overflow stage mutated state: count=%d total=%d ordinal=%d", count, total, state.modelContents.nextOrdinal)
		}
	})

	run("restart_uses_persisted_content_only", func(t *testing.T) {
		original, key := stagedModelContentFixture(t, "session", 1, 1, 3, "call", "model-preview-canary")
		assertModelContentLookup(t, original, key, "call", "model-preview-canary")
		restarted := configuredModelContentState(t, "session", 2, 64, 128)
		if err := restarted.beginModelContentIteration("session", 2, 1); err != nil {
			t.Fatalf("begin restarted iteration: %v", err)
		}
		restartKey := key
		restartKey.requestGeneration = 2
		assertModelContentMissing(t, restarted, restartKey, "call")
		assertModelContentSlotsEmpty(t, restarted)
	})

	actual := append([]string(nil), seen...)
	want := append([]string(nil), canonical...)
	sort.Strings(actual)
	sort.Strings(want)
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("slot lifecycle subtests = %v, want closed set %v", actual, want)
	}
}

func configuredModelContentState(t *testing.T, conversationID string, generation uint64, maxRecordBytes, maxSessionBytes int64) *executionState {
	t.Helper()
	state := &executionState{}
	if err := state.configureModelContentSlots(conversationID, generation, maxRecordBytes, maxSessionBytes); err != nil {
		t.Fatalf("configure model content slots: %v", err)
	}
	return state
}

func stagedModelContentFixture(t *testing.T, conversationID string, generation uint64, iteration, messageIndex int, callID, content string) (*executionState, modelContentSlotKey) {
	t.Helper()
	state := configuredModelContentState(t, conversationID, generation, 1024, 4096)
	if err := state.beginModelContentIteration(conversationID, generation, iteration); err != nil {
		t.Fatalf("begin model content iteration: %v", err)
	}
	key, err := state.stageModelContent(conversationID, generation, iteration, messageIndex, callID, testSafeText(content))
	if err != nil {
		t.Fatalf("stage model content: %v", err)
	}
	return state, key
}

func modelContentConversationFixture(conversationID, callID string) *conversation.Conversation {
	conv := conversation.NewConversation(conversationID, time.Unix(1_700_000_000, 0).UTC())
	conv.Messages = []conversation.Message{
		{Role: conversation.RoleUser, Content: testSafeText("request")},
		{Role: conversation.RoleAssistant, Content: testSafeText("assistant")},
		{Role: conversation.RoleToolCall, Content: testSafeText("Read"), Tool: &conversation.ToolState{CallID: callID, Name: "Read"}},
		{Role: conversation.RoleToolResult, Content: testSafeText("persisted-only"), Tool: &conversation.ToolState{CallID: callID, Name: "Read", Result: testSafeText("persisted-only")}},
	}
	return conv
}

func assertModelContentLookup(t *testing.T, state *executionState, key modelContentSlotKey, callID, want string) {
	t.Helper()
	content, ok, err := state.lookupModelContent(key, callID)
	if err != nil {
		t.Fatalf("lookup model content: %v", err)
	}
	if !ok || content.Text() != want {
		t.Fatalf("lookup model content = (%q,%t), want (%q,true)", content.Text(), ok, want)
	}
}

func assertModelContentMissing(t *testing.T, state *executionState, key modelContentSlotKey, callID string) {
	t.Helper()
	content, ok, err := state.lookupModelContent(key, callID)
	if err != nil {
		t.Fatalf("lookup missing model content: %v", err)
	}
	if ok || content.Text() != "" {
		t.Fatalf("missing model content = (%q,%t), want empty,false", content.Text(), ok)
	}
}

func assertModelContentSlotsEmpty(t *testing.T, state *executionState) {
	t.Helper()
	count, total := state.modelContentSlotStats()
	if count != 0 || total != 0 {
		t.Fatalf("model content slots not empty: count=%d total=%d", count, total)
	}
}
