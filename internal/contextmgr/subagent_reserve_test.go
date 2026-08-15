package contextmgr

import (
	"context"
	"testing"

	"xagent/internal/config"
	"xagent/internal/conversation"
)

func TestSubagentResultReserveMovesOnlyAutomaticCompactionThreshold(t *testing.T) {
	manager := &Manager{cfg: config.ContextConfig{
		ModelWindowTokens:  100,
		AutoMarginTokens:   10,
		ManualMarginTokens: 5,
		RecentKeepMessages: 1,
	}}
	conv := &conversation.Conversation{}
	if manager.shouldSummarize(conv, 79, ModeAuto, 0) {
		t.Fatal("automatic compaction triggered without the result reserve")
	}
	if !manager.shouldSummarize(conv, 79, ModeAuto, 11) {
		t.Fatal("result reserve did not move the automatic compaction threshold")
	}
	if manager.shouldSummarize(conv, 99, ModeManual, 99) {
		t.Fatal("result reserve changed manual compaction semantics")
	}
}

func TestPrepareRejectsInvalidSubagentResultReserve(t *testing.T) {
	enabled := true
	manager := &Manager{cfg: config.ContextConfig{
		Enabled:             &enabled,
		ModelWindowTokens:   100,
		AutoMarginTokens:    10,
		ManualMarginTokens:  5,
		RecentKeepTokens:    5,
		RecentKeepMessages:  1,
		SummaryFailureLimit: 1,
	}}
	for _, reserve := range []int64{-1, 100, 101} {
		if _, err := manager.PrepareWithOptions(context.Background(), &conversation.Conversation{}, PrepareOptions{
			Mode: ModeAuto, ReservePlanningTokens: reserve,
		}); err == nil {
			t.Fatalf("invalid result reserve %d was accepted", reserve)
		}
	}
}
