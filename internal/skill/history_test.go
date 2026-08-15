package skill

import (
	"context"
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/redact"
)

var historyTestRedactor = redact.NewRuntimeRedactor()

func historySafeText(value string) redact.SafeText { return historyTestRedactor.Redact(value) }

func TestCompleteTurnScannerKeepsToolChainAtomic(t *testing.T) {
	messages := []conversation.Message{
		{Role: conversation.RoleUser, Content: historySafeText("older user")},
		{Role: conversation.RoleAssistant, Content: historySafeText("calling")},
		{Role: conversation.RoleToolCall, Tool: &conversation.ToolState{CallID: "call-1", Name: "Read"}},
		{Role: conversation.RoleToolResult, Tool: &conversation.ToolState{CallID: "call-1", Name: "Read", Result: historySafeText("safe result")}},
		{Role: conversation.RoleAssistant, Content: historySafeText("older done")},
		{Role: conversation.RoleUser, Content: historySafeText("newer user")},
		{Role: conversation.RoleAssistant, Content: historySafeText("calling")},
		{Role: conversation.RoleToolCall, Tool: &conversation.ToolState{CallID: "call-2", Name: "Bash"}},
	}
	scanner := NewCompleteTurnScanner(messages)
	turnRange, ok, err := scanner.Next(context.Background(), messages)
	if err != nil || !ok {
		t.Fatalf("scan complete older turn: range=%#v ok=%v err=%v", turnRange, ok, err)
	}
	if turnRange.Start() != 0 || turnRange.End() != 5 {
		t.Fatalf("tool turn range = [%d:%d], want [0:5]", turnRange.Start(), turnRange.End())
	}
	turn, err := turnRange.Messages(messages)
	if err != nil || len(turn) != 5 || turn[2].Role != conversation.RoleToolCall || turn[3].Role != conversation.RoleToolResult {
		t.Fatalf("tool chain was not returned atomically: turn=%#v err=%v", turn, err)
	}
	if _, ok, err := scanner.Next(context.Background(), messages); err != nil || ok {
		t.Fatalf("unexpected additional turn: ok=%v err=%v", ok, err)
	}

	mismatched := []conversation.Message{
		{Role: conversation.RoleUser},
		{Role: conversation.RoleToolCall, Tool: &conversation.ToolState{CallID: "call-a", Name: "Read"}},
		{Role: conversation.RoleToolResult, Tool: &conversation.ToolState{CallID: "call-b", Name: "Read"}},
		{Role: conversation.RoleAssistant},
	}
	mismatchScanner := NewCompleteTurnScanner(mismatched)
	if _, ok, err := mismatchScanner.Next(context.Background(), mismatched); err != nil || ok {
		t.Fatalf("mismatched tool chain was accepted: ok=%v err=%v", ok, err)
	}
}

func TestCompleteTurnScannerRejectsIncompleteEdges(t *testing.T) {
	messages := []conversation.Message{
		{Role: conversation.RoleAssistant, Content: historySafeText("orphan assistant")},
		{Role: conversation.RoleToolResult, Tool: &conversation.ToolState{CallID: "orphan", Name: "Read"}},
		{Role: conversation.RoleUser, Content: historySafeText("complete user")},
		{Role: conversation.RoleAssistant, Content: historySafeText("complete answer")},
		{Role: conversation.RoleUser, Content: historySafeText("trailing user")},
		{Role: conversation.RoleAssistant, Content: historySafeText("calling")},
		{Role: conversation.RoleToolCall, Tool: &conversation.ToolState{CallID: "open", Name: "Read"}},
	}
	scanner := NewCompleteTurnScanner(messages)
	turnRange, ok, err := scanner.Next(context.Background(), messages)
	if err != nil || !ok || turnRange.Start() != 2 || turnRange.End() != 4 {
		t.Fatalf("middle complete turn = %#v ok=%v err=%v, want [2:4]", turnRange, ok, err)
	}
	if _, ok, err := scanner.Next(context.Background(), messages); err != nil || ok {
		t.Fatalf("incomplete leading edge was returned: ok=%v err=%v", ok, err)
	}
	nilContextScanner := NewCompleteTurnScanner(messages)
	if _, ok, err := nilContextScanner.Next(nil, messages); err == nil || ok {
		t.Fatalf("nil context did not fail closed: ok=%v err=%v", ok, err)
	}
}

func TestCompleteTurnScannerWalksNewestWithoutFullConversationClone(t *testing.T) {
	scannerType := reflect.TypeOf(CompleteTurnScanner{})
	for index := 0; index < scannerType.NumField(); index++ {
		if scannerType.Field(index).Type.Kind() != reflect.Int {
			t.Fatalf("scanner field %s retains non-index state %v", scannerType.Field(index).Name, scannerType.Field(index).Type)
		}
	}
	rangeType := reflect.TypeOf(CompleteTurnRange{})
	for index := 0; index < rangeType.NumField(); index++ {
		if rangeType.Field(index).Type.Kind() != reflect.Int {
			t.Fatalf("range field %s retains non-index state %v", rangeType.Field(index).Name, rangeType.Field(index).Type)
		}
	}
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate history test source")
	}
	productionSource, err := os.ReadFile(strings.TrimSuffix(testFile, "_test.go") + ".go")
	if err != nil {
		t.Fatalf("read history production source: %v", err)
	}
	if strings.Contains(string(productionSource), "ContextMessages(") {
		t.Fatal("history scanner production path copies the full conversation through ContextMessages")
	}

	messages := []conversation.Message{
		{Role: conversation.RoleUser, Content: historySafeText("older")},
		{Role: conversation.RoleAssistant, Content: historySafeText("older done")},
		{Role: conversation.RoleUser, Content: historySafeText("newer")},
		{Role: conversation.RoleAssistant, Content: historySafeText("newer done")},
	}
	scanner := NewCompleteTurnScanner(messages)
	newest, ok, err := scanner.Next(context.Background(), messages)
	if err != nil || !ok || newest.Start() != 2 || newest.End() != 4 {
		t.Fatalf("newest range = %#v ok=%v err=%v", newest, ok, err)
	}
	older, ok, err := scanner.Next(context.Background(), messages)
	if err != nil || !ok || older.Start() != 0 || older.End() != 2 {
		t.Fatalf("older range = %#v ok=%v err=%v", older, ok, err)
	}

	aliasProbe := []conversation.Message{{Role: conversation.RoleUser}, {Role: conversation.RoleAssistant}}
	aliasScanner := NewCompleteTurnScanner(aliasProbe)
	aliasProbe[1].Role = conversation.RoleThinking
	if _, ok, err := aliasScanner.Next(context.Background(), aliasProbe); err != nil || ok {
		t.Fatalf("scanner used a cloned role snapshot: ok=%v err=%v", ok, err)
	}

	longRange := make([]conversation.Message, 0, completeTurnContextCheckInterval*3+2)
	longRange = append(longRange, conversation.Message{Role: conversation.RoleUser})
	for index := 0; index < completeTurnContextCheckInterval*3; index++ {
		longRange = append(longRange, conversation.Message{Role: conversation.RoleThinking})
	}
	longRange = append(longRange, conversation.Message{Role: conversation.RoleAssistant})
	canceling := &historyScanCancelContext{cancelAfterChecks: 4}
	longScanner := NewCompleteTurnScanner(longRange)
	if turnRange, ok, err := longScanner.Next(canceling, longRange); !errors.Is(err, context.Canceled) || ok || turnRange != (CompleteTurnRange{}) {
		t.Fatalf("long range cancellation published partial state: range=%#v ok=%v err=%v", turnRange, ok, err)
	}
	if turnRange, ok, err := longScanner.Next(context.Background(), longRange); err != nil || !ok || turnRange.Start() != 0 || turnRange.End() != len(longRange) {
		t.Fatalf("cancelled scanner did not preserve its cursor: range=%#v ok=%v err=%v", turnRange, ok, err)
	}
}

type historyScanCancelContext struct {
	checks            int
	cancelAfterChecks int
}

func (c *historyScanCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *historyScanCancelContext) Done() <-chan struct{}       { return nil }
func (c *historyScanCancelContext) Value(any) any               { return nil }

func (c *historyScanCancelContext) Err() error {
	c.checks++
	if c.checks >= c.cancelAfterChecks {
		return context.Canceled
	}
	return nil
}
